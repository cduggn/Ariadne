#!/usr/bin/env python3
"""Run the golden set through the doctor against a model endpoint and score every diagnosis.

    python3 -m evals.run_golden --base-url http://127.0.0.1:8080/v1 --model Qwen/Qwen3-8B-AWQ --tag gw-baseline
    python3 -m evals.run_golden ... --only dx-oom,dx-audit-1 --concurrency 8 --no-harness

The cluster data comes from the recorded snapshots (deterministic); only the model is live. Writes
metrics/golden-<tag>-<ts>.jsonl (one row per task) and .summary.json (pass rate overall and by task
type, failed rules, stop reasons, inconclusive count, tokens, cached share, latency, refusals by HTTP
status). With --concurrency > 1 the same harness is the app-shaped load generator for the gateway.
"""
from __future__ import annotations

import argparse
import json
import statistics
import sys
import time
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from doctor.agent import build_llm, run_task  # noqa: E402
from evals.build_golden import backend_for  # noqa: E402
from evals.checker import check  # noqa: E402

TASKS = ROOT / "evals" / "golden" / "tasks.jsonl"


def load_tasks(only: str | None) -> list[dict]:
    tasks = [json.loads(line) for line in TASKS.read_text().splitlines() if line.strip()]
    return [t for t in tasks if not only or t["id"] in only.split(",")]


def score(task: dict, run: dict) -> dict:
    b = backend_for(task["snapshots"])
    v = check(task, run["diagnosis"], run["trace"], b)
    steps = run["steps"]
    return {"id": task["id"], "task_type": task["task_type"], "tier": task.get("tier", "easy"), "pass": v["pass"], "failed": v["failed"], "stop": run["stop"],
            "n_steps": len(steps), "trace": run["trace"], "repairs": run["repairs"], "harness": run["harness"],
            "http_status": [s.get("http_status") for s in steps if s.get("http_status")],
            "tool_errors": sum(1 for s in steps for c in s.get("calls", []) if c.get("error")),
            "prompt_tokens": [s.get("prompt_tokens") for s in steps], "completion_tokens": [s.get("completion_tokens") for s in steps],
            "cached_tokens": [s.get("cached_tokens") for s in steps], "latency_s": [s.get("latency_s") for s in steps],
            "headers": run["headers"], "diagnosis": run["diagnosis"]}


def _pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    return round(xs[min(len(xs) - 1, int(p * len(xs)))], 3) if xs else None


def summarise(rows: list[dict], meta: dict) -> dict:
    flat = lambda k: [x for r in rows for x in r[k] if x is not None]  # noqa: E731
    by: dict[str, list[bool]] = {}
    tiers: dict[str, list[bool]] = {}
    for r in rows:
        by.setdefault(r["task_type"], []).append(r["pass"])
        tiers.setdefault(r["tier"], []).append(r["pass"])
    return {**meta, "tasks": len(rows), "passed": sum(r["pass"] for r in rows),
            "pass_rate": round(sum(r["pass"] for r in rows) / max(1, len(rows)), 3),
            "pass_rate_by_type": {k: round(sum(v) / len(v), 3) for k, v in sorted(by.items())},
            "pass_rate_by_tier": {k: round(sum(v) / len(v), 3) for k, v in sorted(tiers.items())},
            "stop_reasons": dict(Counter(r["stop"] for r in rows)),
            "inconclusive": sum(r["stop"] == "inconclusive" for r in rows),
            "http_refusals": dict(Counter(c for r in rows for c in r["http_status"])),
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
    ap.add_argument("--model", default="Qwen/Qwen3-8B-AWQ")
    ap.add_argument("--only")
    ap.add_argument("--concurrency", type=int, default=1)
    ap.add_argument("--repeat", type=int, default=1, help="run the task list N times (load generation)")
    ap.add_argument("--tag", default="run")
    ap.add_argument("--no-harness", action="store_true")
    ap.add_argument("--out", default=str(ROOT / "metrics"))
    a = ap.parse_args()

    tasks = load_tasks(a.only) * max(1, a.repeat)
    llm = build_llm(a.base_url, a.model)
    t0 = time.time()

    def one(task: dict) -> dict:
        row = score(task, run_task(task, backend_for(task["snapshots"]), llm, harness=not a.no_harness))
        print(f"{row['id']:22} {row['tier']:11} {'PASS' if row['pass'] else 'FAIL'} steps={row['n_steps']:2} "
              f"stop={row['stop']:13} {'; '.join(row['failed'])[:110]}", flush=True)
        return row

    with ThreadPoolExecutor(max_workers=max(1, a.concurrency)) as ex:
        rows = list(ex.map(one, tasks))
    stamp = time.strftime("%Y%m%d-%H%M%S")
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    base = out / f"golden-{a.tag}-{stamp}"
    base.with_suffix(".jsonl").write_text("".join(json.dumps(r, ensure_ascii=False) + "\n" for r in rows))
    summary = summarise(rows, {"tag": a.tag, "model": a.model, "base_url": a.base_url, "concurrency": a.concurrency,
                               "repeat": a.repeat, "harness": not a.no_harness, "wall_s": round(time.time() - t0, 1), "timestamp": stamp})
    Path(f"{base}.summary.json").write_text(json.dumps(summary, indent=2))
    print(json.dumps({k: summary[k] for k in ("pass_rate", "pass_rate_by_type", "pass_rate_by_tier", "stop_reasons", "inconclusive",
                                              "http_refusals", "top_failed_rules")}, indent=2))
    print(f"wrote {base}.jsonl and .summary.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
