"""The model matrix for the write-up and the grader: every profile's fit, every golden result and one ranking (D-40).

    python -m serving.matrix                 # writes design/model-matrix.md from deploy/ and metrics/
    python -m serving.matrix --stdout

Inputs: deploy/models/*.json and deploy/serving.json (paper fit), metrics/kv-<model>-<topology>-*.log (measured KV,
from `make kv`) and metrics/golden-*.summary.json (from `make golden`). Nobody types a number in by hand. To change
a number, rerun the thing that produced it.

The headline result per model and topology is the newest FULL-SET run (all golden tasks, no --only). Its v2 pass
rate (D-41) carries a 95 % Wilson interval; runs whose intervals overlap are not distinguishable yet. "Correct per
GPU-hour" = v2 passes ÷ wall hours ÷ GPU share used (workers × SM share) at the run's concurrency. This number
puts two sliced workers and one whole-card worker on the same axis.
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from serving.fit import all_rows, table, verdict
from serving.profiles import ROOT, load_serving, profiles

OUT = ROOT / "design" / "model-matrix.md"
TIERS = ("easy", "multi_hop", "red_herring", "rightsizing")


def _served_to_profile() -> dict[str, str]:
    return {p["serve"]["served_name"]: p["name"] for p in profiles()}


def load_runs(metrics: Path, n_tasks: int) -> list[dict]:
    """Every golden summary, normalised. Summaries from before D-40 carry no profile or topology: this maps them by
    served model name and marks them `assumed` (every run before D-40 was on the sliced topology)."""
    s, names, runs = load_serving(), _served_to_profile(), []
    for f in sorted(metrics.glob("golden-*.summary.json")):
        r = json.loads(f.read_text())
        assumed = "profile" not in r
        profile = r.get("profile") or names.get(r.get("model"), r.get("model") or "?")
        topo = r.get("topology") or "sliced"
        workers = r.get("workers") or 1
        share = r.get("gpu_share") or workers * s["topologies"].get(topo, {"gpucores": 100})["gpucores"] / 100
        unique = r.get("unique_tasks") or (r["tasks"] // max(1, r.get("repeat") or 1))
        full = unique >= n_tasks and not r.get("only")
        wall_h = (r.get("wall_s") or 0) / 3600
        per_gpu_h = (r["passed_v2"] / wall_h / share) if ("passed_v2" in r and wall_h > 0 and share) else None
        runs.append({**r, "file": f.name, "profile": profile, "topology": topo, "workers": workers, "gpu_share": share,
                     "unique_tasks": unique, "full": full, "assumed": assumed, "correct_per_gpu_hour": per_gpu_h})
    return sorted(runs, key=lambda r: r.get("timestamp") or "")


def _pct(x) -> str:
    return "n/a" if x is None else f"{x:.0%}"


def _ci(r: dict) -> str:
    ci = r.get("pass_rate_v2_ci95")
    return f"{_pct(r.get('pass_rate_v2'))} [{ci[0]:.0%}–{ci[1]:.0%}]" if ci else _pct(r.get("pass_rate_v2"))


def headline(runs: list[dict]) -> dict[tuple, dict]:
    """Newest full-set run per (profile, topology, workers)."""
    best: dict[tuple, dict] = {}
    for r in runs:
        if r["full"]:
            best[(r["profile"], r["topology"], r["workers"])] = r
    return best


def results_table(best: dict[tuple, dict]) -> str:
    head = ("| Model | Topology | Workers | Tasks × repeat | v2 pass [95 % CI] | v1 pass | " + " | ".join(t.replace("_", "-") for t in TIERS)
            + " | Root found | Category | Mechanism | Abstained / failed closed | Steps / task | Step p50 / p95 s | Cached share | "
            "Correct / GPU-hour | Concurrency | Run |")
    out = [head, "|" + "---|" * (head.count("|") - 1)]
    for (_, _, _), r in sorted(best.items(), key=lambda kv: -(kv[1].get("pass_rate_v2") or 0)):
        tiers = r.get("pass_rate_by_tier_v2") or {}
        parts = r.get("parts_v2") or {}
        stops = r.get("stop_reasons") or {}
        out.append(f"| {r['profile']} | {r['topology']} | {r['workers']} | {r['unique_tasks']} × {r.get('repeat') or 1} | {_ci(r)} | "
                   f"{_pct(r.get('pass_rate'))} | " + " | ".join(_pct(tiers.get(t)) for t in TIERS) + " | "
                   f"{_pct(parts.get('root'))} | {_pct(parts.get('category'))} | {_pct(parts.get('mechanism'))} | "
                   f"{stops.get('abstained', 0)} / {stops.get('inconclusive', 0)} | {r.get('steps_per_task_mean') or 'n/a'} | "
                   f"{r.get('step_latency_s_p50') or 'n/a'} / {r.get('step_latency_s_p95') or 'n/a'} | {_pct(r.get('cached_share_of_prompt'))} | "
                   f"{'n/a' if r['correct_per_gpu_hour'] is None else round(r['correct_per_gpu_hour'])} | {r.get('concurrency', 'n/a')} | "
                   f"{r.get('tag')} {r.get('timestamp', '')} {r.get('git_commit') or ''} |")
    return "\n".join(out)


def ranking(best: dict[tuple, dict], fits: list[dict]) -> list[str]:
    lines = []
    ranked = sorted(best.values(), key=lambda r: (-(r.get("pass_rate_v2") or 0), -(r["correct_per_gpu_hour"] or 0)))
    for i, r in enumerate(ranked, 1):
        lo = (r.get("pass_rate_v2_ci95") or [None])[0]
        nxt = ranked[i] if i < len(ranked) else None
        sep = ""
        if nxt and nxt.get("pass_rate_v2_ci95") and lo is not None:
            sep = (". Clearly ahead of the next model" if lo > nxt["pass_rate_v2_ci95"][1]
                   else ". Not yet distinguishable from the next model, because the intervals overlap")
        rate = "no throughput figure" if r["correct_per_gpu_hour"] is None else f"{r['correct_per_gpu_hour']:.0f} correct diagnoses per GPU-hour"
        lines.append(f"{i}. **{r['profile']}** on {r['topology']} × {r['workers']}: v2 {_ci(r)}, {rate}{sep}")
    run_keys = {(k[0], k[1]) for k in best}
    for p in profiles():
        rows = [f for f in fits if f["model"] == p["name"]]
        if any((p["name"], f["topology"]) in run_keys for f in rows):
            continue
        where = "; ".join(f"{f['topology']}: {verdict(f)}" for f in rows)
        why = {"baseline": "because no full-set run is on file yet", "round-1": "scheduled for the next GPU session",
               "round-2": "to run if time allows after round 1", "paper-only": "not scheduled"}[p["plan"]]
        lines.append(f"- Not run: **{p['name']}**, plan {p['plan']}, {why}. Fit: {where}.")
    return lines


def history(runs: list[dict]) -> str:
    head = "| When | Tag | Model | Topology | Tasks (unique) | Repeat | v1 | v2 | Stops | Commit | Note |"
    out = [head, "|" + "---|" * (head.count("|") - 1)]
    for r in reversed(runs):
        note = ("full set" if r["full"] else f"partial: {r.get('only') or 'subset'}") + ("; profile/topology assumed (pre D-40)" if r["assumed"] else "")
        stops = ", ".join(f"{k} {v}" for k, v in (r.get("stop_reasons") or {}).items())
        out.append(f"| {r.get('timestamp', '?')} | {r.get('tag')} | {r['profile']} | {r['topology']} | {r['unique_tasks']} | "
                   f"{r.get('repeat') or 1} | {r.get('passed', '?')}/{r['tasks']} | "
                   f"{str(r['passed_v2']) + '/' + str(r['tasks']) if 'passed_v2' in r else 'n/a'} | {stops} | {r.get('git_commit') or 'n/a'} | {note} |")
    return "\n".join(out)


def render(metrics: Path | None = None) -> str:
    metrics = metrics or ROOT / "metrics"
    s = load_serving()
    n_tasks = sum(1 for line in (ROOT / "evals" / "golden" / "tasks.jsonl").read_text().splitlines() if line.strip())
    fits = all_rows(metrics)
    runs = load_runs(metrics, n_tasks)
    best = headline(runs)
    e = s["engine"]
    gpus = ", ".join(f"{g['name']} ({k})" for k, g in s["gpus"].items())
    parts = [
        "# Model matrix",
        "",
        "`make matrix` (serving/matrix.py) generates this file from `deploy/models/*.json`, `deploy/serving.json` and "
        "`metrics/`, and CI fails if it is stale. Don't edit it by hand. To change a number, rerun whatever produced it. "
        "D-40 covers the profiles, topologies and this matrix, and D-41 covers the v2 score. "
        "`design/model-architecture-guide.md` explains the architecture.",
        "",
        f"The GPUs are the {gpus}. Every model runs with the same engine settings: `{e['image'].split('@')[0].split('/')[-1]}`, "
        f"context {e['max_model_len']:,}, at most {e['max_num_seqs']} sequences, {e['max_num_batched_tokens']:,} batched tokens, "
        f"prefix caching, {e['gpu_memory_utilization']} of the memory HAMi exposes, and 16-bit KV. Sampling follows each "
        "model's card, as set in the profile's `client` block.",
        "",
        "Each topology names its GPU:",
        *[f"- **{k}** ({v['gpu']}), {v['purpose']}." for k, v in s["topologies"].items()],
        "",
        "## 1. Where each model can run",
        "",
        "`serving/fit.py` computes the paper numbers, and the measured tokens come from vLLM's own startup report (`make kv`). "
        "Sequences at 24k are full-length requests that fit at once. At 12k, each worker caches the 3,787-token shared prefix "
        "once. Prefill is the uncached time for 12k tokens on the topology's share of the SMs. Hybrid models also keep a "
        "recurrent state per sequence, which is an estimate.",
        "",
        table(fits),
        "",
        f"## 2. Results on the golden set ({n_tasks} tasks)",
        "",
        "Each row is the newest full-set run for that model, topology and worker count. v1 is the original score, kept for "
        "continuity. v2 is the corrected score (D-41): it counts only evidence the model was shown, accepts equally "
        "supported categories, checks that the mechanism is stated, and treats required tools as advisory. The tier, "
        "root-found, category and mechanism columns are v2 rates over all task runs.",
        "",
        results_table(best) if best else "_No full-set run is on file yet. Run `make golden MODEL=<profile> TOPO=<topology> REPEAT=3`._",
        "",
        "## 3. Ranking and status",
        "",
        "Models are ranked by v2 pass rate, then by correct diagnoses per GPU-hour. Quality is compared on the full-GPU "
        "topology, because slicing changes capacity and latency but not the answers. The chosen model then serves the "
        "slicing, routing and KV-hop demonstration on the sliced topology.",
        "",
        *ranking(best, fits),
        "",
        "## 4. Every run on file",
        "",
        history(runs) if runs else "_none_",
        "",
        "One result isn't in these files. The first full baseline (2026-09-27, 8B, sliced) scored 9/26 on v1 in its first "
        "pass, but the crash fixed in D-39 lost its run file. Only D-39 records it.",
        "",
        "## 5. Adding a model",
        "",
        "1. Write `deploy/models/<name>.json` with the pinned revision, the architecture from its `config.json`, the vLLM "
        "args and the sampling from its model card.",
        "2. Run `make fit MODEL=<name> TOPO=full`, and `TOPO=sliced`, to see the paper fit and the gate.",
        "3. On the GPU, run `make deploy MODEL=<name> TOPO=<topology>`, then `make kv MODEL=… TOPO=…`, then "
        "`make golden MODEL=… TOPO=… TAG=<name> REPEAT=3 CONC=4`.",
        "4. Run `make matrix` and commit `metrics/` and this file.",
        "",
    ]
    return "\n".join(parts)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--stdout", action="store_true")
    ap.add_argument("--metrics", default=str(ROOT / "metrics"))
    a = ap.parse_args(argv)
    text = render(Path(a.metrics))
    if a.stdout:
        sys.stdout.write(text)
    else:
        OUT.write_text(text)
        print(f"wrote {OUT.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
