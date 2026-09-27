# Capacity — Qwen3-8B-AWQ on A100 40 GB, two HAMi slices, cluster-doctor workload

*Paper numbers from `inference-architect/scripts/feasibility.py` (inherited from the trip-planner
project, D-16 there); token sizes **measured** 2026-09-27 with the pinned Qwen3 tokenizer
(`Qwen/Qwen3-8B-AWQ` @ `4da05a8…`) over the recorded lab snapshots. Per worker = one 20 GiB slice.*

| Quantity | Value | Source |
|---|---|---|
| Weights (AWQ 4-bit) | 5.7 GiB | `[hf]` |
| KV bytes / token | 147,456 B (144 KiB) = 2 × 36 layers × 8 KV heads × 128 × 2 B | `[hf]` → formula |
| KV pool per worker | ≈ 10.95 GiB ≈ **79,700 tokens** (paper; measure with `make kv`) | `[estimate]` until measured |
| Shared prefix: ruleset 773 + tool schemas 1,655 + ~60 template | **2,488 tokens** | measured |
| Cluster card | 90 tokens on the 1-node kind lab (larger on k3s: more nodes, namespaces) | measured |
| Unique tokens per investigation | median **1,932**, max 2,908 | measured (reference traces) |
| Unique tokens per audit (3–4 namespaces) | 4,763 – 8,175 | measured |
| End-of-task context | investigation ≤ 5.5k; audit ≤ 10.8k (fits `--max-model-len 16384`) | measured |

```
tasks that fit in KV = (79,700 − 2,578 shared) ÷ unique tokens per task
  investigations (median 1,932) ≈ 40 · (max 2,908) ≈ 26 · audits (8,175) ≈ 9
KV at --max-num-seqs 32, all investigations = (2,578 + 32 × 1,932) ÷ 79,700 ≈ 0.81
0.80 shed line ≈ 32 investigations ≈ 7.5 audits
```

**First-limiter hypothesis.** For interactive-only traffic, KV and decode slots bind together near 32;
as soon as audits mix in (4× the unique tokens), **KV binds first** — the course's stated first failure
("the system runs out of KV memory first"). Live logs are longer than the lab's, so real contexts grow
and KV binds earlier still: re-measure unique tokens on the first live run. What would change this:
a measured pool well under 79.7k, or cached share far below the trip planner's 0.95.
