#!/usr/bin/env python3
"""Pull the node's Prometheus history over a bench window into metrics/ts-<tag>-<stamp>.json, before make down
destroys it (D-48). Every /metrics file we keep is one scrape at the end of a run, so its gauges read 0; this keeps
the 5 s series instead, with the probe events (metrics/events-*.jsonl) that fall inside the window.

    python -m lab.export                                  # the last make bench window (.cache/bench.json), via lam ssh
    python -m lab.export --start 1791400000 --tag run1    # a window by hand
    python -m lab.export --prom-url http://127.0.0.1:9090 # through make prom instead of lam ssh

A query that fails is recorded under `errors` and the rest still export. Exit 0 when at least one series has data.
"""
from __future__ import annotations

import argparse
import json
import math
import shlex
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from collections.abc import Callable
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
METRICS = ROOT / "metrics"
MARKER = ROOT / ".cache" / "bench.json"
PROXY = "/api/v1/namespaces/monitoring/services/prometheus-server:80/proxy"
MAX_POINTS = 11_000                                     # Prometheus refuses a range query with more points per series
MIN_STEP = 5                                            # the node's scrape interval

QUERIES: dict[str, str] = {
    "gw_queue_depth": "orch_replica_queue_depth",
    "gw_inflight": "orch_replica_active_requests",
    "gw_phase": "orch_replica_phase",
    "gw_shed_rate": "sum by (reason) (rate(orch_shed_total[30s]))",
    "gw_pick_rate": "sum by (pod) (rate(orch_pick_total[30s]))",
    "gw_queue_p95": 'histogram_quantile(0.95, sum by (le) (rate(orch_request_duration_seconds_bucket{stage="queue"}[1m])))',
    "gw_e2e_p99": 'histogram_quantile(0.99, sum by (le) (rate(orch_request_duration_seconds_bucket{stage="e2e"}[1m])))',
    "gw_hop_rate": "sum by (result) (rate(orch_hop_total[1m]))",
    "vllm_waiting": "vllm:num_requests_waiting",
    "vllm_running": "vllm:num_requests_running",
    "vllm_kv_usage": "vllm:kv_cache_usage_perc",
    "vllm_preempt_rate": "sum by (pod) (rate(vllm:num_preemptions_total[30s]))",
    "vllm_aborts": 'sum by (pod) (vllm:request_success_total{finished_reason="abort"})',
    "vllm_iter_tokens_p95": "histogram_quantile(0.95, sum by (le, pod) (rate(vllm:iteration_tokens_total_bucket[30s])))",
    "vllm_itl_p95": "histogram_quantile(0.95, sum by (le, pod) (rate(vllm:inter_token_latency_seconds_bucket[30s])))",
    "vllm_ttft_p95": "histogram_quantile(0.95, sum by (le, pod) (rate(vllm:time_to_first_token_seconds_bucket[30s])))",
    "gpu_power_w": "DCGM_FI_DEV_POWER_USAGE",
    "gpu_sm_active": "DCGM_FI_PROF_SM_ACTIVE",
    "vllm_replicas_ready": 'kube_statefulset_status_replicas_ready{statefulset="vllm"}',
    "vllm_replicas_wanted": 'kube_statefulset_replicas{statefulset="vllm"}',
    "keda_desired_replicas": 'kube_horizontalpodautoscaler_status_desired_replicas{horizontalpodautoscaler="keda-hpa-vllm"}',
    "keda_demand": "doctor:vllm_demand_requests",
}

Fetch = Callable[[str], dict]


def ssh_fetch(node: str) -> Fetch:
    """Prometheus through the API server's service proxy on the node; ssh joins the argv into a remote shell line."""
    def fetch(path_and_query: str) -> dict:
        cmd = ["lam", "ssh", node, "--", "kubectl", "get", "--raw", shlex.quote(PROXY + path_and_query)]
        return json.loads(subprocess.run(cmd, check=True, capture_output=True, text=True).stdout)
    return fetch


