#!/usr/bin/env python3
"""Run the golden set through the doctor against a model endpoint and score every diagnosis.

    python3 -m evals.run_golden --profile qwen3-8b-awq --topology sliced --tag baseline --repeat 3
    python3 -m evals.run_golden --base-url http://127.0.0.1:8080/v1 --profile qwen3-8b-awq --tag gw-baseline
    python3 -m evals.run_golden ... --only dx-oom,dx-audit-1 --concurrency 8 --no-harness

The cluster data comes from the recorded snapshots (deterministic); only the model is live. Writes
metrics/golden-<tag>-<ts>.jsonl (one row per task) and .summary.json. Every row is scored twice (D-41):
`pass` is the legacy v1 score (kept for continuity), `pass_v2` the corrected one (observed evidence,
accepted equivalent categories, mechanism facts, advisory tool rules). The summary carries both, a 95 %
interval on the v2 rate, per-tier rates, sub-scores, stop reasons, tokens, cached share, latency, refusals
by HTTP status, and the model profile, topology and workers the run used. `make matrix` reads it (D-40).
With --concurrency > 1 the same harness is the app-shaped load generator for the gateway.
"""
from __future__ import annotations

import argparse
import json
import math
import statistics
import subprocess
import sys
import threading
import time
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from agent.agent import build_llm, load_profile, run_task  # noqa: E402
from evals.build_golden import backend_for  # noqa: E402
from evals.checker import PARTS, check, check_v2  # noqa: E402
from serving.profiles import load_serving  # noqa: E402

TASKS = ROOT / "evals" / "golden" / "tasks.jsonl"


def load_tasks(only: str | None) -> list[dict]:
    tasks = [json.loads(line) for line in TASKS.read_text().splitlines() if line.strip()]
    return [t for t in tasks if not only or t["id"] in only.split(",")]


def score(task: dict, run: dict) -> dict:
    b = backend_for(task["snapshots"])
    v = check(task, run["diagnosis"], run["trace"], b)
    steps = run["steps"]
    v2 = check_v2(task, run["diagnosis"], b, observed=run.get("observed"), calls=[c for s in steps for c in s.get("calls", [])])
    return {"id": task["id"], "task_type": task["task_type"], "tier": task.get("tier", "easy"), "pass": v["pass"], "failed": v["failed"],
            "pass_v2": v2["pass"], "failed_v2": v2["failed"], "advisory": v2["advisory"], "parts": v2["parts"],
            "observed_refs": run.get("observed_refs"), "stop": run["stop"],
            "n_steps": len(steps), "trace": run["trace"], "repairs": run["repairs"], "harness": run["harness"],
            "http_status": [s.get("http_status") for s in steps if s.get("http_status")],
            "refusals": [r for s in steps for r in s.get("refusals", [])],          # every gateway 429/503, retried or not
            "tool_errors": sum(1 for s in steps for c in s.get("calls", []) if c.get("error")),
            "prompt_tokens": [s.get("prompt_tokens") for s in steps], "completion_tokens": [s.get("completion_tokens") for s in steps],
            "cached_tokens": [s.get("cached_tokens") for s in steps], "latency_s": [s.get("latency_s") for s in steps],
            "headers": run["headers"], "diagnosis": run["diagnosis"],
            "finish_reasons": [s.get("finish_reason") for s in steps],
            "calls": [{k: c[k] for k in ("step", "name", "args", "error", "validation_errors") if k in c}      # replayable: the snapshot
                      for s in steps for c in s.get("calls", [])]}                                          # backend is deterministic


def error_row(task: dict, e: Exception) -> dict:
    """One task crashed: record it as a failure with the reason, keep the run going."""
    why = [f"exception: {type(e).__name__}: {str(e)[:200]}"]
    return {"id": task["id"], "task_type": task["task_type"], "tier": task.get("tier", "easy"), "pass": False, "failed": why,
            "pass_v2": False, "failed_v2": why, "advisory": [], "parts": dict.fromkeys(PARTS, False), "observed_refs": 0,
            "stop": f"error_{type(e).__name__}", "n_steps": 0,
            "trace": [], "repairs": 0, "harness": True, "http_status": [], "refusals": [], "tool_errors": 0, "prompt_tokens": [],
            "completion_tokens": [], "cached_tokens": [], "latency_s": [], "headers": {}, "diagnosis": None, "finish_reasons": [], "calls": []}


