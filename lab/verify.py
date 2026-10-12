#!/usr/bin/env python3
"""Verdicts on the three open findings, from the files a session leaves behind (F43, F44, F45).

    python -m lab.verify client-gone metrics/events-<tag>-<stamp>.jsonl
    python -m lab.verify hop metrics/gateway-log-<tag>-<stamp>.jsonl
    python -m lab.verify ramp metrics/events-<tag>-<stamp>.jsonl metrics/gateway-log-<tag>-<stamp>.jsonl --pod vllm-1

client-gone  Did the engine stop generating for a client that hung up, and did its KV come back (F43)? Reads the
             vllm_sample events the client-gone probe writes around each hang-up.
hop          Did a hopped step find its history in the destination's cache (F45)? Reads the gateway's request log
             lines (`make gateway-log`) and compares three groups of steps: hopped, moved without a copy, same worker.
ramp         What did a returned worker do in its first minutes back (F44)? Placements, latency, errors and finished
             runs per 30 s window, next to the worker that stayed up.

Each command prints a table and one verdict line, and exits 0. The verdict says "unclear" rather than guess.
"""
from __future__ import annotations

import argparse
import json
import re
import statistics
import sys
from collections import defaultdict
from datetime import datetime
from pathlib import Path

ABORT_SLACK_TOKENS = 32          # one engine step may land after the hang-up
HISTORY_SLACK_TOKENS = 64        # the prefix cache counts whole 16-token blocks
MOVED = {"below_threshold", "recompute_cheaper", "failed", "busy"}


def read_jsonl(path: Path) -> list[dict]:
    out = []
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            out.append(json.loads(line))
        except ValueError:
            continue                                     # kubectl logs can interleave a non-JSON line
    return out


def epoch(text: str) -> float:
    """slog's RFC 3339 time, with any number of fractional digits, as seconds."""
    text = re.sub(r"(\.\d{6})\d+", r"\1", text).replace("Z", "+00:00")
    return datetime.fromisoformat(text).timestamp()


def median(xs: list[float]) -> float | None:
    return round(statistics.median(xs), 3) if xs else None


# --- client-gone (F43) -------------------------------------------------------------------------------------------

def client_gone_verdicts(events: list[dict]) -> list[dict]:
    """One verdict per client-gone run: which worker took it, whether generation stopped, whether the KV came back."""
    by_run: dict[str, dict[str, dict[float, dict]]] = defaultdict(lambda: defaultdict(dict))
    hung_up = {}
    for e in events:
        if e["probe"] != "client-gone":
            continue
        d = e["detail"]
        if e["kind"] == "vllm_sample" and "error" not in d:
            by_run[d["run"]][d["pod"]][d["at_s"]] = d
        elif e["kind"] == "client_gone_aborted":
            hung_up[d["run"]] = d["seconds"]
    out = []
    for run in sorted(set(by_run) | set(hung_up)):
        pods = by_run.get(run)
        if not pods:
            out.append({"run": run, "verdict": "unclear", "why": "no vllm_sample events; the probe predates the sampling"})
            continue
        if run not in hung_up:
            out.append({"run": run, "verdict": "unclear", "why": "the probe never hung up (answer came early or errored)"})
            continue
        before = {pod: s.get(-1.0) for pod, s in pods.items()}
        grown = {}
        for pod, s in pods.items():
            after = [t for t in s if t >= 0]
            if before.get(pod) and after:
                grown[pod] = s[min(after)].get("generation_tokens", 0) - before[pod].get("generation_tokens", 0)
        under_load = any(b and b.get("running", 0) > 0 for b in before.values())
        if not grown or max(grown.values()) <= 0:
            out.append({"run": run, "verdict": "unclear", "why": "no worker generated anything after the send", "under_load": under_load})
            continue
        pod = max(grown, key=grown.get)
        s, b = pods[pod], before[pod]
        early = s.get(2.0) or s[min(t for t in s if t >= 0)]
        last = s[max(s)]
        late_tokens = last.get("generation_tokens", 0) - early.get("generation_tokens", 0)
        running = {t: int(s[t].get("running", 0)) for t in sorted(s) if t >= 0}
        kv = {t: round(s[t].get("kv_usage", 0.0), 4) for t in sorted(s)}
        aborts = last.get("finished", {}).get("abort", 0) - b.get("finished", {}).get("abort", 0)
        row = {"run": run, "pod": pod, "hung_up_after_s": hung_up[run], "under_load": under_load,
               "tokens_after_2s": int(late_tokens), "running": running, "kv_usage": kv, "abort_counter_delta": int(aborts)}
        if under_load:
            row.update(verdict="unclear", why="other requests were running on the worker, so its counters are theirs too")
        elif late_tokens <= ABORT_SLACK_TOKENS and running.get(max(running), 0) <= int(b.get("running", 0)):
            back = kv[max(kv)] <= kv[-1.0] + 0.005
            row.update(verdict="aborted", why="generation stopped within 2 s of the hang-up and the running count fell back"
                       + ("; KV usage returned to its level before the request" if back else "; KV usage did not return"))
        elif late_tokens > ABORT_SLACK_TOKENS:
            row.update(verdict="ran_on", why=f"the engine generated {int(late_tokens)} more tokens after the hang-up")
        else:
            row.update(verdict="unclear", why="generation stopped but the running count stayed up")
        out.append(row)
    return out