def http_fetch(url: str) -> Fetch:
    def fetch(path_and_query: str) -> dict:
        with urllib.request.urlopen(url.rstrip("/") + path_and_query, timeout=120) as r:
            return json.load(r)
    return fetch


def step_for(start: float, end: float) -> int:
    return max(MIN_STEP, math.ceil((end - start) / MAX_POINTS))


def query_range(fetch: Fetch, query: str, start: float, end: float, step: int) -> list[dict]:
    body = fetch("/api/v1/query_range?" + urllib.parse.urlencode({"query": query, "start": start, "end": end, "step": step}))
    if body.get("status") != "success":
        raise ValueError(body.get("error") or body.get("errorType") or "status " + str(body.get("status")))
    return [{"labels": {k: v for k, v in r["metric"].items() if k != "__name__"},
             "values": [[float(t), x] for t, v in r["values"] if math.isfinite(x := float(v))]}
            for r in body["data"]["result"]]


def events_in(metrics: Path, start: float, end: float) -> list[dict]:
    lines = (line for p in metrics.glob("events-*.jsonl") for line in p.read_text().splitlines() if line.strip())
    return sorted((e for e in map(json.loads, lines) if start <= e["t"] <= end), key=lambda e: e["t"])


def export(fetch: Fetch, tag: str, start: float, end: float, metrics: Path) -> dict:
    step = step_for(start, end)
    series, errors = {}, {}
    for name, query in QUERIES.items():
        try:
            series[name] = {"query": query, "results": query_range(fetch, query, start, end, step)}
        except Exception as e:                          # one bad query (a missing exporter, a timeout) must not lose the rest
            errors[name] = f"{type(e).__name__}: {e}"
    return {"tag": tag, "start": start, "end": end, "step_s": step, "series": series,
            "events": events_in(metrics, start, end), "errors": errors}


def summary(ts: dict) -> list[str]:
    out = [f"{name}: {len(s['results'])} series, {sum(len(r['values']) for r in s['results'])} points"
           for name, s in ts["series"].items()]
    return out + [f"{name}: ERROR {why}" for name, why in ts["errors"].items()]


def main(argv: list[str] | None = None, fetch: Fetch | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--start", type=float, help="Unix seconds (default: the start in .cache/bench.json)")
    ap.add_argument("--end", type=float, help="Unix seconds (default: now)")
    ap.add_argument("--tag", help="default: the tag in .cache/bench.json")
    ap.add_argument("--node", default="cluster-doctor", help="the lam instance whose Prometheus to read")
    ap.add_argument("--prom-url", help="read Prometheus directly, e.g. http://127.0.0.1:9090 (make prom)")
    a = ap.parse_args(argv)
    marker = json.loads(MARKER.read_text()) if MARKER.is_file() else {}
    start = a.start if a.start is not None else marker.get("start")
    if start is None:
        print(f"no --start and no bench marker at {MARKER}: pass --start <unix seconds>", file=sys.stderr)
        return 2
    fetch = fetch or (http_fetch(a.prom_url) if a.prom_url else ssh_fetch(a.node))
    ts = export(fetch, a.tag or marker.get("tag", "run"), float(start), a.end or time.time(), METRICS)
    METRICS.mkdir(parents=True, exist_ok=True)
    path = METRICS / f"ts-{ts['tag']}-{time.strftime('%Y%m%d-%H%M%S')}.json"
    path.write_text(json.dumps(ts) + "\n")
    print(path)
    print("\n".join(summary(ts)))
    if marker and a.start is None:                       # only the bench window counts as exported for make down
        MARKER.write_text(json.dumps({**marker, "exported": str(path)}) + "\n")
    return 0 if any(r["values"] for s in ts["series"].values() for r in s["results"]) else 1


if __name__ == "__main__":
    sys.exit(main())
