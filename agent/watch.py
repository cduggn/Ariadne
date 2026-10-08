"""Autonomous mode: detect cheaply, diagnose only what changed (D-37).

    uv run python -m agent watch --context lambda --out metrics/watch.jsonl
    uv run python -m agent watch --snapshot crashloop,cascade-db --once          # recorded faults, one cycle

Every --interval seconds (default 60):
  scan      no model: per namespace, problem pods (by owner), Deployments with unavailable replicas, failed
            Jobs, Services with no ready endpoints, recent Warning events on non-Pod objects (e.g. a ReplicaSet refused by a quota), and running
            pods whose last 30 log lines hold ≥ 3 error-looking lines (a count only; --no-log-scan to skip).
  filter    fingerprint = namespace|Kind/name. A namespace is diagnosed when it has a fingerprint that was not
            there at its last diagnosis and its --cooldown (900 s) has passed. Resolved fingerprints are
            forgotten, so a relapse triggers again. A gateway refusal or transport error is retried next scan.
  diagnose  an investigation per changed namespace (X-Priority interactive), up to --max-parallel at once. The
            scan builds the report only from structured fields (kind, name, sanitised reason, restarts), never
            from free text in the cluster, because the report is a user turn.
  schedule  audits of every watched namespace in groups of 3 every --audit-every (24 h) and right-sizing per
            namespace every --rightsize-every (7 d), both X-Priority batch; 0 turns one off.
  emit      one JSON line per diagnosis (stdout, and appended to --out); human lines on stderr; Prometheus
            text at http://--metrics-addr/metrics (default 127.0.0.1:9109; empty to disable).
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import subprocess
import sys
import threading
import time
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from . import tools as T
from .agent import build_llm, resolve_model, run_task
from .backends import Backend, check_name

SYSTEM_NAMESPACES = {"kube-system", "kube-public", "kube-node-lease", "local-path-storage"}
AUDIT_GROUP = 3            # 4 namespaces reached the context budget on the GPU (dx-audit-2, D-39)
REPORTS = {
    "audit": "Scheduled audit: check these namespaces and report every root cause, or confirm they are healthy.",
    "rightsize": "Scheduled right-sizing: which workloads are over-provisioned, and what should their requests be? Don't break anything.",
}
MAX_STEPS = {"investigate": 16, "audit": 30, "rightsize": 12}
RETRY_STOPS = ("http_", "transport_error")


# ---- metrics ----------------------------------------------------------------------------------

class Metrics:
    """Counters and gauges in Prometheus text format; thread-safe; no dependency."""

    HELP = {
        "doctor_scans_total": ("counter", "Scan cycles completed."),
        "doctor_scan_errors_total": ("counter", "Namespaces that could not be read during a scan."),
        "doctor_scan_seconds": ("gauge", "Duration of the last scan."),
        "doctor_last_scan_timestamp_seconds": ("gauge", "Unix time of the last completed scan."),
        "doctor_watched_namespaces": ("gauge", "Namespaces being watched."),
        "doctor_open_problems": ("gauge", "Problem fingerprints currently present, per namespace."),
        "doctor_detections_total": ("counter", "New problem fingerprints seen, per namespace."),
        "doctor_skipped_total": ("counter", "Changed namespaces not diagnosed yet, by reason."),
        "doctor_diagnoses_total": ("counter", "Diagnoses finished, by mode, status and stop reason."),
        "doctor_findings_total": ("counter", "Root causes reported, by category."),
        "doctor_diagnosis_seconds_sum": ("counter", "Total diagnosis wall time, by mode."),
        "doctor_diagnosis_seconds_count": ("counter", "Diagnoses timed, by mode."),
        "doctor_prompt_tokens_total": ("counter", "Prompt tokens sent to the model, by mode."),
        "doctor_cached_tokens_total": ("counter", "Prompt tokens served from the prefix cache, by mode."),
        "doctor_completion_tokens_total": ("counter", "Tokens generated, by mode."),
        "doctor_model_calls_total": ("counter", "Model calls, by mode."),
    }

    def __init__(self):
        self._v: dict[tuple[str, tuple], float] = {}
        self._lock = threading.Lock()

    def inc(self, name: str, n: float = 1, **labels) -> None:
        with self._lock:
            k = (name, tuple(sorted(labels.items())))
            self._v[k] = self._v.get(k, 0) + n

    def set(self, name: str, v: float, **labels) -> None:
        with self._lock:
            self._v[(name, tuple(sorted(labels.items())))] = v

    def get(self, name: str, **labels) -> float:
        return self._v.get((name, tuple(sorted(labels.items()))), 0)

    def render(self) -> str:
        with self._lock:
            items = sorted(self._v.items())
        out, seen = [], set()
        for (name, labels), v in items:
            if name not in seen:
                kind, text = self.HELP.get(name, ("gauge", name))
                base = name.removesuffix("_sum").removesuffix("_count") if name.startswith("doctor_diagnosis_seconds") else name
                if base not in seen:
                    out += [f"# HELP {base} {text}", f"# TYPE {base} {'summary' if base != name else kind}"]
                    seen.add(base)
                seen.add(name)
            lab = ",".join(f'{k}="{str(val).replace(chr(92), "").replace(chr(34), "")}"' for k, val in labels)
            out.append(f"{name}{{{lab}}} {v:g}" if lab else f"{name} {v:g}")
        return "\n".join(out) + "\n"


def serve_metrics(metrics: Metrics, addr: str) -> ThreadingHTTPServer:
    host, _, port = addr.rpartition(":")

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            if self.path.split("?")[0] != "/metrics":
                self.send_error(404)
                return
            body = metrics.render().encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer((host or "127.0.0.1", int(port)), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


# ---- scan -------------------------------------------------------------------------------------

_REASON = re.compile(r"^(Init:|Restarted after )?[A-Z][A-Za-z]{1,39}$")      # Kubernetes reasons are CamelCase tokens


def _reason(text: str) -> str:
    """Only a CamelCase reason token reaches the report (a user turn); anything else, such as free text a workload or
    controller could have written, becomes "Other". Tool results carry the details, as untrusted data."""
    text = (text or "").strip()
    return text if _REASON.match(text) else ("NotReady" if not text else "Other")


def _ts(s: str | None) -> dt.datetime | None:
    try:
        return dt.datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.UTC) if s else None
    except ValueError:
        return None


LOG_ERRORS = re.compile(r"\b(error|errors|failed|failure|refused|timed out|timeout|x509|certificate|unreachable|"
                        r"no such host|bad address|exception|fatal|panic|denied)\b", re.IGNORECASE)
LOG_LINES, LOG_THRESHOLD = 30, 3


def log_errors(b: Backend, ns: str, pod: dict) -> int:
    """Error-looking lines in the last LOG_LINES of each container of a running pod (a count, never the text)."""
    n = 0
    for c in pod["spec"]["containers"]:
        try:
            lines = b.logs(ns, pod["metadata"]["name"], c["name"], False).splitlines()[-LOG_LINES:]
        except (LookupError, OSError, subprocess.TimeoutExpired):
            continue
        n += sum(1 for line in lines if LOG_ERRORS.search(line))
    return n


def scan_namespace(b: Backend, ns: str, event_window_s: int, logs: bool = True) -> dict[str, str]:
    """{fingerprint: one-line structured symptom} for one namespace. No model; only structured fields and counts."""
    found: dict[str, str] = {}
    rss = b.objects("replicasets", ns)
    for row in T.list_problem_pods(b, ns)["problem_pods"]:
        owner = row.get("owner") or {"kind": "Pod", "name": row["pod"]}
        fp = f"{ns}|{owner['kind']}/{owner['name']}"
        found.setdefault(fp, f"{owner['kind']}/{owner['name']}: {_reason(row['reason'])}, ready {row['ready']}, restarts {row['restarts']}")
    if logs:        # failures that only show in logs: a client of an expired certificate, a wrong CA, a dead DNS server
        for p in b.objects("pods", ns):
            if p.get("status", {}).get("phase") != "Running":
                continue
            owner = T.owner_of(p, rss) or {"kind": "Pod", "name": p["metadata"]["name"]}
            fp = f"{ns}|{owner['kind']}/{owner['name']}"
            if fp not in found and (n := log_errors(b, ns, p)) >= LOG_THRESHOLD:
                found[fp] = f"{owner['kind']}/{owner['name']}: LogErrors, {n} of the last {LOG_LINES} log lines look like errors"
    for d in b.objects("deployments", ns):
        want, ready = d.get("spec", {}).get("replicas", 1), d.get("status", {}).get("readyReplicas", 0)
        if want and ready < want:
            found.setdefault(f"{ns}|Deployment/{d['metadata']['name']}", f"Deployment/{d['metadata']['name']}: {ready}/{want} replicas ready")
    slices = b.objects("endpointslices", ns)
    for svc in b.objects("services", ns):          # a selector that matches no ready pod: callers get connection errors
        name = svc["metadata"]["name"]
        if not svc.get("spec", {}).get("selector"):
            continue
        ready = [ep for sl in slices if sl.get("metadata", {}).get("labels", {}).get("kubernetes.io/service-name") == name
                 for ep in sl.get("endpoints") or [] if (ep.get("conditions") or {}).get("ready")]
        if not ready:
            found.setdefault(f"{ns}|Service/{name}", f"Service/{name}: NoReadyEndpoints")
    for j in b.objects("jobs", ns):
        st = j.get("status", {})
        if st.get("failed") and not st.get("succeeded"):
            found.setdefault(f"{ns}|Job/{j['metadata']['name']}", f"Job/{j['metadata']['name']}: {st['failed']} failed pods")
    now = b.now()
    for e in b.objects("events", ns):
        io = e.get("involvedObject", {})
        seen = _ts(e.get("lastTimestamp") or e.get("eventTime"))
        if e.get("type") != "Warning" or io.get("kind") in (None, "Pod") or (seen and (now - seen).total_seconds() > event_window_s):
            continue
        found.setdefault(f"{ns}|{io['kind']}/{io.get('name', '')}", f"{io['kind']}/{io.get('name', '')}: Warning {_reason(e.get('reason', ''))}")
    return found


# ---- the watcher ------------------------------------------------------------------------------

class Watcher:
    def __init__(self, backend: Backend, llm, *, namespaces: list[str] | None = None, exclude: set[str] = frozenset(), log_scan: bool = True,
                 cluster: str = "live", tenant: str = "platform", cooldown_s: int = 900, event_window_s: int = 900,
                 audit_every_s: int = 86_400, rightsize_every_s: int = 604_800, max_parallel: int = 4,
                 emit: Callable[[dict], None] = lambda r: None, log: Callable[[str], None] = lambda s: None,
                 clock: Callable[[], float] = time.monotonic, available: Callable[[], list[str]] | None = None):
        self.b, self.llm, self.cluster, self.tenant, self.log_scan = backend, llm, cluster, tenant, log_scan
        self.fixed, self.exclude = namespaces, set(exclude) | SYSTEM_NAMESPACES
        self.cooldown, self.window, self.max_parallel = cooldown_s, event_window_s, max_parallel
        self.emit, self.log, self.clock = emit, log, clock
        self.available = available or backend.namespaces
        self.metrics = Metrics()
        self.diagnosed: dict[str, set[str]] = {}          # fingerprints present at each namespace's last diagnosis
        self.last_run: dict[str, float] = {}
        start = clock()
        self.every = {"audit": audit_every_s, "rightsize": rightsize_every_s}
        self.due = {m: start + s for m, s in self.every.items() if s}

    def namespaces(self) -> list[str]:
        return self.fixed or [ns for ns in self.available() if ns not in self.exclude]

    def scan(self) -> dict[str, dict[str, str]]:
        t0, found = time.perf_counter(), {}
        watched = self.namespaces()
        for ns in watched:
            try:
                found[ns] = scan_namespace(self.b, ns, self.window, self.log_scan)
            except (LookupError, OSError, ValueError, subprocess.TimeoutExpired) as e:
                self.metrics.inc("doctor_scan_errors_total", namespace=ns)
                self.log(f"scan error in {ns}: {e}")
        self.metrics.set("doctor_watched_namespaces", len(watched))
        self.metrics.set("doctor_scan_seconds", round(time.perf_counter() - t0, 3))
        self.metrics.set("doctor_last_scan_timestamp_seconds", time.time())
        self.metrics.inc("doctor_scans_total")
        for ns, fps in found.items():
            self.metrics.set("doctor_open_problems", len(fps), namespace=ns)
        return found

    def plan(self, found: dict[str, dict[str, str]]) -> list[dict]:
        """Tasks to run this cycle: investigations for changed namespaces, then any scheduled work that is due."""
        now, tasks = self.clock(), []
        for ns, fps in found.items():
            prev = self.diagnosed.get(ns, set()) & set(fps)       # forget resolved problems so a relapse triggers again
            self.diagnosed[ns] = prev
            new = sorted(set(fps) - prev)
            if not new:
                continue
            if ns in self.last_run and now - self.last_run[ns] < self.cooldown:
                self.metrics.inc("doctor_skipped_total", reason="cooldown")
                continue
            self.metrics.inc("doctor_detections_total", len(new), namespace=ns)
            symptoms = "; ".join(fps[fp] for fp in sorted(fps))
            tasks.append({"id": f"watch-{ns}", "task_type": "investigate", "namespaces": [ns], "tenant": self.tenant,
                          "max_steps": MAX_STEPS["investigate"], "trigger": new, "fingerprints": sorted(fps),
                          "report": f"Automated detection in {ns}: {symptoms}. Find the root cause of each problem."})
        watched = sorted(found)
        for mode, due in list(self.due.items()):
            if now < due or not watched:
                continue
            self.due[mode] = now + self.every[mode]
            groups = [watched[i:i + AUDIT_GROUP] for i in range(0, len(watched), AUDIT_GROUP)] if mode == "audit" else [[ns] for ns in watched]
            for i, g in enumerate(groups):
                tasks.append({"id": f"{mode}-{i + 1}", "task_type": mode, "namespaces": g, "tenant": self.tenant,
                              "max_steps": MAX_STEPS[mode], "trigger": ["schedule"], "report": REPORTS[mode]})
        return tasks

    def run(self, task: dict) -> dict:
        t0 = time.perf_counter()
        try:
            run = run_task(task, self.b, self.llm, cluster=self.cluster)
        except Exception as e:  # one task must never stop the watcher
            self.log(f"{task['task_type']} {','.join(task['namespaces'])} crashed: {type(e).__name__}: {e}")
            run = {"diagnosis": None, "steps": [], "trace": [], "repairs": 0, "stop": f"error_{type(e).__name__}",
                   "run_id": f"{task['id']}-error"}
        wall = round(time.perf_counter() - t0, 2)
        d, steps, mode = run["diagnosis"], run["steps"], task["task_type"]
        status = (d or {}).get("status", "none")
        rec = {"time": dt.datetime.now(dt.UTC).strftime("%Y-%m-%dT%H:%M:%SZ"), "mode": mode, "namespaces": task["namespaces"],
               "trigger": task["trigger"], "status": status, "stop": run["stop"], "diagnosis": d, "run_id": run["run_id"],
               "model_calls": len(steps), "tool_calls": len(run["trace"]), "repairs": run["repairs"], "wall_s": wall,
               "prompt_tokens": sum(s.get("prompt_tokens") or 0 for s in steps),
               "cached_tokens": sum(s.get("cached_tokens") or 0 for s in steps),
               "completion_tokens": sum(s.get("completion_tokens") or 0 for s in steps)}
        m = self.metrics
        m.inc("doctor_diagnoses_total", mode=mode, status=status, stop=run["stop"])
        m.inc("doctor_diagnosis_seconds_sum", wall, mode=mode)
        m.inc("doctor_diagnosis_seconds_count", mode=mode)
        for key in ("prompt_tokens", "cached_tokens", "completion_tokens"):
            m.inc(f"doctor_{key}_total", rec[key], mode=mode)
        m.inc("doctor_model_calls_total", len(steps), mode=mode)
        for f in (d or {}).get("findings", []):
            m.inc("doctor_findings_total", category=f["category"])
        return rec

    def cycle(self) -> list[dict]:
        found = self.scan()
        tasks = self.plan(found)
        if tasks:
            self.log(f"{len(tasks)} diagnosis task(s): " + ", ".join(f"{t['task_type']} {','.join(t['namespaces'])}" for t in tasks))
        with ThreadPoolExecutor(max_workers=max(1, self.max_parallel)) as ex:
            recs = list(ex.map(self.run, tasks))
        for task, rec in zip(tasks, recs, strict=True):
            if task["task_type"] == "investigate":
                ns = task["namespaces"][0]
                if rec["stop"].startswith(RETRY_STOPS):           # refused or unreachable, so try again next scan
                    self.metrics.inc("doctor_skipped_total", reason="retry")
                else:
                    self.diagnosed[ns] = set(task["fingerprints"])
                    self.last_run[ns] = self.clock()
            self.log(_line(rec))
            self.emit(rec)
        return recs

    def forever(self, interval_s: float, stop: threading.Event | None = None) -> None:
        stop = stop or threading.Event()
        while not stop.is_set():
            t0 = time.monotonic()
            self.cycle()
            stop.wait(max(0.0, interval_s - (time.monotonic() - t0)))


def _line(rec: dict) -> str:
    d = rec["diagnosis"] or {}
    what = "; ".join(f"{f['category']} {f['kind']} {f['namespace']}/{f['name']}" for f in d.get("findings", [])) or rec["stop"]
    return (f"{rec['mode']} {','.join(rec['namespaces'])} → {rec['status'].upper()} {what} "
            f"({rec['model_calls']} calls, {rec['wall_s']}s, prompt {rec['prompt_tokens']:,}, cached {rec['cached_tokens']:,})")


# ---- entry point ------------------------------------------------------------------------------

def main(argv: list[str], *, llm=None, stdout=None, stderr=None, open_source=None) -> int:
    from .cli import EXIT_NO_DIAGNOSIS, EXIT_USAGE, SourceError, open_source as _open  # noqa: I001  (a local import avoids an import cycle)
    out, err = stdout or sys.stdout, stderr or sys.stderr
    ap = argparse.ArgumentParser(prog="python -m agent watch", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group()
    src.add_argument("--context", help="kubectl context (default: the current context)")
    src.add_argument("--snapshot", help="comma-separated recorded faults instead of a live cluster")
    ap.add_argument("--kubeconfig")
    ap.add_argument("-n", "--namespaces", help="comma-separated namespaces (default: all but system namespaces)")
    ap.add_argument("--exclude", default="", help="comma-separated namespaces to ignore (e.g. monitoring,hami-system)")
    ap.add_argument("--interval", type=float, default=60)
    ap.add_argument("--no-log-scan", action="store_true", help="skip the error-line count over running pods' recent logs")
    ap.add_argument("--cooldown", type=int, default=900)
    ap.add_argument("--event-window", type=int, default=900, help="ignore Warning events older than this (s)")
    ap.add_argument("--audit-every", type=int, default=86_400)
    ap.add_argument("--rightsize-every", type=int, default=604_800)
    ap.add_argument("--max-parallel", type=int, default=4)
    ap.add_argument("--base-url", default=os.environ.get("DOCTOR_BASE_URL", "http://127.0.0.1:8000/v1"))
    ap.add_argument("--profile", default=os.environ.get("DOCTOR_PROFILE"), help="model profile: served name and sampling (D-40)")
    ap.add_argument("--model", help="served model name (default: the profile's, DOCTOR_MODEL, or Qwen/Qwen3-8B-AWQ)")
    ap.add_argument("--tenant", default="platform")
    ap.add_argument("--out", help="append one JSON line per diagnosis to this file")
    ap.add_argument("--metrics-addr", default="127.0.0.1:9109", help="host:port for /metrics; empty to disable")
    ap.add_argument("--once", action="store_true", help="one scan-and-diagnose cycle, then exit")
    try:
        a = ap.parse_args(argv)
    except SystemExit as e:
        return EXIT_USAGE if e.code else 0

    def fail(code: int, msg: str) -> int:
        print(f"cluster-doctor watch: {msg}", file=err)
        return code

    try:
        a.model, client = resolve_model(a.model, a.profile)
    except (OSError, ValueError, KeyError) as e:
        return fail(EXIT_USAGE, f"cannot read model profile {a.profile!r}: {e}")
    try:
        check_name(a.tenant, "tenant")
        fixed = [check_name(x.strip(), "namespace") for x in (a.namespaces or "").split(",") if x.strip()] or None
        exclude = {check_name(x.strip(), "namespace") for x in a.exclude.split(",") if x.strip()}
        backend, available, cluster, where = (open_source or _open)(a.context, a.snapshot, a.kubeconfig)
    except ValueError as e:
        return fail(EXIT_USAGE, str(e))
    except SourceError as e:
        return fail(e.code, str(e))
    missing = [ns for ns in fixed or [] if ns not in available]
    if missing:
        return fail(EXIT_USAGE, f"namespace not found: {', '.join(missing)}")

    lock = threading.Lock()

    def emit(rec: dict) -> None:
        line = json.dumps(rec, ensure_ascii=False)
        with lock:
            print(line, file=out, flush=True)
            if a.out:
                with open(a.out, "a") as f:
                    f.write(line + "\n")

    def log(msg: str) -> None:
        print(f"{time.strftime('%H:%M:%S')} {msg}", file=err, flush=True)

    snapshot_ns = available if a.snapshot else None
    w = Watcher(backend, llm or build_llm(a.base_url, a.model, client=client), namespaces=fixed, exclude=exclude, cluster=cluster, log_scan=not a.no_log_scan,
                tenant=a.tenant, cooldown_s=a.cooldown, event_window_s=a.event_window, audit_every_s=a.audit_every,
                rightsize_every_s=a.rightsize_every, max_parallel=a.max_parallel, emit=emit, log=log,
                available=(lambda: snapshot_ns) if snapshot_ns else None)
    server = serve_metrics(w.metrics, a.metrics_addr) if a.metrics_addr else None
    log(f"watching {where} · {len(w.namespaces())} namespaces · every {a.interval:g}s · model {a.model} at {a.base_url}"
        + (f" · metrics http://{a.metrics_addr}/metrics" if server else ""))
    try:
        if a.once:
            w.cycle()
        else:
            w.forever(a.interval)
    except KeyboardInterrupt:
        log("stopped")
    except SourceError as e:
        return fail(EXIT_NO_DIAGNOSIS, str(e))
    finally:
        if server:
            server.shutdown()
    return 0