# --- hop (F45) ---------------------------------------------------------------------------------------------------

def request_lines(log: list[dict]) -> list[dict]:
    return [r for r in log if r.get("msg") == "request" and r.get("run") and r.get("step")]


def hop_groups(log: list[dict]) -> dict:
    """Every step after the first, grouped by how it reached its worker, with the share that found its history cached."""
    reqs = request_lines(log)
    runs: dict[str, list[dict]] = defaultdict(list)
    for r in reqs:
        runs[r["run"]].append(r)
    first = [r["cached_tokens"] for r in reqs if r["step"] == 1 and r.get("status") == 200 and r.get("cached_tokens")]
    groups: dict[str, list[dict]] = {"hopped": [], "moved_recompute": [], "same_pod": []}
    for steps in runs.values():
        steps.sort(key=lambda r: r["step"])
        for prev, cur in zip(steps, steps[1:], strict=False):
            if cur.get("status") != 200 or not prev.get("prompt_tokens") or cur["step"] != prev["step"] + 1:
                continue
            hop = cur.get("hop") or ""
            if hop == "hopped":
                group = "hopped"
            elif hop in MOVED or cur.get("pod") != prev.get("pod"):
                group = "moved_recompute"
            else:
                group = "same_pod"
            found = cur.get("cached_tokens", 0) >= prev["prompt_tokens"] - HISTORY_SLACK_TOKENS
            groups[group].append({"run": cur["run"], "step": cur["step"], "from": prev.get("pod"), "to": cur.get("pod"),
                                  "history_tokens": prev["prompt_tokens"], "cached_tokens": cur.get("cached_tokens", 0),
                                  "found_history": found, "hop_ms": cur.get("hop_ms"), "upstream_ms": cur.get("upstream_ms")})
    summary = {}
    for name, rows in groups.items():
        summary[name] = {"n": len(rows),
                         "found_history_share": round(sum(r["found_history"] for r in rows) / len(rows), 3) if rows else None,
                         "median_cached_over_history": median([r["cached_tokens"] / r["history_tokens"] for r in rows]),
                         "median_upstream_ms": median([r["upstream_ms"] for r in rows if r.get("upstream_ms") is not None]),
                         "median_hop_ms": median([r["hop_ms"] for r in rows if r.get("hop_ms")])}
    hopped, moved = summary["hopped"], summary["moved_recompute"]
    if not hopped["n"]:
        verdict, why = "unclear", "no hopped step in the log (GW_HOP_MIN_TOKENS=1 forces them)"
    elif not moved["n"]:
        verdict, why = "unclear", "no moved-without-copy step to compare against"
    elif hopped["found_history_share"] >= 0.8 and moved["found_history_share"] <= 0.2:
        verdict, why = "transferred", "hopped steps found their history in the destination's cache and recomputed moves did not"
    elif abs(hopped["found_history_share"] - moved["found_history_share"]) <= 0.2:
        verdict, why = "not_transferred", "hopped steps found no more of their history than moves that recomputed"
    else:
        verdict, why = "unclear", "the two groups differ, but not cleanly"
    return {"shared_prefix_tokens": median(first), "groups": summary, "rows": groups, "verdict": verdict, "why": why}


# --- ramp (F44) --------------------------------------------------------------------------------------------------

