# Capacity — Qwen3-8B-AWQ on A100 40 GB, two HAMi slices, cluster-doctor workload

*Paper numbers from `inference-architect/scripts/feasibility.py` (inherited from the trip-planner
project, D-16 there). Token sizes **measured** 2026-09-27 with the pinned Qwen3 tokenizer
(`Qwen/Qwen3-8B-AWQ` @ `4da05a8…`) over the recorded lab snapshots, after the tiered catalogue (D-31).
Per worker = one 20 GiB slice. Re-measure on live logs (they are longer than the lab's).*

| Quantity | Value | Source |
|---|---|---|
| Weights (AWQ 4-bit) | 5.7 GiB | `[hf]` |
| KV bytes / token | 147,456 B (144 KiB) = 2 × 36 layers × 8 KV heads × 128 × 2 B | `[hf]` → formula |
| KV pool per worker | paper ≈ 10.95 GiB ≈ 79,700 tokens · **measured 79,056 tokens** (4,941 blocks × 16; `vllm:cache_config_info`, 2026-09-27) — paper within 0.8 %; vLLM's own max concurrency at 24,576 = 3.22 | measured |
| Model context limit | 40,960 positions; served with `--max-model-len 24576` | `[hf]` config.json |
| Shared prefix: ruleset 1,202 + tool schemas 2,525 + ~60 template | **3,787 tokens** | measured |
| Cluster card | 112 tokens (1 node, 23 namespaces on kind) | measured |

**Unique tokens per task** (reference traces: problem pods, events, logs and describes of up to three pods per namespace, services, diagnosis):

| Tier / task type | median | max | end-of-task context (max) |
|---|---|---|---|
| easy / investigate | 2,459 | 5,482 | 9.4k |
| multi_hop / investigate | 3,570 | 9,752 | 13.7k |
| red_herring / investigate | 4,768 | 9,726 | 13.6k |
| rightsizing / rightsize | 5,200 | 5,200 | 9.1k |
| easy / audit (3–4 namespaces) | 7,743 | 8,046 | 11.9k |
| multi_hop / audit (3 namespaces) | 11,176 | 11,176 | **15.1k** |

The multi-hop audit reached the old 16,384 limit, so `--max-model-len` is raised to 24,576 and the
loop's context stop to 23,000. vLLM uses the limit only for admission — KV blocks are allocated as tokens
arrive — so short tasks cost nothing extra (D-29 amended).

```
tasks that fit in KV = (79,700 − 3,899 shared) ÷ unique tokens per task
  easy investigations ≈ 31 · multi-hop ≈ 21 · red-herring ≈ 16 · audits ≈ 7–10
KV at --max-num-seqs 32, easy investigations only = (3,899 + 32 × 2,459) ÷ 79,700 ≈ 1.03  → over-full
0.80 shed line ≈ 24 easy · 17 multi-hop · 13 red-herring investigations · 6 audits
```

**First limiter.** KV, clearly: even easy investigations overfill the pool before the 32 decode slots
are used, and every harder tier (longer logs, more hops) makes it worse — the course's stated first failure.
The gateway's KV line (0.80) is the admission control that matters; audits (batch) are the first to shed.
What would change this: a measured pool far from 79.7k, or a cached-prefix share far below the trip
planner's 0.95 (the 3.8k shared prefix is ~40 % of an easy investigation's context).

## First GPU session — measured (2026-09-27, vllm-0 only, `/metrics` totals)
| Quantity | Value | Reading |
|---|---|---|
| KV pool | 79,056 tokens | paper 79,700 (−0.8 %); the 20 GiB HAMi slice is in effect |
| Prompt tokens / served from prefix cache | 8,998,048 / 8,573,088 = **95.3 %** | only ~425k tokens were actually prefilled |
| Generated tokens | 323,430, of which ~249k were whitespace from 324 `length`-capped requests (D-38) | useful decode ≈ 75k: prompt : useful output ≈ 120 : 1 |
| Preemptions | 0 | KV never ran out; peak usage ~70 % at ~7 concurrent long requests |
| TTFT | first request ~4.5 s (cold), then p50 ~0.1–0.3 s, p95 ≤ ~1 s | cold vs warm: the warm-up proof |
| ITL | p50 ~5 ms at 1 request → ~17 ms (p95 ~24 ms) at ~7 | per-stream decode slows ~3×, aggregate rises |
| Prompt length | p50 5–10k, p95 up to ~20k (audits) | the 24,576 window is needed |
