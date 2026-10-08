# Findings

What we measured or learned, with the evidence for each. `decisions.md` records what we chose and why; this file
records what the system showed us. Revise both together before the presentation.

Each finding carries one of four labels:
- **measured** means it comes from a file in `metrics/` or a live scrape, named next to it;
- **estimate** means it comes from `serving/fit.py` or arithmetic and has not been measured;
- **observed** means someone saw it on a dashboard or in logs during a session, and no file holds it;
- **unverified** marks a likely explanation that nobody has checked.

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
- **F7. The A100 KV pool is stable across sessions and engine attempts.** vLLM reported 79,056 tokens on 09-27, 09-28
  and twice on 10-04. (measured)

## 3. Prefix caching and the gateway

- **F8. The prefix cache serves most of the prompt.** Cached share of prompt tokens: 94.9% for the 8B direct, 94.6%
  through the gateway, 96.9% for the 30B-A3B. (measured, golden summaries)
- **F9. Qwen3.8 caches less of its prompt, 87.8%.** Its hybrid Gated DeltaNet layers can only resume from
  block-boundary checkpoints (vLLM's `align` mode), so vLLM recomputes some tail tokens that a standard-attention model
  would reuse. (measured; the cause is the likely explanation, unverified)
- **F10. On the gateway dashboard during the Qwen3.8 run, about 97% of prompt tokens per second were cache hits.** The
  shared prefix ran at ~4–6k tokens/s, run history at ~2–4k and uncached tokens at a few hundred. (observed,
  `orch_prompt_tokens_total`.) This
  is higher than F9's 87.8% because the dashboard showed a stretch mid-run, while the summary covers every step,
  including each run's first.
- **F11. Stickiness is what keeps run history cached.** `run_hit` is each run's own history served from cache; without
  stickiness it would turn into `miss`. The A/B (F29) measured 10% more prefill without stickiness. (measured)

## 4. Latency, batching and chunked prefill

- **F12. Qwen3.8 on half an H100 is decode-bound.** Step latency p50 2.9 s and p95 24.2 s (`golden-gw-38` summary),
  against 0.44 s and 6.5 s for the 8B direct. The fit's decode limit is about 32 tokens/s per stream on half the card,
  so a 300–768-token diagnosis takes 10–25 s on its own. (measured latency; the decode limit is an estimate)
- **F13. A spike to ~40 s end-to-end on vllm-1, with TTFT and queue time falling, is one long decode.** One request
  wrote a long answer while few others ran. It was not a fault. (observed; not yet tied to a specific request)
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
- **F25. Up to 16 concurrent runs, admission control protected KV.** The gateway shed new runs and vLLM preempted
  nothing. At 32 the load overran it. vLLM preempted 3 requests per worker, and the vLLM dashboard showed thrashing,
  with waiting requests, TTFT, ITL and queue time up, and the prefix hit rate and throughput down. (measured
  preemptions; observed dashboards)
- **F26. The gateway's in-flight cap is sized for the A100, not the H100 half.** 16 in flight per worker suited a
  79k-token slice holding the 8B; a 51k-token half holding the 27B fits far fewer. Continuing runs are also exempt from
  the KV line down to 5% free, so at high concurrency most requests skip it. Both let more work into vLLM than its KV
  holds. D-43 now sizes the cap from the measured pool (4 on an H100 half), and F36 measures the trade. (measured
  cause)
- **F27. A refused run fails outright.** The gateway's 503 carries the reason (`kv_free`) and `Retry-After`, but the
  doctor neither retries nor records the reason, so each shed shows as a failed, undiagnosed run. That is why pass
  rates fall at the knee rather than latency rising. (measured; fixed by D-44)
- **F28. Client aborts are counted as worker errors.** Cancelling a sweep left 5 requests logged `upstream_error` with
  502 (four in the same millisecond, across both workers), when they should be `client_gone` with no status. The
  "Upstream errors" panel overstates worker faults by those 5. (measured, gateway log 12:51:30–35 UTC; fixed by D-44)

## 4c. The routing A/B (10-06)

Same workload in both arms: the golden set, 26 tasks × 2, concurrency 4, Qwen3.8-27B-FP8 on two H100 halves through the
gateway, gateway defaults. Only the pick policy differs.

| | `prefix_then_load` (sticky) | `least_loaded` |
|---|---|---|
| v2 pass [95% CI] | 86.5% [74.7–93.3] | 88.5% [77.0–94.6] |
| Cached share of prompt | **87.8%** | 85.6% |
| Uncached prompt tokens prefilled | **416,445** | 459,909 (+10.4%) |
| Step latency p50 | **2.86 s** | 3.10 s (+8%) |
| Step latency p95, steps after the first | **25.3 s** | 28.1 s (+11%) |
| Continuing steps back on their run's worker | **94%** (1,127 of 1,204)* | 77% (300 of 388) |

Evidence: `metrics/golden-gw-38-20261006-131640.*`, `metrics/golden-gw-38-ll-20261006-140918.*`,
`metrics/gateway-gw-38-ll-20261006-142337.prom`, `metrics/gateway-gw-38-20261006-140723.prom`. *The sticky arm's
gateway counters were saved after the sweep as well, so they include the overloaded levels and understate stickiness at
concurrency 4. (measured)

- **F29. Stickiness saves prefill and time without changing answers.** Without it, vLLM recomputed 10% more prompt
  tokens and steps were 8–11% slower, at the same pass rate. The gap is modest at concurrency 4 because load is low and
  even. `least_loaded` then often picks the run's previous worker anyway (77% of the time). Both workers also hold the
  shared 3.9k prefix from warm-up, so only each run's own history is at stake. We expect the gap to widen under uneven
  or heavier load, which no run has tested. (measured gap; the widening is a guess)

- **F30. The 768-token output cap never bound in the A/B.** All 901 steps across both arms finished with `tool_calls`;
  none with `length`. The ~40 s end-to-end spike seen on vllm-1 (F13) was a long answer that still fit under the cap.
  (measured: `finish_reasons` in `metrics/golden-gw-38-*20261006*.jsonl`)

## 4d. KV-sized admission, re-run (10-07)

Qwen3.8-27B-FP8 on two H100 halves, now with D-43 (gateway in-flight cap 4 per worker, from the measured pool) and
D-44 (refused steps retried after `Retry-After`, up to ~15 s). **The node was an H100 SXM5** ("H100 80GB HBM3"), about
1.7× the memory bandwidth of the 10-06 PCIe node, so every latency and part of the load result below is confounded
with the hardware.

| | 10-06: cap 16, no retries, PCIe | 10-07: cap 4, retries, SXM5 |
|---|---|---|
| c=16 v2 pass [95% CI] | 50.0% [32.1–67.9] | 69.2% [50.0–83.5] |
| c=32 v2 pass [95% CI] | 38.5% [22.4–57.5] | 53.8% [35.5–71.2] |
| Runs that waited out a refusal (c=16 / c=32) | none (no retries) | 10 / 13 |
| Refusals by reason (c=16 / c=32) | `kv_free` 13 / 14 (each ended a run) | `timeout_queue` 42 / 83, `kv_free` 4 / 0 |
| vLLM preemptions, both workers, after c=32 | 6 | 3 |
| Golden at c=4 (×2): v2 pass, step p50 / p95 | 86.5%, 2.9 s / 24.2 s | 88.5%, 1.9 s / 15.7 s |

Evidence: `metrics/golden-sweep-qwen3.8-27b-fp8-c{16,32}-20261007-*.summary.json`,
`metrics/golden-gw-38-kv-20261007-153043.*`, `metrics/{gateway,vllm-0,vllm-1}-sweep-c{16,32}-20261007-151105.prom`,
`.cache/ready.json` for the GPU. The gateway log confirmed `"max_inflight":4`. (measured)

- **F31. Pass rates held up better under load, with the cause not separable from the faster GPU.** Each interval still
  overlaps its 10-06 counterpart. The same-node control in F36 settles the attribution. (measured)
- **F32. Overload moved from inside vLLM to the gateway's queue.** Refusals are now almost all `timeout_queue` (a
  request waited in the gateway's queue past its deadline) instead of `kv_free`, and 10–13 runs per level waited out a
  refusal and finished. That is the intended behaviour. Requests wait in priority order at the gateway instead of
  being preempted in the engine. (measured)
- **F33. Preemptions halved but did not reach 0** (3 against 6, all on vllm-1). This is D-43's revisit trigger: the
  continuing-run exemption (down to 5% free KV) still lets a run's next step in when its worker is nearly full.
  (measured; the cause is the likely explanation, unverified)
- **F34. At 32 concurrent runs, ~15 s of retries is shorter than the queue's wait.** The runs that still failed (4 at
  c=16, 8 at c=32) ran out of retries while requests timed out in the queue. Longer retries or a longer queue deadline
  would trade more latency for more completed runs. (measured)