def result_paths(out: Path, tag: str, stamp: str) -> tuple[Path, Path]:
    """The run's rows and summary files. The suffix is appended, never swapped: a tag such as
    `sweep-qwen3.8-27b-fp8-c16` has a dot, and Path.with_suffix would cut it there, sending every level's rows to one
    `golden-sweep-qwen3.jsonl` (it did, on 10-06 and 10-07)."""
    base = out / f"golden-{tag}-{stamp}"
    return Path(f"{base}.jsonl"), Path(f"{base}.summary.json")


def _pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    return round(xs[min(len(xs) - 1, int(p * len(xs)))], 3) if xs else None


def wilson(k: int, n: int, z: float = 1.96) -> list[float] | None:
    """95 % Wilson interval for k passes out of n, which stays honest at small n (26 tasks × a few repeats)."""
    if not n:
        return None
    p = k / n
    mid, half = (p + z * z / (2 * n)) / (1 + z * z / n), z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / (1 + z * z / n)
    return [round(max(0.0, mid - half), 3), round(min(1.0, mid + half), 3)]


def _rates(rows: list[dict], key: str, field: str) -> dict:
    groups: dict[str, list[bool]] = {}
    for r in rows:
        groups.setdefault(r[key], []).append(r[field])
    return {k: round(sum(v) / len(v), 3) for k, v in sorted(groups.items())}


