"""Build and run the report notebook (D-46): report/report.ipynb, plus its charts as design/figures/*.png.

    make report        # uv run --group report python -m report.build

The cells live here, so the notebook is reviewed as code and rebuilt from the committed metrics; running it embeds the
outputs, so GitHub shows the charts without a kernel.
"""
from __future__ import annotations

import sys
import tempfile
from pathlib import Path

import nbformat
from jupyter_client import KernelManager
from nbclient import NotebookClient

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "report" / "report.ipynb"

md, code = nbformat.v4.new_markdown_cell, nbformat.v4.new_code_cell

CELLS = [
    md("""# Cluster doctor: results

What the cluster doctor's serving stack measured on Lambda GPUs between 2026-09-27 and 2026-10-07, answering the
brief's questions. Every number comes from a committed file in `metrics/` (loaded by `report/results.py`), so
`make report` rebuilds this notebook and its charts from the repo. Each section cites the findings it rests on
(`design/findings.md`, F-numbers) and the decisions behind it (`design/decisions.md`, D-numbers).

**The setup.** The doctor is an agent that diagnoses Kubernetes faults in 5 to 16 chained model calls per run. It runs
on self-hosted vLLM behind a Go gateway that guards, admits, places and queues every call (D-42). The golden set is 26
recorded faults in four tiers, scored twice (v1, and the stricter v2, D-41)."""),
    code("""import re
from pathlib import Path

import matplotlib
import matplotlib.pyplot as plt
from IPython.display import Image, Markdown, display

from report.results import FIGURES, by_label, histogram, kv_measured, prom, rows, summary, total
from serving import fit, profiles

ROOT = Path.cwd()                           # the kernel runs in the repo root (report/build.py)

# The reference palette's first three categorical slots (they validate for every pair), light surface and ink.
BLUE, ORANGE, AQUA = "#2a78d6", "#eb6834", "#1baf7a"
SURFACE, INK, INK2, GRID = "#fcfcfb", "#0b0b0b", "#52514e", "#e4e3df"
plt.rcParams.update({
    "figure.facecolor": SURFACE, "axes.facecolor": SURFACE, "savefig.facecolor": SURFACE,
    "axes.edgecolor": GRID, "axes.labelcolor": INK2, "xtick.color": INK2, "ytick.color": INK2, "text.color": INK,
    "axes.spines.top": False, "axes.spines.right": False, "axes.grid": True, "grid.color": GRID, "grid.linewidth": 0.8,
    "axes.axisbelow": True, "font.size": 10, "axes.titlesize": 11, "axes.titleweight": "bold", "legend.frameon": False,
})
FIGURES.mkdir(parents=True, exist_ok=True)

def show(fig, name):
    fig.tight_layout()
    fig.savefig(FIGURES / f"{name}.png", dpi=160)
    plt.close(fig)
    display(Image(filename=str(FIGURES / f"{name}.png")))

def table(header, body):
    lines = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    lines += ["| " + " | ".join(str(c) for c in r) + " |" for r in body]
    display(Markdown("\\n".join(lines)))

def pct(x):
    return f"{x:.1%}"

def ci(s):
    return f"[{s['pass_rate_v2_ci95'][0]:.0%}–{s['pass_rate_v2_ci95'][1]:.0%}]\""""),

    md("""## 1. What the app sends: shared tokens against unique ones

Every call starts with the same ~3.8k-token prefix: the triage ruleset, the tool schemas and the cluster card. Each step
then appends the previous answer and a new tool result, so step *n*'s prompt is step *n−1*'s plus a little more. The
prefix cache only has to compute that new tail, provided the step runs on the worker that served the last one, which is
what the gateway's stickiness is for (F8–F11)."""),
    code("""r = rows("gw-38-20261006-131640")
steps = range(1, 11)
def median(xs):
    xs = sorted(xs)
    return xs[len(xs) // 2] if xs else 0
cached = [median([x["cached_tokens"][i - 1] or 0 for x in r if len(x["cached_tokens"]) >= i]) for i in steps]
prompt = [median([x["prompt_tokens"][i - 1] or 0 for x in r if len(x["prompt_tokens"]) >= i]) for i in steps]
uncached = [p - c for p, c in zip(prompt, cached, strict=True)]
fig, ax = plt.subplots(figsize=(7.5, 3.6))
ax.bar(steps, cached, color=BLUE, width=0.62, label="served from the prefix cache")
ax.bar(steps, uncached, bottom=cached, color=ORANGE, width=0.62, label="computed (new tail)",
       edgecolor=SURFACE, linewidth=2)
ax.set(title="Prompt tokens per agent step (median over 52 runs, Qwen3.8-27B)", xlabel="step in the run",
       ylabel="tokens", xticks=list(steps))
ax.grid(axis="x", visible=False)
ax.legend(loc="upper left")
show(fig, "prompt_per_step")
s = summary("gw-38-20261006-131640")
display(Markdown(f"Over every step of the run, **{pct(s['cached_share_of_prompt'])}** of prompt tokens came from the cache."))"""),

    md("""## 2. Which model: quality on the golden set

The same 26 tasks, twice each, scored v2 with 95% Wilson intervals. Qwen3.8-27B (FP8, Aug 2026) is clearly above the 8B
and probably above the 30B-A3B, whose interval overlaps it slightly (F1). Through the gateway or not, the 8B scores the
same: routing changes where a call runs, not what it answers (F2)."""),
    code("""models = [
    ("Qwen3-8B AWQ · A100 slice", "baseline-20260928-161531"),
    ("Qwen3-8B AWQ · 2 slices, via gateway", "gw-ptl-20261004-162326"),
    ("Qwen3-30B-A3B AWQ · whole A100", "30b-smoke-20260928-164635"),
    ("Qwen3.8-27B FP8 · 2 H100 halves, via gateway", "gw-38-20261006-131640"),
]
fig, ax = plt.subplots(figsize=(7.5, 3.0))
for y, (_, run) in enumerate(models):
    s = summary(run)
    lo, hi = s["pass_rate_v2_ci95"]
    ax.plot([lo, hi], [y, y], color=INK2, linewidth=2, solid_capstyle="round")
    ax.plot(s["pass_rate_v2"], y, "o", color=BLUE, markersize=9, markeredgecolor=SURFACE, markeredgewidth=2)
    ax.annotate(pct(s["pass_rate_v2"]), (s["pass_rate_v2"], y), xytext=(0, 9), textcoords="offset points",
                ha="center", color=INK, fontsize=9)
ax.set_yticks(range(len(models)), [m[0] for m in models])
ax.set(xlim=(0, 1), ylim=(-0.6, len(models) - 0.3), xlabel="pass rate")
ax.set_title("v2 pass rate with 95% interval (26 tasks × 2)", pad=12)
ax.xaxis.set_major_formatter(matplotlib.ticker.PercentFormatter(1.0))
ax.grid(axis="y", visible=False)
show(fig, "model_quality")
table(["Model, where", "v2 pass [95% CI]", "easy", "multi-hop", "red herring", "rightsizing"],
      [[name, f"{pct(summary(run)['pass_rate_v2'])} {ci(summary(run))}"]
       + [pct(summary(run)["pass_rate_by_tier_v2"].get(t, 0)) for t in ("easy", "multi_hop", "red_herring", "rightsizing")]
       for name, run in models])"""),

    md("""## 3. Capacity on paper against what vLLM measured

`serving/fit.py` predicts each worker's KV pool from the model's config and the slice's memory. vLLM reports the real
pool at startup (`make kv`). The fit is within 1–6% for standard-attention models, and 20–34% optimistic for the hybrid
Qwen3.8, more so on a smaller slice (F5). On an H100 half that leaves room for about two full-length (24k-token) runs
per worker, which makes KV the first limiter (F6)."""),
    code("""pairs = [("qwen3-8b-awq", "sliced", "8B · A100 slice"), ("qwen3-30b-a3b-2507-awq", "full", "30B-A3B · whole A100"),
         ("qwen3.8-27b-fp8", "h100-full", "Qwen3.8 · whole H100"), ("qwen3.8-27b-fp8", "h100-half", "Qwen3.8 · H100 half")]
paper, measured = [], []
for model, topo, _ in pairs:
    paper.append(fit.fit(profiles.load_profile(model), topo)["tokens_paper"])
    measured.append(kv_measured(model, topo))
fig, axes = plt.subplots(1, len(pairs), figsize=(8.5, 3.2))
for ax, (_, _, label), p, m in zip(axes, pairs, paper, measured, strict=True):
    ax.bar([0], [p], color=BLUE, width=0.7)
    ax.bar([1], [m], color=ORANGE, width=0.7)
    ax.set_xticks([0, 1], ["paper", "measured"])
    ax.set_title(label, fontsize=9.5)
    ax.annotate(f"{m / p - 1:+.0%}", (1, m), xytext=(0, 4), textcoords="offset points", ha="center", fontsize=9)
    ax.yaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: f"{v / 1000:.0f}k"))
    ax.grid(axis="x", visible=False)
axes[0].set_ylabel("KV tokens per worker")
fig.suptitle("KV pool per worker: the fit's estimate against vLLM's own report", fontweight="bold", fontsize=11)
show(fig, "kv_fit")
table(["Pair", "paper", "measured", "gap"],
      [[label, f"{p:,}", f"{m:,}", f"{m / p - 1:+.1%}"] for (_, _, label), p, m in zip(pairs, paper, measured, strict=True)])"""),

    md("""## 4. Placement: does keeping a run on its worker matter?

The same golden workload twice on two Qwen3.8 halves at concurrency 4, changing only the gateway's pick policy:
`prefix_then_load` keeps a run on the worker that holds its history, `least_loaded` ignores history. Answers are the same.
Without stickiness, 10% more prompt tokens were recomputed and steps were 8–11% slower (F29). The gap is modest at this
load because the two workers are evenly loaded, so `least_loaded` often picks the run's previous worker anyway."""),
    code("""arms = [("prefix_then_load", "gw-38-20261006-131640"), ("least_loaded", "gw-38-ll-20261006-140918")]
def uncached(run):
    return sum(sum(p or 0 for p in x["prompt_tokens"]) - sum(c or 0 for c in x["cached_tokens"]) for x in rows(run))
def p95_after_first(run):
    xs = sorted(t for x in rows(run) for t in x["latency_s"][1:] if t)
    return xs[int(0.95 * len(xs))]
metrics_ab = [("v2 pass rate", lambda r: summary(r)["pass_rate_v2"], "{:.1%}"),
              ("cached share of prompt", lambda r: summary(r)["cached_share_of_prompt"], "{:.1%}"),
              ("prompt tokens recomputed", uncached, "{:,.0f}"),
              ("step p95, after step 1 (s)", p95_after_first, "{:.1f}")]
fig, axes = plt.subplots(1, len(metrics_ab), figsize=(9.5, 3.0))
for ax, (title, f, fmt) in zip(axes, metrics_ab, strict=True):
    vals = [f(run) for _, run in arms]
    ax.bar([0, 1], vals, color=[BLUE, ORANGE], width=0.7)
    for i, v in enumerate(vals):
        ax.annotate(fmt.format(v), (i, v), xytext=(0, 3), textcoords="offset points", ha="center", fontsize=9)
    ax.set_xticks([0, 1], ["sticky", "least\\nloaded"])
    ax.set_title(title, fontsize=9.5)
    ax.set_yticks([])
    ax.grid(False)
    ax.spines["left"].set_visible(False)
fig.suptitle("Routing A/B: prefix_then_load (sticky) against least_loaded, same workload", fontweight="bold", fontsize=11)
show(fig, "routing_ab")
g = by_label(prom("gateway-gw-38-ll-20261006-142337"), "orch_sticky_total", "outcome")
cont = g.get("hit", 0) + g.get("broken_load", 0) + g.get("broken_shed", 0)
display(Markdown(f"Under `least_loaded`, **{g.get('hit', 0) / cont:.0%}** of continuing steps happened to land on the "
                 "worker holding their history; under `prefix_then_load` it was 94% even across the overloaded sweep (F29)."))"""),

    md("""## 5. Under load: the knee, and sizing admission to KV

The golden set at rising concurrency on two Qwen3.8 halves (26 tasks per level). On 10-06, with the gateway allowing 16
requests in flight per worker, runs failed past 8 concurrent: each refused step ended its run, and at 32 vLLM preempted
(F24, F25). On 10-07 (a faster H100 SXM5) two changes were tested on the same node: the doctor now waits out a refusal
(D-44), and the in-flight cap is sized to the measured KV pool, 4 per half (D-43). The cap kept vLLM healthy, with ~95%
fewer preemptions, a warmer cache and lower tail latency, but fewer runs finished, because queued requests hit the
gateway's deadline before the retries outlasted the queue (F36). Sizing for zero preemption is stricter than the workload
needs; the next step is a cap of 6–8 (backlog)."""),
    code("""curves = [
    ("10-06 PCIe · cap 16 · no retries", ORANGE,
     {4: "gw-38-20261006-131640", 8: "sweep-qwen3.8-27b-fp8-c8-20261006-135139",
      16: "sweep-qwen3.8-27b-fp8-c16-20261006-135533", 32: "sweep-qwen3.8-27b-fp8-c32-20261006-135709"}),
    ("10-07 SXM5 · cap 16 · retries", AQUA,
     {16: "sweep-qwen3.8-27b-fp8-c16-20261007-160229", 32: "sweep-qwen3.8-27b-fp8-c32-20261007-160543"}),
    ("10-07 SXM5 · cap 4 (KV-sized) · retries", BLUE,
     {4: "gw-38-kv-20261007-153043", 16: "sweep-qwen3.8-27b-fp8-c16-20261007-151105",
      32: "sweep-qwen3.8-27b-fp8-c32-20261007-151406"}),
]
fig, ax = plt.subplots(figsize=(7.5, 3.8))
levels = [4, 8, 16, 32]                    # evenly spaced: each level doubles the one before
for label, color, runs in curves:
    xs = sorted(runs)
    ys = [summary(runs[c])["pass_rate_v2"] for c in xs]
    ax.plot([levels.index(c) for c in xs], ys, "-o", color=color, linewidth=2, markersize=8,
            markeredgecolor=SURFACE, markeredgewidth=2, label=label)
ax.set(ylim=(0, 1), title="v2 pass rate against concurrent runs (two Qwen3.8 workers)",
       xlabel="concurrent agent runs", ylabel="v2 pass rate")
ax.set_xticks(range(len(levels)), [str(c) for c in levels])
ax.grid(axis="x", visible=False)
ax.yaxis.set_major_formatter(matplotlib.ticker.PercentFormatter(1.0))
ax.legend(loc="lower left")
show(fig, "knee")
def preempt(stamp, level):
    return sum(total(prom(f"{pod}-sweep-c{level}-{stamp}"), "vllm:num_preemptions_total") for pod in ("vllm-0", "vllm-1"))
rows_t = []
for label, stamp in [("10-06 PCIe · cap 16 · no retries", "20261006-135138"), ("10-07 SXM5 · cap 16 · retries", "20261007-160229"),
                     ("10-07 SXM5 · cap 4 · retries", "20261007-151105")]:
    p16, p32 = preempt(stamp, 16), preempt(stamp, 32)
    rows_t.append([label, f"{p16:.0f}", f"{p32 - p16:.0f}"])
table(["Run", "vLLM preemptions at c=16", "added at c=32"], rows_t)"""),

    md("""## 6. What dies where: guard, admit, place, queue

The gateway names every refusal (`orch_shed_total{reason}`); the client sees only 429 (tenant over quota, stays local)
or 503 (capacity, may overflow). Up to 16 concurrent runs the KV shed (`kv_free`) protected vLLM from preemption (F25).
With admission sized to KV the overload moved into the gateway's queue, so refusals became `timeout_queue` (F32).
No restricted request ever left the box."""),
    code("""scrapes = [("10-06 · cap 16 · golden + sweep", "gateway-sweep-c32-20261006-135138"),
           ("10-07 · cap 4 · sweep 16 & 32", "gateway-sweep-c32-20261007-151105"),
           ("10-07 · cap 16 · control 16 & 32", "gateway-kv-control-20261007-160843")]
reasons = sorted({r for _, s in scrapes for r, v in by_label(prom(s), "orch_shed_total", "reason").items() if v})
table(["Gateway counters"] + reasons + ["restricted off-box"],
      [[label] + [f"{by_label(prom(s), 'orch_shed_total', 'reason').get(r, 0):.0f}" for r in reasons]
       + [f"{total(prom(s), 'orch_restricted_offbox_total'):.0f}"] for label, s in scrapes])
display(Markdown("Counters run from each gateway start, and `kubectl set env` restarts the gateway, so each row covers "
                 "only its own runs. The 10-06 `upstream_error` 502s were client aborts, now counted as `client_gone` "
                 "(F28, D-44)."))"""),

    md("""## 7. Inside the engine: continuous batching and chunked prefill

vLLM rebuilds one batch per worker on every forward pass (an engine step), capped at 32 requests and 8,192 tokens. The
histogram of tokens per step shows both effects: single-token steps are decode with a batch of one, 2–32 tokens are
several requests decoding together, and larger steps carry a prefill. Steps near 8,192 are long prompts split by chunked
prefill; they are rare because the prefix cache keeps each step's uncached tail to a few hundred tokens (F14, F15)."""),
    code("""h = histogram(prom("vllm-0-kv-control-20261007-160843"), "vllm:iteration_tokens_total")
labels, counts, lo = [], [], 0
for le, n in h:
    if le == float("inf"):
        labels.append(f">{int(lo):,}")
    else:
        labels.append(f"{int(lo) + 1:,}–{int(le):,}" if le > 1 else "1")
        lo = le
    counts.append(n)
keep = [i for i, n in enumerate(counts) if n]
fig, ax = plt.subplots(figsize=(8, 3.4))
ax.bar(range(len(keep)), [counts[i] for i in keep], color=BLUE, width=0.7)
ax.set_xticks(range(len(keep)), [labels[i] for i in keep], rotation=30, ha="right")
ax.set(yscale="log", title="Tokens per engine step on vllm-0, 10-07 (all runs)", xlabel="tokens in one forward pass",
       ylabel="engine steps (log scale)")
ax.grid(axis="x", visible=False)
show(fig, "engine_steps")
big = sum(n for le, n in h if le > 4096)
display(Markdown(f"Of **{sum(counts):,.0f}** steps, **{big:,.0f}** processed more than 4,096 tokens."))"""),

    md("""## 8. Production alerts

Five rules in `deploy/observability/alerts.yaml`, each threshold taken from the system and each tested with promtool to
fire on its condition and stay quiet below it (D-45)."""),
    code("""text = (ROOT / "deploy" / "observability" / "alerts.yaml").read_text()
names = [(n, " ".join(e.split())) for n, e in
         re.findall(r"- alert: (\\w+)\\n\\s+expr: >?(.*?)\\n\\s+(?:for|labels):", text, re.S)]
sev = re.findall(r"severity: (\\w+)", text)
table(["Alert", "Condition", "Severity"], [[n, f"`{e}`", s] for (n, e), s in zip(names, sev, strict=True)])"""),

    md("""## 9. Recommendations, and what changes at 10× traffic

- **Model:** serve Qwen3.8-27B FP8 for diagnosis quality; keep the 8B as the fallback that fits an A100 slice.
- **Admission:** keep sizing the in-flight cap from the measured KV pool, but for a small preemption budget rather than
  none (a cap of 6–8 on an H100 half); lengthen the interactive queue deadline if completion matters more than latency.
- **Placement:** keep `prefix_then_load`. It costs nothing at low load and saves prefill as load grows uneven.
- **At 10× traffic:**
  - more workers, not bigger slices: KV per worker is the limiter, so add halves or whole cards and let the gateway
    spread runs;
  - a second gateway replica needs the run table shared or runs partitioned by id (D-42);
  - an overflow backend for non-restricted work (Superlinked serves the same model, F22) turns refusals into slower
    answers;
  - the KV hop starts paying off once contexts grow and moves become common (F35).
- **Knobs that are wrong for this workload:** the 768-token output cap never bound (F30); chunked prefill rarely binds
  (F15); the fit calculator needs a hybrid-model correction before it sizes a Qwen3.8 deployment (F5)."""),
]


def build() -> nbformat.NotebookNode:
    nb = nbformat.v4.new_notebook(cells=CELLS)
    nb.metadata["kernelspec"] = {"name": "python3", "display_name": "Python 3", "language": "python"}
    with tempfile.TemporaryDirectory() as sockets:              # a local socket, not unencrypted TCP, kept out of the repo
        km = KernelManager(kernel_name="python3", transport="ipc", ip=str(Path(sockets) / "kernel"))
        NotebookClient(nb, km=km, timeout=300, resources={"metadata": {"path": str(ROOT)}}).execute()
    return nb


def main() -> int:
    nb = build()
    OUT.write_text(nbformat.writes(nb))
    print(f"wrote {OUT.relative_to(ROOT)} and design/figures/*.png")
    return 0


if __name__ == "__main__":
    sys.exit(main())