- **F35. The KV hop fired on real hardware without being forced.** 8 hops completed the Mooncake protocol (the source
  held the KV and the gateway told the destination to pull it), 0 failed, and 26 moves were below the 8,192-token
  threshold. Nobody has confirmed that the destination pulled the KV rather than recomputing it. vLLM 0.29 exposes no
  KV-transfer metric, and the forced-hop run failed because the tunnel was down after the gateway restarted.
  (measured protocol; the transfer itself unverified)

### Same-node control: cap 4 against cap 16 (10-07, both on the SXM5, both with D-44 retries)

| | Cap 4 (D-43) | Cap 16 (old) |
|---|---|---|
| c=16 v2 pass [95% CI] | 69.2% [50.0–83.5] | 84.6% [66.5–93.9] |
| c=32 v2 pass [95% CI] | 53.8% [35.5–71.2] | 61.5% [42.5–77.6] |
| vLLM preemptions added (c=16 / c=32) | 1 / 2 | 9 / 34 |
| Cached share (c=16 / c=32) | 81.3% / 81.6% | 73.3% / 72.7% |
| Step latency p95 (c=16 / c=32) | 21.8 s / 21.7 s | 25.3 s / 26.9 s |
| Refusals | `timeout_queue` 42 / 83 | `kv_free` 58 / 132 |
| Runs that waited out a refusal | 10 / 13 | 22 / 19 |

