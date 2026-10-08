"""Ariadne on the command line: diagnose a live cluster (read-only) or a recorded fault snapshot.

    uv run python -m agent investigate -n inventory "stock-api keeps restarting"
    uv run python -m agent audit -n orders,pricing,finance --context lambda
    uv run python -m agent rightsize -n analytics
    uv run python -m agent investigate -n orders --snapshot crashloop       # recorded fault, no cluster needed
    uv run python -m agent watch --context lambda                            # autonomous: detect, then diagnose (agent/watch.py)

The model comes from --base-url / DOCTOR_BASE_URL (default http://127.0.0.1:8000/v1, the `make tunnel` port) and
--model / DOCTOR_MODEL; the API key comes only from VLLM_API_KEY. Live reads go through kubectl with
read-only verbs (get, logs, top, version) using --context / --kubeconfig; Prometheus, OpenCost, S3 and
Cost Explorer are optional (DOCTOR_PROMETHEUS_URL, DOCTOR_OPENCOST_URL, DOCTOR_S3_BUCKET, DOCTOR_CCEXPLORER).
To reach Lambda's k3s from a laptop, run `make kubeconfig k8s-tunnel`, then pass --kubeconfig .cache/lambda-kubeconfig
--context lambda.

Progress goes to stderr; the result goes to stdout (a report, or --json for the full run record).
Exit status: 0 healthy · 1 issue found · 2 no grounded diagnosis (inconclusive, step cap, context budget,
gateway refusal, model or cluster unreachable) · 64 bad usage.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path

from .agent import build_llm, resolve_model, run_task
from .backends import KubectlBackend, SnapshotBackend, check_name

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures"
EXIT_HEALTHY, EXIT_ISSUE, EXIT_NO_DIAGNOSIS, EXIT_USAGE = 0, 1, 2, 64

MODES = {   # task_type: (default report, max model calls)
    "investigate": ("Something is wrong in these namespaces. Find the root cause.", 16),
    "audit": ("Audit: check these namespaces and report every root cause, or confirm they are healthy.", 30),
    "rightsize": ("Which workloads are over-provisioned, and what should their requests be? Don't break anything.", 12),
}
STOPS = {
    "inconclusive": "the model's answer failed the grounding checks after 2 repairs; escalate to a human",
    "abstained": "the model could not ground a diagnosis and said so (see its summary); escalate to a human",
    "step_cap": "ran out of model calls before submitting",
    "context_budget": "the conversation reached the context budget before submitting",
    "transport_error": "could not reach the model endpoint (is `make tunnel` running?)",
    "http_429": "refused by the gateway: tenant over its rate",
    "http_503": "refused by the gateway: serving is saturated, retry later",
}


class Style:
    def __init__(self, on: bool):
        self.on = on

    def __call__(self, text: str, code: str) -> str:
        return f"\033[{code}m{text}\033[0m" if self.on else text


def _color(stream) -> bool:
    return stream.isatty() and not os.environ.get("NO_COLOR")


# ---- progress (stderr) ------------------------------------------------------------------------

def _args(args: dict) -> str:
    return ", ".join(f"{k}={v}" for k, v in args.items() if v not in ("", None, False))


def progress_printer(err, st: Style):
    """Returns the run_task hook: one line per model call (latency, tokens) and one per tool call."""
    def on_update(node: str, update: dict) -> None:
        for s in update.get("steps", []):
            if s.get("http_status") or s.get("error"):
                print(st(f"  s{s['step']:<2} model call failed: {s.get('http_status') or s.get('error')}", "31"), file=err)
                continue
            cached = s.get("cached_tokens")
            print(st(f"  s{s['step']:<2} model {s['latency_s']:.1f}s · prompt {s.get('prompt_tokens') or 0:,}"
                     + (f" (cached {cached:,})" if cached is not None else "") + f" · out {s.get('completion_tokens') or 0:,}", "2"),
                  file=err)
        for c in update.get("calls", []):
            if c["name"] == "submit_diagnosis":
                line = ("       submit_diagnosis → rejected: " + "; ".join(c.get("validation_errors", []))[:160]
                        if c["rejected"] else "       submit_diagnosis → accepted")
                print(st(line, "33" if c["rejected"] else "32"), file=err)
            elif c["name"] is None:
                print(st("       (no tool call, nudged)", "33"), file=err)
            else:
                line = f"       {c['name']}({_args(c.get('args') or {})})"
                print(line + (st(f" → {c['error'][:120]}", "33") if c.get("error") else ""), file=err)
        if update.get("stop") and node == "limit":
            print(st(f"  stopped: {update['stop']}", "33"), file=err)
        err.flush()
    return on_update


# ---- report (stdout) --------------------------------------------------------------------------

def _obj(o: dict) -> str:
    return f"{o['kind']} {o['namespace']}/{o['name']}"


def render(run: dict, st: Style, wall_s: float, model: str) -> str:
    d, stop = run["diagnosis"], run["stop"]
    steps = run["steps"]
    out: list[str] = []
    if d is None or d.get("status") == "inconclusive":
        out.append(st("NO GROUNDED DIAGNOSIS", "1;31") + f" — {STOPS.get(stop, stop)}")
        if stop == "abstained":
            out.append(f"  {d['summary']}")
        for e in (d or {}).get("validation_errors", []):
            out.append(f"  · {e}")
    elif d["status"] == "healthy":
        out.append(st("HEALTHY", "1;32") + " — no root causes found")
        out.append(f"  {d['summary']}")
    else:
        n = len(d["findings"])
        out.append(st("ISSUE", "1;31") + f" — {n} root cause{'s' * (n != 1)}")
        out.append(f"  {d['summary']}")
        for i, f in enumerate(d["findings"], 1):
            out.append("")
            out.append(st(f"{i}. {f['category']}", "1") + f" · {_obj(f)} · confidence {f['confidence']}")
            out.append(f"   why       {f['root_cause']}")
            if f["affects"]:
                out.append("   affects   " + ", ".join(_obj(a) for a in f["affects"]))
            out.append("   evidence  " + ", ".join(f["evidence"]))
            out.append(f"   fix       {f['fix']}  " + st("(suggested; the doctor never changes the cluster)", "2"))
            rz = f.get("resize") or {}
            if rz.get("cpu_request") or rz.get("memory_request"):
                out.append(f"   resize    cpu {rz.get('cpu_request') or '—'} · memory {rz.get('memory_request') or '—'}")
    prompt = sum(s.get("prompt_tokens") or 0 for s in steps)
    cached = sum(s.get("cached_tokens") or 0 for s in steps)
    done = sum(s.get("completion_tokens") or 0 for s in steps)
    share = f" ({cached / prompt:.0%} cached)" if prompt and any(s.get("cached_tokens") is not None for s in steps) else ""
    out.append("")
    out.append(st(f"{len(steps)} model calls · {len(run['trace'])} tool calls · {run['repairs']} repairs · {wall_s:.1f}s · "
                  f"prompt {prompt:,} tokens{share} · output {done:,} · stop {stop} · {model} · run {run['run_id']}", "2"))
    return "\n".join(out)


def exit_code(run: dict) -> int:
    d = run["diagnosis"]
    if d is None or d.get("status") == "inconclusive":
        return EXIT_NO_DIAGNOSIS
    return EXIT_HEALTHY if d["status"] == "healthy" else EXIT_ISSUE


# ---- entry point ------------------------------------------------------------------------------

def parse(argv: list[str] | None) -> argparse.Namespace:
    ap = argparse.ArgumentParser(prog="python -m agent", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("mode", choices=sorted(MODES), help="investigate a report, audit namespaces, or find over-provisioned "
                    "workloads; `watch` (see `python -m agent watch --help`) runs autonomously")
    ap.add_argument("report", nargs="?", help="what the user sees (optional; each mode has a default)")
    ap.add_argument("-n", "--namespaces", required=True, help="comma-separated namespaces to examine")
    src = ap.add_mutually_exclusive_group()
    src.add_argument("--context", help="kubectl context of a live cluster (default: the current context)")
    src.add_argument("--snapshot", help=f"diagnose a recorded fault from {FIXTURES.name}/snapshots instead (e.g. crashloop)")
    ap.add_argument("--kubeconfig", help="kubeconfig file for the live cluster")
    ap.add_argument("--base-url", default=os.environ.get("DOCTOR_BASE_URL", "http://127.0.0.1:8000/v1"))
    ap.add_argument("--profile", default=os.environ.get("DOCTOR_PROFILE"),
                    help="model profile (deploy/models/<name>.json or its name): served model name and sampling (D-40)")
    ap.add_argument("--model", help="served model name (default: the profile's, DOCTOR_MODEL, or Qwen/Qwen3-8B-AWQ)")
    ap.add_argument("--tenant", default="platform", help="X-Tenant header for the gateway")
    ap.add_argument("--max-steps", type=int, help="model-call cap (default: 16 investigate, 30 audit, 12 rightsize)")
    ap.add_argument("--json", action="store_true", help="print the full run record as JSON instead of the report")
    ap.add_argument("--out", help="also write the full run record as JSON to this file")
    ap.add_argument("-q", "--quiet", action="store_true", help="no progress lines")
    return ap.parse_intermixed_args(argv)            # the report may come after the options on every Python version


class SourceError(Exception):
    def __init__(self, code: int, msg: str):
        super().__init__(msg)
        self.code = code


def open_source(context: str | None, snapshot: str | None, kubeconfig: str | None):
    """(backend, namespaces available, cluster name for the card, description) for a live context or recorded
    faults (comma-separated). Raises SourceError(EXIT_USAGE | EXIT_NO_DIAGNOSIS, message)."""
    if snapshot:
        root = (FIXTURES / "snapshots").resolve()
        paths = [FIXTURES / "snapshots" / f"{x.strip()}.json" for x in snapshot.split(",") if x.strip()]
        bad = [p.stem for p in paths if not p.is_file() or p.resolve().parent != root]
        if bad or not paths:
            names = ", ".join(sorted(p.stem for p in root.glob("*.json")))
            raise SourceError(EXIT_USAGE, f"no recorded snapshot {', '.join(bad) or snapshot!r}; available: {names}")
        backend = SnapshotBackend.load(FIXTURES / "cluster.json", *paths)
        return backend, sorted(backend.dump["namespaces"]), "doctor-lab", f"snapshot {snapshot}"   # the golden set's name, so the card hits the same cached prefix
    if kubeconfig:
        os.environ["KUBECONFIG"] = kubeconfig                                        # inherited by the kubectl subprocess
    backend = KubectlBackend(context=context)
    try:
        available = backend.namespaces()
    except (LookupError, OSError, subprocess.TimeoutExpired, ValueError) as e:
        raise SourceError(EXIT_NO_DIAGNOSIS, f"cannot read the cluster: {e}. For Lambda: make kubeconfig k8s-tunnel, then "
                                             "--kubeconfig .cache/lambda-kubeconfig --context lambda") from e
    return backend, available, context or "live", f"context {context or 'live'}"


def main(argv: list[str] | None = None, *, llm=None, stdout=None, stderr=None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    if argv[:1] == ["watch"]:                      # autonomous mode (D-37)
        from .watch import main as watch_main
        return watch_main(argv[1:], llm=llm, stdout=stdout, stderr=stderr)
    out, err = stdout or sys.stdout, stderr or sys.stderr
    try:
        a = parse(argv)
    except SystemExit as e:                        # argparse exits 0 for --help and 2 for bad usage, reported as 64
        return EXIT_USAGE if e.code else 0
    st_err, st_out = Style(_color(err)), Style(_color(out) and not a.json)

    def fail(code: int, msg: str) -> int:
        print(st_err(f"ariadne: {msg}", "31"), file=err)
        return code

    try:
        namespaces = [check_name(ns.strip(), "namespace") for ns in a.namespaces.split(",") if ns.strip()]
        check_name(a.tenant, "tenant")
    except ValueError as e:
        return fail(EXIT_USAGE, str(e))
    if not namespaces:
        return fail(EXIT_USAGE, "give at least one namespace with -n")
    try:
        a.model, client = resolve_model(a.model, a.profile)
    except (OSError, ValueError, KeyError) as e:
        return fail(EXIT_USAGE, f"cannot read model profile {a.profile!r}: {e}")

    try:
        backend, available, cluster, where = open_source(a.context, a.snapshot, a.kubeconfig)
    except SourceError as e:
        return fail(e.code, str(e))
    missing = [ns for ns in namespaces if ns not in available]
    if missing:                                   # kubectl returns an empty list for a typo, which must not read as "healthy"
        return fail(EXIT_USAGE, f"namespace not found: {', '.join(missing)}; available: {', '.join(available)}")

    report, max_steps = MODES[a.mode]
    task = {"id": f"cli-{a.mode}", "task_type": a.mode, "namespaces": namespaces, "report": a.report or report,
            "max_steps": a.max_steps or max_steps, "tenant": a.tenant}
    if not a.quiet:
        print(st_err(f"ariadne · {a.mode} · {', '.join(namespaces)} · {where} · {a.model} at {a.base_url}", "1"), file=err)
    t0 = time.perf_counter()
    try:
        run = run_task(task, backend, llm or build_llm(a.base_url, a.model, client=client), cluster=cluster,
                       on_update=None if a.quiet else progress_printer(err, st_err))
    except (LookupError, OSError, subprocess.TimeoutExpired) as e:                 # the cluster card could not be read
        return fail(EXIT_NO_DIAGNOSIS, f"cannot read the cluster: {e}")
    wall = time.perf_counter() - t0
    if not a.quiet:
        print(file=err)
    record = {**run, "mode": a.mode, "namespaces": namespaces, "source": where, "model": a.model, "wall_s": round(wall, 2)}
    if a.out:
        Path(a.out).write_text(json.dumps(record, indent=2, ensure_ascii=False))
    print(json.dumps(record, indent=2, ensure_ascii=False) if a.json else render(run, st_out, wall, a.model), file=out)
    return exit_code(run)
