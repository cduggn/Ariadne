# Findings

What we measured or learned, with the evidence for each. `decisions.md` records what we chose and why; this file
records what the system showed us. Revise both together before the presentation.

Each finding has a label:
- **measured**: from a file in `metrics/` or a live scrape, named next to it;
- **estimate**: from `serving/fit.py` or arithmetic, not yet measured;
- **observed**: seen on a dashboard or in logs during a session, not saved as a file;
- **unverified**: a likely explanation nobody has checked.

## 1. Model quality

| Model, where | v2 pass [95% CI] | Tiers (v2) | Evidence |
|---|---|---|---|
| Qwen3-8B-AWQ, A100 slice, direct | 51.9% [38.7–64.9] | easy 68, multi-hop 43, red herring 25, rightsizing 0 | `metrics/golden-baseline-20260928-161531.summary.json` (measured) |
| Qwen3-8B-AWQ, A100 slices ×2, **through the gateway** | 48.1% [35.1–61.3] | easy 61, multi-hop 43, red herring 25, rightsizing 0 | `metrics/golden-gw-ptl-20261004-162326.summary.json` (measured) |
| Qwen3-30B-A3B-2507 AWQ, whole A100 | 67.3% [53.8–78.5] | easy 75, multi-hop 50, red herring 75, rightsizing 50 | `metrics/golden-30b-smoke-20260928-164635.summary.json` (measured) |
| **Qwen3.8-27B-FP8, H100 halves ×2, through the gateway** | **86.5% [74.7–93.3]** | easy 89, multi-hop 71, **red herring 100, rightsizing 100** | `metrics/golden-gw-38-20261006-131640.summary.json` (measured) |

- **F1. Qwen3.8-27B is the best model, clearly above the 8B.** Its interval (74.7–93.3) sits above the 8B's
  (38.7–64.9). Against the 30B-A3B (53.8–78.5) the intervals overlap slightly, so "better" there is likely but not
  established at 52 runs. (measured)
- **F2. The gateway changes placement, not answers.** On the 8B, 48.1% through the gateway against 51.9% direct, with
  overlapping intervals. (measured)
- **F3. Qwen3.8's failures are mostly the audits.** 5 of its 7 failed runs are `dx-audit-1/2/3`: 22–24 steps, then
  inconclusive after repairs. Audits cover 4 namespaces per task, so they run out of steps rather than reasoning
  wrongly. The other two (`dx-eviction`, `dx-tls-expired`) each passed on their other repeat. (measured)
- **F4. Two doctor-side defects cost the 8B a lot of its score.** In the 8B baseline's 52 runs, the `describe` tool
  rejected a capitalised `kind` 101 times (`Deployment`; it accepts only `deployment`), and diagnoses with more than 8
  evidence refs failed validation 24 times, including runs that had found the right root cause. Both are in
  `design/backlog.md`. (measured: counted in `metrics/golden-baseline-20260928-161531.jsonl`)

## 2. Capacity and KV

| Pair | Paper KV pool | Measured | Gap | Evidence |
|---|---|---|---|---|
| Qwen3-8B-AWQ, A100 20 GiB slice | 79,699 | 79,056 | −0.8% | `metrics/kv-qwen3-8b-awq-sliced-*.log` (same on 09-28 and 10-04) |
| Qwen3-30B-A3B AWQ, whole A100 | 197,563 | 185,136 | −6.3% | `metrics/kv-qwen3-30b-a3b-2507-awq-full-20260928-164223.log` |
| Qwen3.8-27B-FP8, whole H100 | 653,004 | 525,797 | −19.5% | `metrics/kv-qwen3.8-27b-fp8-h100-full-20261006-101648.log` |
| **Qwen3.8-27B-FP8, H100 39 GiB half** | 77,926 | **51,092** | **−34.4%** | `metrics/kv-qwen3.8-27b-fp8-h100-half-20261006-125054.log` |

- **F5. The fit calculator is accurate for standard attention and over-optimistic for the hybrid model.** Within 1% for
  the 8B and 6% for the 30B-A3B, but 20% off on the whole H100 and 34% off on a half for Qwen3.8. The fixed costs it
  underestimates (recurrent state, the vision encoder and MTP head in the checkpoint, larger activations) weigh more on
  a smaller slice. (measured; the cause is unverified)