Evidence: `metrics/golden-sweep-qwen3.8-27b-fp8-c{16,32}-20261007-{151105,151406,160229,160543}.summary.json`,
`metrics/{gateway,vllm-0,vllm-1}-sweep-c{16,32}-20261007-{151105,160229}.prom`; Grafana on 10-07 showed vLLM running up
to 10 requests per worker with up to 10 waiting and KV at 100% during the cap-16 run, against at most 4 under cap 4.
(measured)

- **F36. Sizing admission to KV trades completed runs for engine health.** On the same node, cap 4 cut vLLM
  preemptions by ~95%, kept 8 points more of the prompt cached and lowered tail latency by ~15%. But fewer runs
  finished, because queued requests hit the gateway's queue deadline before the doctor's ~15 s of retries ran out. Under cap
  16 the retries outlasted `kv_free` sheds while vLLM absorbed the overload by preempting, slower and with more
  recomputation, but completing more runs. The pass-rate intervals overlap, so the completion gap is suggestive at 26
  runs per level. F31's improvement over 10-06 came mostly from the retries (D-44) and the faster GPU, not from the cap.
  (measured)
- **F37. Every Qwen3.8 sweep level's rows went to one file, `metrics/golden-sweep-qwen3.jsonl`.** The runner used
  `Path.with_suffix`, which cut the tag at the dot in `qwen3.8`. The rows are all there, appended, but not split by
  level; summaries were unaffected. Fixed in the runner (`result_paths`, with a test). (measured)

## 4e. Autoscaling rehearsal (10-07, kind)