def ramp_windows(events: list[dict], log: list[dict], pod: str, window_s: float = 30.0, windows: int = 10) -> dict:
    """Per window after the pod's return: placements, median upstream_ms, errors and finished runs, for it and the rest."""
    deleted = ready = None
    for e in events:
        if e["probe"] != "worker-return" or e["detail"].get("pod") != pod:
            continue
        if e["kind"] == "worker_deleted":
            deleted, ready = e["t"], None
        elif e["kind"] == "worker_phase" and e["detail"].get("phase") == "ready" and deleted and e["t"] > deleted + 5:
            ready = ready or e["t"]
    if not deleted or not ready:
        return {"verdict": "unclear", "why": f"no delete and return of {pod} in the events"}
    reqs = [dict(r, t=epoch(r["time"])) for r in request_lines(log) if r.get("time")]
    last_step = {}
    for r in reqs:
        if r["run"] not in last_step or r["step"] > last_step[r["run"]]["step"]:
            last_step[r["run"]] = r
    finished_at = {r["run"]: r["t"] for r in last_step.values() if r.get("status") == 200}

    def stats(rows: list[dict]) -> dict:
        return {"placements": len(rows), "median_upstream_ms": median([r["upstream_ms"] for r in rows if r.get("upstream_ms") is not None]),
                "median_cached_share": median([r["cached_tokens"] / r["prompt_tokens"] for r in rows if r.get("prompt_tokens")]),
                "errors": sum(1 for r in rows if r.get("status") != 200)}

    baseline = stats([r for r in reqs if r.get("pod") == pod and deleted - 120 <= r["t"] < deleted])
    table = []
    for i in range(windows):
        lo, hi = ready + i * window_s, ready + (i + 1) * window_s
        inside = [r for r in reqs if lo <= r["t"] < hi]
        table.append({"window_s": int(i * window_s), "returned": stats([r for r in inside if r.get("pod") == pod]),
                      "others": stats([r for r in inside if r.get("pod") != pod]),
                      "runs_finished": sum(1 for t in finished_at.values() if lo <= t < hi)})
    gap_errors = sum(1 for r in reqs if deleted <= r["t"] < ready and r.get("status") != 200)
    first = next((w["window_s"] for w in table if w["returned"]["placements"]), None)
    settled = next((w["window_s"] for w in table if w["returned"]["placements"] >= 3 and w["others"]["median_upstream_ms"]
                    and w["returned"]["median_upstream_ms"] <= 1.25 * w["others"]["median_upstream_ms"]), None)
    if first is None:
        verdict, why = "unclear", "the returned worker took no placement in the window; the load may have ended first"
    elif settled is None:
        verdict, why = "slow_ramp", f"first placement at {first} s after ready, latency never within 25% of the other worker"
    else:
        verdict, why = "ramped", f"first placement at {first} s after ready, latency within 25% of the other worker by {settled} s"
    return {"pod": pod, "down_s": round(ready - deleted, 1), "errors_while_down": gap_errors, "baseline_before_delete": baseline,
            "windows": table, "verdict": verdict, "why": why}


# --- printing ----------------------------------------------------------------------------------------------------

def table(rows: list[list], head: list[str]) -> str:
    cells = [head, *[[("" if v is None else str(v)) for v in r] for r in rows]]
    width = [max(len(r[i]) for r in cells) for i in range(len(head))]
    return "\n".join("  ".join(c.ljust(w) for c, w in zip(r, width, strict=True)) for r in cells)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("what", choices=["client-gone", "hop", "ramp"])
    ap.add_argument("files", nargs="+", type=Path, help="events.jsonl, gateway-log.jsonl, or both for ramp")
    ap.add_argument("--pod", default="vllm-1", help="ramp: the worker the probe deleted")
    ap.add_argument("--json", action="store_true", help="print the verdict as JSON instead of a table")
    a = ap.parse_args(argv)
    if a.what == "client-gone":
        result = client_gone_verdicts(read_jsonl(a.files[0]))
        if a.json:
            print(json.dumps(result, indent=1))
        else:
            print(table([[r["run"], r.get("pod"), r.get("under_load"), r.get("tokens_after_2s"), r.get("abort_counter_delta"),
                          r["verdict"], r["why"]] for r in result],
                        ["run", "pod", "under_load", "tokens_after_2s", "aborts", "verdict", "why"]))
        return 0
    if a.what == "hop":
        result = hop_groups(read_jsonl(a.files[0]))
        if a.json:
            print(json.dumps({k: v for k, v in result.items() if k != "rows"}, indent=1))
        else:
            print(f"shared prefix (median cached on step 1): {result['shared_prefix_tokens']}")
            print(table([[g, s["n"], s["found_history_share"], s["median_cached_over_history"], s["median_upstream_ms"], s["median_hop_ms"]]
                         for g, s in result["groups"].items()],
                        ["group", "n", "found_history", "cached/history", "upstream_ms", "hop_ms"]))
            print(f"verdict: {result['verdict']}. {result['why']}")
        return 0
    if len(a.files) < 2:
        ap.error("ramp needs the events file and the gateway log")
    result = ramp_windows(read_jsonl(a.files[0]), read_jsonl(a.files[1]), a.pod)
    if a.json:
        print(json.dumps(result, indent=1))
    elif "windows" not in result:
        print(f"verdict: {result['verdict']}. {result['why']}")
    else:
        print(f"{result['pod']} was down {result['down_s']} s; {result['errors_while_down']} request(s) failed meanwhile; "
              f"before the delete it took {result['baseline_before_delete']['placements']} placements in 120 s "
              f"at a median {result['baseline_before_delete']['median_upstream_ms']} ms")
        print(table([[w["window_s"], w["returned"]["placements"], w["returned"]["median_upstream_ms"], w["returned"]["median_cached_share"],
                      w["returned"]["errors"], w["others"]["placements"], w["others"]["median_upstream_ms"], w["runs_finished"]]
                     for w in result["windows"]],
                    ["after_ready_s", "returned_n", "returned_ms", "returned_cached", "returned_err", "others_n", "others_ms", "runs_done"]))
        print(f"verdict: {result['verdict']}. {result['why']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