- **F6. On a half H100, Qwen3.8 holds about two full-length requests.** vLLM reports a maximum concurrency of 2.08 at
  24,576 tokens, against the fit's 2.9. KV, not the 32-sequence cap, is the first limiter. (measured)
- **F7. The A100 KV pool is stable across sessions and engine attempts:** 79,056 tokens on 09-27, 09-28 and twice on
  10-04. (measured)

## 3. Prefix caching and the gateway

- **F8. Prefix caching carries most of the prompt.** Cached share of prompt tokens: 94.9% for the 8B direct, 94.6%
  through the gateway, 96.9% for the 30B-A3B. (measured, golden summaries)
- **F9. Qwen3.8 caches less: 87.8%.** Its hybrid Gated DeltaNet layers can only resume from block-boundary checkpoints
  (vLLM's `align` mode), so some tail tokens are recomputed that a standard-attention model would reuse. (measured; the
  cause is the likely explanation, unverified)
- **F10. On the gateway dashboard during the Qwen3.8 run, about 97% of prompt tokens per second were cache hits:**
  shared prefix ~4–6k tokens/s, run history ~2–4k, uncached a few hundred. (observed, `orch_prompt_tokens_total`.) This
  is higher than F9's 87.8% because the dashboard showed a stretch mid-run, while the summary covers every step,
  including each run's first.
- **F11. Stickiness is what keeps run history cached.** `run_hit` is each run's own history served from cache; without
  stickiness it would turn into `miss`. The `least_loaded` arm of the A/B measures how much. (A/B pending)

## 4. Latency, batching and chunked prefill

- **F12. Qwen3.8 on half an H100 is decode-bound.** Step latency p50 2.9 s and p95 24.2 s (`golden-gw-38` summary),
  against 0.44 s and 6.5 s for the 8B direct. The fit's decode limit is about 32 tokens/s per stream on half the card,
  so a 300–768-token diagnosis takes 10–25 s on its own. (measured latency; the decode limit is an estimate)
- **F13. A spike to ~40 s end-to-end on vllm-1 with TTFT and queue time falling** is one long decode, not a fault: one
  request writing a long answer while few others ran. (observed; not yet tied to a specific request)
- **F14. Continuous batching only appears when requests overlap.** Of vllm-0's 44,121 engine steps, 31,169 processed a
  single token (decode with a batch of one, from the concurrency-1 sweep level) and 12,507 processed 2–8 tokens (2–8
  requests decoding together, mostly the concurrency-4 golden run). (measured, live `vllm:iteration_tokens_total`
  on 10-06)
- **F15. Chunked prefill is configured but rarely binds.** Only 8 of 44,121 steps on vllm-0 were in the 4,097–8,192-token
  range, because the prefix cache keeps each step's uncached tail to a few hundred tokens. It would bind for a cold
  worker, a run moved without the KV hop, or a very large tool output. (measured, same histogram)
- **F16. vllm-1 ran 3 steps above the 8,192-token cap** (`--max-num-batched-tokens`). Possibly vLLM handling the
  hybrid layers differently. (measured; unverified cause)
- **F17. In our stack, queueing happens at the gateway, not in vLLM.** The gateway holds each worker to 16 in flight,
  under vLLM's 32-sequence cap, so vLLM's own waiting queue should stay near zero and priority ordering happens at the
  gateway. (design; check `vllm:num_requests_waiting` in the sweep)

## 4b. Load: the knee (sweep, 10-06)

Qwen3.8-27B-FP8 on two H100 halves, through the gateway with `prefix_then_load`, gateway in-flight cap 16 per worker,
26 tasks per level (`REPEAT=1`).

| Concurrency | v2 pass [95% CI] | Refused (503 `kv_free`) | vLLM preemptions (cumulative, both workers) | Step p50 / p95 s | Cached share |
|---|---|---|---|---|---|
| 4 (golden, ×2) | 86.5% [74.7–93.3] | 0 | 0 | 2.9 / 24.2 | 87.8% |
| 8 | 88.5% [71.0–96.0] | 1 | 0 | 3.1 / 25.0 | 86.1% |
| 16 | 50.0% [32.1–67.9] | 13 | 0 | 3.4 / 24.0 | 81.9% |
| 32 | 38.5% [22.4–57.5] | 14 | **6** (3 + 3) | 3.8 / 25.9 | 82.8% |