The D-49 ScaledObject, with the pinned KEDA 2.21.0 and Prometheus 29.33.0 charts, ran on the kind lab against a stand-in
`vllm` StatefulSet and a fake gateway `/metrics` whose numbers were set by hand. No model and no GPU were involved, so
these are the scaler's own timings, not a worker's load time.

- **F38. The scaler behaves as designed, in both directions and on both triggers.** Demand of 7 (4 in flight, 3 queued)
  against a cap of 4 asked for 2 workers 142 s after it began: about 80 s for the 2-minute average to pass 4, then the
  60 s scale-up window. Dropping demand to 1 removed the second worker 670 s later (the 10-minute quiet window plus the
  average falling). With demand at 1, capacity sheds at 3 a minute alone added a worker after about 100 s. KEDA read
  both recording rules from the chart's Prometheus service with no extra configuration. On hardware, the model load
  and warm-up add minutes to every scale-up. (measured, kind lab; HPA events at 20:42:54 and 20:54:24 UTC)

- **F39. On the H100 the scaler acts in about a minute, but a worker takes 8 minutes to arrive.** A golden run at
  concurrency 8 on one worker shed `timeout_queue` and `kv_free` at about 0.4 a second. The capacity-shed trigger asked
  for 2 workers about 80 s after the load began (the demand trigger never fired: the cap held in-flight at 4 and the
  queue drained by shedding, so demand sat at the threshold). `vllm-1` was created at 23:30:49 and ready at 23:39:10,
  8 min 21 s later, about 2 minutes after the run had ended. A second run started 9.7 minutes after the first ended;
  KEDA removed `vllm-1` 40 s into it, because the 10-minute window ran out before the new load reached the 2-minute
  averages, then re-created it at 23:47:33. No request failed (vLLM `error` and `abort` both 0), but one worker carried
  the run with KV free under 20%. The window is now 15 minutes (D-51). With it, a third run on both workers shed nothing and
  KEDA removed `vllm-1` at 00:23:54, 15 quiet minutes after that run. The v2 pass rate follows the worker count: 59.6%
  and 57.7% on one worker under load (`as-up`, `as-up2`), 84.6% on two (`as-up3`), near the 86.5% baseline (F1).
  (measured, H100; HPA events, pod timestamps, `metrics/golden-as-*`, `metrics/ts-autoscale-20261008-002807.json`,
  `design/screenshots/`)

## 4f. The full sweep and the Part 5 probes (10-08)

Two sessions ran `make bench` with `REPEAT=2`, so every level ran 52 runs. On the A100 that morning (no H100 was
free), Qwen3-8B-AWQ ran on two HAMi slices. In the afternoon, Qwen3.8-27B-FP8 ran on two halves of an H100 SXM5, with
the cap at 4, the D-44 retries and the KV hop on. Each bench ended with the probes against a golden run at concurrency 4
and `make export`. The power draw shows the H100 was an SXM5, because it reached 600 W and a PCIe H100 stops at 350 W.

| Concurrency | H100, Qwen3.8 v2 pass [95% CI] | Refused (H100) | A100, Qwen3-8B v2 pass | Refused (A100) |
|---|---|---|---|---|
| 1 | 82.7% [70.3–90.6] | 0 | 50.0% | 0 |
| 4 (golden) | 86.5% [74.7–93.3] | 0 | 55.8% | 0 |
| 4 (sweep) | 84.6% [72.5–92.0] | 0 | 51.9% | 0 |
| 8 | 82.7% [70.3–90.6] | `kv_free` 1 | 53.8% | 0 |
| 16 | 63.5% [49.9–75.2] | `timeout_queue` 127, `kv_free` 38 | 48.1% | 0 |
| 32 | 23.1% [13.7–36.1] | `timeout_queue` 220, `kv_free` 33 | 51.9% | `timeout_queue` 100, `kv_free` 75 |
| 4, with probes | 75.0% [61.8–84.8] | `kv_free` 7, two runs ended on a 502 | 51.9% | two runs ended on a 502 |