def summarise(rows: list[dict], meta: dict) -> dict:
    flat = lambda k: [x for r in rows for x in r[k] if x is not None]  # noqa: E731
    n, k2 = len(rows), sum(r.get("pass_v2", False) for r in rows)
    parts = {p: round(sum(r.get("parts", {}).get(p, False) for r in rows) / max(1, n), 3) for p in PARTS}
    return {**meta, "tasks": n, "unique_tasks": len({r["id"] for r in rows}), "passed": sum(r["pass"] for r in rows),
            "pass_rate": round(sum(r["pass"] for r in rows) / max(1, n), 3),
            "pass_rate_by_type": _rates(rows, "task_type", "pass"),
            "pass_rate_by_tier": _rates(rows, "tier", "pass"),
            "passed_v2": k2, "pass_rate_v2": round(k2 / max(1, n), 3), "pass_rate_v2_ci95": wilson(k2, n),
            "pass_rate_by_type_v2": _rates(rows, "task_type", "pass_v2"),
            "pass_rate_by_tier_v2": _rates(rows, "tier", "pass_v2"),
            "parts_v2": parts,
            "top_failed_rules_v2": Counter(f.split(":")[0] for r in rows for f in r.get("failed_v2", [])).most_common(10),
            "advisory_v2": Counter(f.split(":")[0] for r in rows for f in r.get("advisory", [])).most_common(5),
            "stop_reasons": dict(Counter(r["stop"] for r in rows)),
            "inconclusive": sum(r["stop"] == "inconclusive" for r in rows),
            "abstained": sum(r["stop"] == "abstained" for r in rows),
            "http_refusals": dict(Counter(c for r in rows for c in r["http_status"])),       # refusals that ended a run
            "refusal_reasons": dict(Counter(f"{x['http_status']} {x['reason']}" for r in rows for x in r.get("refusals", []))),
            "runs_that_waited_out_a_refusal": sum(1 for r in rows if r.get("refusals") and not r["http_status"]),
            "top_failed_rules": Counter(f.split(":")[0] for r in rows for f in r["failed"]).most_common(10),
            "repairs": sum(r["repairs"] for r in rows), "tool_errors": sum(r["tool_errors"] for r in rows),
            "steps_per_task_mean": round(statistics.mean(r["n_steps"] for r in rows), 2) if rows else None,
            "prompt_tokens_last_step_p50": _pct([r["prompt_tokens"][-1] for r in rows if r["prompt_tokens"]], 0.5),
            "prompt_tokens_last_step_max": max((r["prompt_tokens"][-1] or 0 for r in rows if r["prompt_tokens"]), default=None),
            "completion_tokens_per_step_p50": _pct(flat("completion_tokens"), 0.5),
            "cached_share_of_prompt": (round(sum(flat("cached_tokens")) / sum(flat("prompt_tokens")), 3)
                                       if flat("cached_tokens") and sum(flat("prompt_tokens")) else None),
            "step_latency_s_p50": _pct(flat("latency_s"), 0.5), "step_latency_s_p95": _pct(flat("latency_s"), 0.95)}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base-url", default="http://127.0.0.1:8000/v1")
    ap.add_argument("--profile", default="qwen3-8b-awq", help="model profile in deploy/models/ (served name + sampling)")
    ap.add_argument("--model", help="served model name override (default: the profile's)")
    ap.add_argument("--topology", default="sliced", help="how the GPU was carved for this run (deploy/serving.json)")
    ap.add_argument("--workers", type=int, default=1, help="vLLM replicas serving during the run")
    ap.add_argument("--only")
    ap.add_argument("--concurrency", type=int, default=1)
    ap.add_argument("--repeat", type=int, default=1, help="run the task list N times (load generation)")
    ap.add_argument("--tag", default="run")
    ap.add_argument("--no-harness", action="store_true")
    ap.add_argument("--out", default=str(ROOT / "metrics"))
    a = ap.parse_args()

    tasks = load_tasks(a.only) * max(1, a.repeat)
    served, client = load_profile(a.profile)
    a.model = a.model or served
    topo = load_serving()["topologies"][a.topology]
    llm = build_llm(a.base_url, a.model, client=client)
    t0 = time.time()
    stamp = time.strftime("%Y%m%d-%H%M%S")
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    rows_path, summary_path = result_paths(out, a.tag, stamp)
    lock = threading.Lock()

    def one(task: dict) -> dict:
        try:
            row = score(task, run_task(task, backend_for(task["snapshots"]), llm, harness=not a.no_harness))
        except Exception as e:  # one task must never lose the run
            row = error_row(task, e)
        with lock:                                  # written as each task finishes, so an interrupted run keeps its rows
            with rows_path.open("a") as f:
                f.write(json.dumps(row, ensure_ascii=False) + "\n")
            print(f"{row['id']:22} {row['tier']:11} v1 {'PASS' if row['pass'] else 'FAIL'} v2 {'PASS' if row['pass_v2'] else 'FAIL'} "
                  f"steps={row['n_steps']:2} stop={row['stop']:13} {'; '.join(row['failed_v2'])[:100]}", flush=True)
        return row

    with ThreadPoolExecutor(max_workers=max(1, a.concurrency)) as ex:
        rows = list(ex.map(one, tasks))
    try:
        commit = subprocess.run(["git", "-C", str(ROOT), "rev-parse", "--short", "HEAD"], capture_output=True, text=True, check=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        commit = None
    summary = summarise(rows, {"tag": a.tag, "profile": a.profile, "model": a.model, "topology": a.topology, "workers": a.workers,
                               "gpu_share": round(a.workers * topo["gpucores"] / 100, 2), "base_url": a.base_url,
                               "concurrency": a.concurrency, "repeat": a.repeat, "harness": not a.no_harness, "scorers": ["v1", "v2"],
                               "only": a.only, "git_commit": commit, "wall_s": round(time.time() - t0, 1), "timestamp": stamp})
    summary_path.write_text(json.dumps(summary, indent=2))
    print(json.dumps({k: summary[k] for k in ("pass_rate", "pass_rate_v2", "pass_rate_v2_ci95", "pass_rate_by_tier_v2", "parts_v2",
                                              "stop_reasons", "inconclusive", "abstained", "http_refusals", "refusal_reasons",
                                              "runs_that_waited_out_a_refusal", "top_failed_rules_v2")}, indent=2))
    print(f"wrote {rows_path} and {summary_path.name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