Evidence: `metrics/golden-sweep-qwen3.8-27b-fp8-c{8,16,32}-20261006-*.summary.json`, and
`metrics/{gateway,vllm-0,vllm-1}-sweep-c{8,16,32}-20261006-135138.prom` for sheds and preemptions. (measured)

- **F24. The knee is between 8 and 16 concurrent runs, and KV is the limiter.** Each half holds ~51k tokens (F6), about
  two full contexts, while 16 runs put ~8 per worker at 5–15k tokens each. Every refusal was the gateway's `kv_free`
  shed. (measured)
- **F25. Up to 16, admission control protected KV: the gateway shed new runs and vLLM preempted nothing.** At 32 it was
  overrun: vLLM preempted 3 requests per worker, and the vLLM dashboard showed the thrashing pattern (waiting up,
  prefix hit rate and throughput down, TTFT, ITL and queue time up). (measured preemptions; observed dashboards)
- **F26. The gateway's in-flight cap is sized for the A100, not the H100 half.** 16 in flight per worker suited a
  79k-token slice holding the 8B; a 51k-token half holding the 27B fits far fewer. Continuing runs are also exempt from
  the KV line down to 5% free, so at high concurrency most requests skip it. Both let more work into vLLM than its KV
  holds. Fix and experiment: `design/backlog.md`, "KV-sized admission". (measured cause; fix pending)
- **F27. A refused run fails outright.** The gateway's 503 carries the reason (`kv_free`) and `Retry-After`, but the
  doctor neither retries nor records the reason, so each shed shows as a failed, undiagnosed run. That is why pass
  rates fall at the knee rather than latency rising. (measured)
- **F28. Client aborts are counted as worker errors.** Cancelling a sweep left 5 requests logged `upstream_error` with
  502 (four in the same millisecond, across both workers), when they should be `client_gone` with no status. The
  "Upstream errors" panel overstates worker faults by those 5. (measured, gateway log 12:51:30–35 UTC)

## 5. Infrastructure and operations

- **F18. GPU fallback works on real hardware.** `make up` skipped GH200 (no capacity), took an H100 PCIe in us-west-3
  on 10-06, and cloud-init detected it and booted Qwen3.8. The H100 reported 81,559 MiB, exactly what `serving.json`
  assumed. (measured, `.cache/ready.json`)
- **F19. vLLM v0.30.0-cu129 crash-loops on start:** the image ships torch cu130 with a cu129 torchvision
  (vllm-project/vllm#59157). We stay on v0.29.0. (measured on 10-04)
- **F20. A slow Lambda boot once produced three billed `cluster-doctor` nodes**, because `make up` retried a launch
  that had created the instance. Fixed: it now refuses while one exists and stops after a launch that leaves one behind;
  `make resume` finishes a slow boot. (observed on 10-06)
- **F21. Lambda can hold a node in `booting` for over 15 minutes** before it becomes active. (observed on 10-06)
- **F22. Superlinked overflow is blocked on their billing.** Their API serves `Qwen/Qwen3.8-27B-FP8`, the same model,
  but returns HTTP 402 `INSUFFICIENT_CREDITS` for every operation while the console shows a $520 grant; an EU-pinned
  key has no reachable EU endpoint. (measured on 10-04; reported to their support)
- **F23. The KV hop is built and tested but not yet measured on hardware.** Copy bandwidth between two HAMi halves and
  Qwen3.8's hybrid state through the connector are unverified. Test plan H1–H3. (status)

## Still to measure in this session

- The `least_loaded` arm (F11): cached share, `run_hit` against `miss`, step latency.
- After "KV-sized admission" lands: the concurrency-32 level again, against F24's row (38.5%, 14 refused, 6 preempted).
- Whether the 768-token output cap binds (`finish_reason length`; the new "Output and the 768-token cap" panels).