Evidence: `metrics/golden-q1-h100-20261008-114653.*`, `metrics/golden-sweep-qwen3.8-27b-fp8-c*-20261008-*.summary.json`,
`metrics/golden-q1-h100-probes-20261008-123332.*`, `metrics/golden-q1-20261008-093450.*`,
`metrics/golden-sweep-qwen3-8b-awq-c*-20261008-*.summary.json`, `metrics/golden-q1-probes-20261008-101639.*`, the
probe times in `metrics/events-q1-h100-probes-20261008-123331.jsonl` and `metrics/events-q1-probes-20261008-101639.jsonl`,
and the time series in `metrics/ts-q1-h100-20261008-131201.json` and `metrics/ts-q1-20261008-102520.json`. (measured)

- **F40. On the H100 the knee is still between 8 and 16, and 32 real concurrent runs fail most of the time.** The pass
  rate holds at 83–87% up to 8 and falls to 63.5% at 16 and 23.1% at 32. The 10-07 sweep scored 53.8% at 32, but it ran
  26 runs per level, so it never had more than 26 in flight. With 52 runs, 32 are in flight at once, and most refusals
  are `timeout_queue`, whose wait outlasts the doctor's ~15 s of retries (F34). The intervals barely overlap (13.7–36.1
  against 35.5–71.2). The run count is the likely cause, unverified. The 8B's pass rate stays at 48–56% at every level,
  even with 175 refusals at 32, so its limit is the model, not capacity (F1). Its cached share stayed at 94–95%.
  (measured)
- **F41. The overload waits in the gateway's queue, not in vLLM's.** Across the H100 bench, the gateway queue held
  requests in 104 of about 2,000 five-second samples (230 request-samples in all). vLLM's waiting queue was above 0 in
  29 (30 request-samples), never more than 2. vLLM preempted 8 requests over the whole sweep, 1 on vllm-0 and 7 on
  vllm-1. That confirms F32 at a real 32 concurrent runs. (measured, `gw_queue_depth` and `vllm_waiting` in the export,
  `metrics/vllm-{0,1}-sweep-c32-20261008-115608.prom`)
- **F42. A 10.5k-token batch prompt doesn't slow the agents' decode at p95.** Three prompts of 10,512–10,525 tokens
  (the probe estimated 13,000) each returned 200 in 2.1–2.5 s. Inter-token latency p95 stayed at 49 ms on both workers
  in the 45 s after each prompt, as in the minute before. The tokens-per-step p95 didn't move either. One prompt
  adds two large prefill steps among many small decode steps, so a p95 can't show it (likely, unverified). That is
  also why the Grafana "tokens per engine step" panel looks empty. (measured)
- **F43. Nothing confirms that vLLM frees the KV of a client that leaves.** Each of the three clients left after
  3.0 s, on both GPUs. vLLM's `request_success_total{finished_reason="abort"}` stayed at 0 on both workers throughout
  both sessions. Either the cancellation doesn't reach vLLM, or vLLM 0.29 doesn't count a disconnect as an abort.
  Neither is checked. The gateway logs `client_gone` but has no metric for it, so the only record is its log, which
  `make down` deletes. (measured counter; the cause unverified)
- **F44. A deleted worker came back in 5 min 37 s, and its in-flight requests failed.** The probe deleted `vllm-1` at
  12:38:33, the gateway marked it down at 12:38:45, and it was warming at 12:44:08 and ready at 12:44:10. The two
  warm-up probes took 2 s, and the warm-up TTFT the gateway re-quotes fell from 63 ms to 56 ms (observed). That is 2 min
  44 s faster than the KEDA scale-up (F39), likely because the restarted pod reused the node's cached weights and
  compile cache (unverified). `vllm-0` took every placement while it was gone, peaking at about 1.7 a second. On both
  GPUs two runs ended on an HTTP 502, likely the steps in flight on the deleted worker. The doctor retried a 503 but not
  a 502, so each one cost a run. It now retries a 502 `upstream_error` too (D-44, amended 10-08). The golden run had ended before the worker returned, so no traffic reached it
  afterwards, and the ramp after a return is still unmeasured. (measured times; the 502s' cause unverified)
- **F45. The KV hop fired 45 times on the H100, with none failed.** From the golden run to the end of the sweep, 68
  more moves were below the 8,192-token threshold. The destination's pull is still unconfirmed (F35). (measured
  protocol, `metrics/gateway-q1-h100-20261008-123330.prom`)
- **F46. On the dashboards, power is the H100's load signal, not utilisation.** DCGM's GPU utilisation read 100% from
  the first single run to the end of the sweep. It counts the time any kernel is running, not how busy the GPU is.
  Power rose from about 450 W with 1 to 8 runs to about 600 W at 16 and 32, and fell to about 110 W idle. Five panels
  need fixing, and they are in `design/backlog.md`. The worker phase panel shows its threshold labels ("2+", "<1")
  instead of down, warming and ready. Container restarts shows fractions. HAMi slice memory and SM activity have no
  data. "KV hops by result" plots the hop p95 in seconds on its req/s axis. (observed, `design/screenshots/q1-h100-*`)
- **F47. The shared prefix grew to about 3.9k tokens.** On the H100 golden run, a run's first call found a median of
  3,920 tokens cached (49 of 52 runs), counted in whole KV blocks, out of a first prompt of 4,734. F8's 3,787 predates
  D-41's longer ruleset and tool schemas. (measured, `metrics/golden-q1-h100-20261008-114653.jsonl`)

## 5. Infrastructure and operations

- **F18. GPU fallback works on real hardware.** `make up` skipped GH200 (no capacity), took an H100 PCIe in us-west-3
  on 10-06, and cloud-init detected it and booted Qwen3.8. The H100 reported 81,559 MiB, exactly what `serving.json`
  assumed. (measured, `.cache/ready.json`)
- **F19. vLLM v0.30.0-cu129 crash-loops on start.** The image ships torch cu130 with a cu129 torchvision
  (vllm-project/vllm#59157). We stay on v0.29.0. (measured on 10-04)
- **F20. A slow Lambda boot once produced three billed `cluster-doctor` nodes**, because `make up` retried a launch
  that had created the instance. `make up` now refuses while one exists and stops after a launch that leaves one
  behind, and `make resume` finishes a slow boot. (observed on 10-06)
- **F21. Lambda can hold a node in `booting` for over 15 minutes** before it becomes active. (observed on 10-06)
- **F22. Superlinked overflow is blocked on their billing.** Their API serves `Qwen/Qwen3.8-27B-FP8`, the same model,
  but returns HTTP 402 `INSUFFICIENT_CREDITS` for every operation while the console shows a $520 grant; an EU-pinned
  key has no reachable EU endpoint. (measured on 10-04; reported to their support)
- **F23. The KV hop's copy bandwidth between two HAMi halves is unmeasured,** and so is whether Qwen3.8's hybrid state
  passes through the connector. F35 shows the protocol working; test plan H1–H3 covers the rest. (status)

## Still to measure

- The tuned cap (backlog): somewhere between 4 and 16, or cap 4 with a longer queue deadline, against F36's table and
  now F40's 52-run levels.
- Whether a hop's destination really pulls the KV: its `cached_tokens` on the hopped step (gateway log `hop`, the
  step's `cached_tokens`), since vLLM 0.29 has no transfer metric (F35, F45).
- Whether vLLM aborts a request when its client leaves (F43). That needs a gateway counter for `client_gone`, and
  vLLM's running count and KV usage in the seconds after a probe.
- A returning worker under load (F44). The background load has to outlast the worker's 5.5-minute return, for
  example a golden run with `REPEAT=4`.
- Whether a scale-down cuts off requests in flight on the removed worker (D-49's revisit trigger). The `as-down` run
  in F39 removed `vllm-1` with no upstream errors, but at one run at a time.
