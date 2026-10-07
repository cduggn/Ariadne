# Model matrix

`make matrix` (serving/matrix.py) generates this file from `deploy/models/*.json`, `deploy/serving.json` and `metrics/`, and CI fails if it is stale. Don't edit it by hand. To change a number, rerun whatever produced it. D-40 covers the profiles, topologies and this matrix, and D-41 covers the v2 score. `design/model-architecture-guide.md` explains the architecture.

The GPUs are the A100 40 GB SXM4 (a100-40), H100 80 GB PCIe (h100-80), GH200 96 GB (arm64 host) (gh200-96). Every model runs with the same engine settings: `vllm-openai:v0.29.0-cu129`, context 24,576, at most 32 sequences, 8,192 batched tokens, prefix caching, 0.9 of the memory HAMi exposes, and 16-bit KV. Sampling follows each model's card, as set in the profile's `client` block.

Each topology names its GPU:
- **sliced** (a100-40), N workers on A100 HAMi slices of 20 GiB and half the SMs each: GPU slicing, routing, affinity and the KV hop between workers.
- **full** (a100-40), one worker with the whole A100: the model-quality benchmark, and the larger models that do not fit a slice.
- **h100-full** (h100-80), one worker with the whole H100 (78 GiB requested, below the card's reported size): the newer FP8 models.
- **h100-half** (h100-80), two workers on H100 halves of 39 GiB: the gateway demo with a 27B model.
- **gh200-full** (gh200-96), one worker with the whole GH200 (92 GiB requested): the newer FP8 models.
- **gh200-half** (gh200-96), two workers on GH200 halves of 46 GiB; HAMi on GH200 is unverified, so this is an experiment.

## 1. Where each model can run

`serving/fit.py` computes the paper numbers, and the measured tokens come from vLLM's own startup report (`make kv`). Sequences at 24k are full-length requests that fit at once. At 12k, each worker caches the 3,787-token shared prefix once. Prefill is the uncached time for 12k tokens on the topology's share of the SMs. Hybrid models also keep a recurrent state per sequence, which is an estimate.

| Model | Plan | Topology | Weights GiB | KV/token KiB | State/seq MiB | KV pool GiB | Tokens (paper) | Tokens (measured) | Seqs @24k | Seqs @12k, prefix shared | Prefill 12k s | Verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| qwen3-8b-awq | baseline | sliced | 5.7 | 144 | 0.0 | 10.9 | 79,699 | 79,056 (-0.8%) | 3.2 | 9.2 | 3.15 | fits |
| qwen3-8b-awq | baseline | full | 5.7 | 144 | 0.0 | 28.9 | 210,771 | n/a | 8.6 | 25.2 | 1.57 | fits |
| qwen3-8b-awq | baseline | h100-full | 5.7 | 144 | 0.0 | 63.1 | 459,807 | n/a | 18.7 | 55.5 | 0.65 | fits |
| qwen3-8b-awq | baseline | h100-half | 5.7 | 144 | 0.0 | 28.0 | 204,217 | n/a | 8.3 | 24.4 | 1.30 | fits |
| qwen3-8b-awq | baseline | gh200-full | 5.7 | 144 | 0.0 | 75.7 | 551,558 | n/a | 22.4 | 66.7 | 0.50 | fits |
| qwen3-8b-awq | baseline | gh200-half | 5.7 | 144 | 0.0 | 34.3 | 250,092 | n/a | 10.2 | 30.0 | 0.99 | fits |
| qwen3-14b-awq | round-1 | sliced | 9.3 | 160 | 0.0 | 7.1 | 46,637 | n/a | 1.9 | 5.2 | 5.68 | fits, tight: 1.9 requests of 24,576 tokens (< 2) |
| qwen3-14b-awq | round-1 | full | 9.3 | 160 | 0.0 | 25.1 | 164,601 | n/a | 6.7 | 19.6 | 2.84 | fits |
| qwen3-14b-awq | round-1 | h100-full | 9.3 | 160 | 0.0 | 59.3 | 388,734 | n/a | 15.8 | 46.9 | 1.17 | fits |
| qwen3-14b-awq | round-1 | h100-half | 9.3 | 160 | 0.0 | 24.2 | 158,703 | n/a | 6.5 | 18.9 | 2.34 | fits |
| qwen3-14b-awq | round-1 | gh200-full | 9.3 | 160 | 0.0 | 71.9 | 471,310 | n/a | 19.2 | 56.9 | 0.90 | fits |
| qwen3-14b-awq | round-1 | gh200-half | 9.3 | 160 | 0.0 | 30.5 | 199,991 | n/a | 8.1 | 23.9 | 1.79 | fits |
| qwen3-30b-a3b-2507-awq | round-1 | sliced | 16.9 | 96 | 0.0 | 0.1 | 955 | n/a | 0.0 | 0.0 | 1.27 | below the gate: 0.0 < 1 requests of 24,576 tokens |
| qwen3-30b-a3b-2507-awq | round-1 | full | 16.9 | 96 | 0.0 | 18.1 | 197,563 | 185,136 (-6.3%) | 7.5 | 22.1 | 0.63 | fits |
| qwen3-30b-a3b-2507-awq | round-1 | h100-full | 16.9 | 96 | 0.0 | 52.3 | 571,118 | n/a | 23.2 | 69.1 | 0.26 | fits |
| qwen3-30b-a3b-2507-awq | round-1 | h100-half | 16.9 | 96 | 0.0 | 17.2 | 187,733 | n/a | 7.6 | 22.4 | 0.52 | fits |
| qwen3-30b-a3b-2507-awq | round-1 | gh200-full | 16.9 | 96 | 0.0 | 64.9 | 708,744 | n/a | 28.8 | 85.8 | 0.20 | fits |
| qwen3-30b-a3b-2507-awq | round-1 | gh200-half | 16.9 | 96 | 0.0 | 23.5 | 256,546 | n/a | 10.4 | 30.8 | 0.40 | fits |
| gemma-4-31b-it-fp8 | round-2 | sliced | 31.0 | 80 | 800.0 | -14.7 | 0 | n/a | 0.0 | 0.0 | 12.03 | does not fit: weights + overhead exceed the budget |
| gemma-4-31b-it-fp8 | round-2 | full | 31.0 | 80 | 800.0 | 3.3 | 43,065 | n/a | 1.2 | 2.1 | 6.01 | fits, tight: 1.2 requests of 24,576 tokens (< 2) |
| gemma-4-31b-it-fp8 | round-2 | h100-full | 31.0 | 80 | 800.0 | 37.5 | 491,331 | n/a | 14.1 | 26.4 | 2.48 | fits |
| gemma-4-31b-it-fp8 | round-2 | h100-half | 31.0 | 80 | 800.0 | 2.4 | 31,268 | n/a | 0.9 | 1.5 | 4.96 | below the gate: 0.9 < 1 requests of 24,576 tokens |
| gemma-4-31b-it-fp8 | round-2 | gh200-full | 31.0 | 80 | 800.0 | 50.1 | 656,482 | n/a | 18.9 | 35.4 | 1.90 | fits |
| gemma-4-31b-it-fp8 | round-2 | gh200-half | 31.0 | 80 | 800.0 | 8.7 | 113,844 | n/a | 3.3 | 6.0 | 3.79 | fits |
| qwen3.5-9b | round-2 | sliced | 18.0 | 32 | 49.1 | -1.4 | 0 | n/a | 0.0 | 0.0 | 3.71 | does not fit: weights + overhead exceed the budget |
| qwen3.5-9b | round-2 | full | 18.0 | 32 | 49.1 | 16.6 | 545,423 | n/a | 20.9 | 55.4 | 1.86 | fits |
| qwen3.5-9b | round-2 | h100-full | 18.0 | 32 | 49.1 | 50.8 | 1,666,088 | n/a | 63.7 | 169.9 | 0.77 | fits |
| qwen3.5-9b | round-2 | h100-half | 18.0 | 32 | 49.1 | 15.7 | 515,932 | n/a | 19.7 | 52.3 | 1.53 | fits |
| qwen3.5-9b | round-2 | gh200-full | 18.0 | 32 | 49.1 | 63.4 | 2,078,965 | n/a | 79.5 | 212.1 | 0.59 | fits |
| qwen3.5-9b | round-2 | gh200-half | 18.0 | 32 | 49.1 | 22.0 | 722,370 | n/a | 27.6 | 73.4 | 1.17 | fits |
| qwen3.8-27b-fp8 | round-2 | sliced | 28.8 | 64 | 146.8 | -12.3 | 0 | n/a | 0.0 | 0.0 | 10.68 | does not fit: weights + overhead exceed the budget |
| qwen3.8-27b-fp8 | round-2 | full | 28.8 | 64 | 146.8 | 5.7 | 92,672 | n/a | 3.4 | 8.4 | 5.34 | fits |
| qwen3.8-27b-fp8 | round-2 | h100-full | 28.8 | 64 | 146.8 | 39.9 | 653,004 | 525,797 (-19.5%) | 19.5 | 49.4 | 2.20 | fits |
| qwen3.8-27b-fp8 | round-2 | h100-half | 28.8 | 64 | 146.8 | 4.8 | 77,926 | 51,092 (-34.4%) | 1.9 | 4.5 | 4.41 | fits, tight: 1.9 requests of 24,576 tokens (< 2) |
| qwen3.8-27b-fp8 | round-2 | gh200-full | 28.8 | 64 | 146.8 | 52.5 | 859,443 | n/a | 31.9 | 81.0 | 1.69 | fits |
| qwen3.8-27b-fp8 | round-2 | gh200-half | 28.8 | 64 | 146.8 | 11.1 | 181,145 | n/a | 6.7 | 16.8 | 3.37 | fits |
| ministral-3-14b | paper-only | sliced | 26.0 | 160 | 0.0 | -9.5 | 0 | n/a | 0.0 | 0.0 | 5.37 | does not fit: weights + overhead exceed the budget |
| ministral-3-14b | paper-only | full | 26.0 | 160 | 0.0 | 8.5 | 55,492 | n/a | 2.3 | 6.3 | 2.68 | fits |
| ministral-3-14b | paper-only | h100-full | 26.0 | 160 | 0.0 | 42.7 | 279,625 | n/a | 11.4 | 33.6 | 1.11 | fits |
| ministral-3-14b | paper-only | h100-half | 26.0 | 160 | 0.0 | 7.6 | 49,594 | n/a | 2.0 | 5.6 | 2.21 | fits |
| ministral-3-14b | paper-only | gh200-full | 26.0 | 160 | 0.0 | 55.3 | 362,201 | n/a | 14.7 | 43.6 | 0.85 | fits |
| ministral-3-14b | paper-only | gh200-half | 26.0 | 160 | 0.0 | 13.9 | 90,882 | n/a | 3.7 | 10.6 | 1.69 | fits |
| qwen3-coder-30b-a3b-awq | paper-only | sliced | 16.9 | 96 | 0.0 | 0.1 | 955 | n/a | 0.0 | 0.0 | 1.27 | below the gate: 0.0 < 1 requests of 24,576 tokens |
| qwen3-coder-30b-a3b-awq | paper-only | full | 16.9 | 96 | 0.0 | 18.1 | 197,563 | n/a | 8.0 | 23.6 | 0.63 | fits |
| qwen3-coder-30b-a3b-awq | paper-only | h100-full | 16.9 | 96 | 0.0 | 52.3 | 571,118 | n/a | 23.2 | 69.1 | 0.26 | fits |
| qwen3-coder-30b-a3b-awq | paper-only | h100-half | 16.9 | 96 | 0.0 | 17.2 | 187,733 | n/a | 7.6 | 22.4 | 0.52 | fits |
| qwen3-coder-30b-a3b-awq | paper-only | gh200-full | 16.9 | 96 | 0.0 | 64.9 | 708,744 | n/a | 28.8 | 85.8 | 0.20 | fits |
| qwen3-coder-30b-a3b-awq | paper-only | gh200-half | 16.9 | 96 | 0.0 | 23.5 | 256,546 | n/a | 10.4 | 30.8 | 0.40 | fits |
| qwen3.6-35b-a3b-awq | paper-only | sliced | 23.2 | 20 | 61.4 | -6.4 | 0 | n/a | 0.0 | 0.0 | 1.15 | does not fit: weights + overhead exceed the budget |
| qwen3.6-35b-a3b-awq | paper-only | full | 23.2 | 20 | 61.4 | 11.6 | 609,484 | n/a | 22.0 | 53.3 | 0.58 | fits |
| qwen3.6-35b-a3b-awq | paper-only | h100-full | 23.2 | 20 | 61.4 | 45.8 | 2,402,549 | n/a | 86.7 | 211.2 | 0.24 | fits |
| qwen3.6-35b-a3b-awq | paper-only | h100-half | 23.2 | 20 | 61.4 | 10.7 | 562,298 | n/a | 20.3 | 49.2 | 0.48 | fits |
| qwen3.6-35b-a3b-awq | paper-only | gh200-full | 23.2 | 20 | 61.4 | 58.4 | 3,063,152 | n/a | 110.5 | 269.4 | 0.18 | fits |
| qwen3.6-35b-a3b-awq | paper-only | gh200-half | 23.2 | 20 | 61.4 | 17.0 | 892,600 | n/a | 32.2 | 78.3 | 0.36 | fits |

## 2. Results on the golden set (26 tasks)

Each row is the newest full-set run for that model, topology and worker count. v1 is the original score, kept for continuity. v2 is the corrected score (D-41): it counts only evidence the model was shown, accepts equally supported categories, checks that the mechanism is stated, and treats required tools as advisory. The tier, root-found, category and mechanism columns are v2 rates over all task runs.

| Model | Topology | Workers | Tasks × repeat | v2 pass [95 % CI] | v1 pass | easy | multi-hop | red-herring | rightsizing | Root found | Category | Mechanism | Abstained / failed closed | Steps / task | Step p50 / p95 s | Cached share | Correct / GPU-hour | Concurrency | Run |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| qwen3-30b-a3b-2507-awq | full | 1 | 26 × 2 | 67% [54%–78%] | 65% | 75% | 50% | 75% | 50% | 77% | 73% | 77% | 0 / 1 | 9.73 | 0.444 / 2.502 | 97% | 326 | 1 | 30b-smoke 20260928-164635 8c0ebec |
| qwen3.8-27b-fp8 | h100-half | 2 | 26 × 1 | 62% [42%–78%] | 54% | 79% | 57% | 25% | 0% | 65% | 65% | 65% | 0 / 2 | 7.42 | 5.051 / 26.93 | 73% | 322 | 32 | sweep-qwen3.8-27b-fp8-c32 20261007-160543 d2eb23a |
| qwen3-8b-awq | sliced | 1 | 26 × 2 | 52% [39%–65%] | 44% | 68% | 43% | 25% | 0% | 62% | 62% | 58% | 0 / 4 | 10.98 | 0.436 / 6.481 | 95% | 267 | 1 | baseline 20260928-161531 8c0ebec |
| qwen3-8b-awq | sliced | 2 | 26 × 2 | 48% [35%–61%] | 42% | 61% | 43% | 25% | 0% | 60% | 58% | 54% | 0 / 4 | 10.85 | 1.064 / 14.648 | 95% | 270 | 8 | gw-ptl 20261004-162326 900642d |

## 3. Ranking and status

Models are ranked by v2 pass rate, then by correct diagnoses per GPU-hour. Quality is compared on the full-GPU topology, because slicing changes capacity and latency but not the answers. The chosen model then serves the slicing, routing and KV-hop demonstration on the sliced topology.

1. **qwen3-30b-a3b-2507-awq** on full × 1: v2 67% [54%–78%], 326 correct diagnoses per GPU-hour. Not yet distinguishable from the next model, because the intervals overlap
2. **qwen3.8-27b-fp8** on h100-half × 2: v2 62% [42%–78%], 322 correct diagnoses per GPU-hour. Not yet distinguishable from the next model, because the intervals overlap
3. **qwen3-8b-awq** on sliced × 1: v2 52% [39%–65%], 267 correct diagnoses per GPU-hour. Not yet distinguishable from the next model, because the intervals overlap
4. **qwen3-8b-awq** on sliced × 2: v2 48% [35%–61%], 270 correct diagnoses per GPU-hour
- Not run: **qwen3-14b-awq**, plan round-1, scheduled for the next GPU session. Fit: sliced: fits, tight: 1.9 requests of 24,576 tokens (< 2); full: fits; h100-full: fits; h100-half: fits; gh200-full: fits; gh200-half: fits.
- Not run: **gemma-4-31b-it-fp8**, plan round-2, to run if time allows after round 1. Fit: sliced: does not fit: weights + overhead exceed the budget; full: fits, tight: 1.2 requests of 24,576 tokens (< 2); h100-full: fits; h100-half: below the gate: 0.9 < 1 requests of 24,576 tokens; gh200-full: fits; gh200-half: fits.
- Not run: **qwen3.5-9b**, plan round-2, to run if time allows after round 1. Fit: sliced: does not fit: weights + overhead exceed the budget; full: fits; h100-full: fits; h100-half: fits; gh200-full: fits; gh200-half: fits.
- Not run: **ministral-3-14b**, plan paper-only, not scheduled. Fit: sliced: does not fit: weights + overhead exceed the budget; full: fits; h100-full: fits; h100-half: fits; gh200-full: fits; gh200-half: fits.
- Not run: **qwen3-coder-30b-a3b-awq**, plan paper-only, not scheduled. Fit: sliced: below the gate: 0.0 < 1 requests of 24,576 tokens; full: fits; h100-full: fits; h100-half: fits; gh200-full: fits; gh200-half: fits.
- Not run: **qwen3.6-35b-a3b-awq**, plan paper-only, not scheduled. Fit: sliced: does not fit: weights + overhead exceed the budget; full: fits; h100-full: fits; h100-half: fits; gh200-full: fits; gh200-half: fits.

## 4. Every run on file

| When | Tag | Model | Topology | Tasks (unique) | Repeat | v1 | v2 | Stops | Commit | Note |
|---|---|---|---|---|---|---|---|---|---|---|
| 20261007-160543 | sweep-qwen3.8-27b-fp8-c32 | qwen3.8-27b-fp8 | h100-half | 26 | 1 | 14/26 | 16/26 | http_503 7, submitted 17, inconclusive 2 | d2eb23a | full set |
| 20261007-160229 | sweep-qwen3.8-27b-fp8-c16 | qwen3.8-27b-fp8 | h100-half | 26 | 1 | 20/26 | 22/26 | submitted 23, http_503 1, inconclusive 2 | d2eb23a | full set |
| 20261007-153043 | gw-38-kv | qwen3.8-27b-fp8 | h100-half | 26 | 2 | 41/52 | 46/52 | submitted 47, inconclusive 4, step_cap 1 | d27f078 | full set |
| 20261007-151406 | sweep-qwen3.8-27b-fp8-c32 | qwen3.8-27b-fp8 | h100-half | 26 | 1 | 13/26 | 14/26 | http_503 8, submitted 14, inconclusive 4 | d27f078 | full set |
| 20261007-151105 | sweep-qwen3.8-27b-fp8-c16 | qwen3.8-27b-fp8 | h100-half | 26 | 1 | 16/26 | 18/26 | http_503 4, submitted 18, inconclusive 4 | d27f078 | full set |
| 20261006-140918 | gw-38-ll | qwen3.8-27b-fp8 | h100-half | 26 | 2 | 40/52 | 46/52 | submitted 47, inconclusive 5 | 00fab90 | full set |
| 20261006-135709 | sweep-qwen3.8-27b-fp8-c32 | qwen3.8-27b-fp8 | h100-half | 26 | 1 | 8/26 | 10/26 | submitted 11, http_503 14, inconclusive 1 | 963e5fb | full set |
| 20261006-135533 | sweep-qwen3.8-27b-fp8-c16 | qwen3.8-27b-fp8 | h100-half | 26 | 1 | 11/26 | 13/26 | http_503 13, submitted 13 | 963e5fb | full set |
| 20261006-135139 | sweep-qwen3.8-27b-fp8-c8 | qwen3.8-27b-fp8 | h100-half | 26 | 1 | 20/26 | 23/26 | submitted 23, inconclusive 2, http_503 1 | 963e5fb | full set |
| 20261006-131640 | gw-38 | qwen3.8-27b-fp8 | h100-half | 26 | 2 | 42/52 | 45/52 | submitted 46, inconclusive 6 | 8d4fe00 | full set |
| 20261004-162326 | gw-ptl | qwen3-8b-awq | sliced | 26 | 2 | 22/52 | 25/52 | submitted 44, inconclusive 4, context_budget 3, step_cap 1 | 900642d | full set |
| 20260928-164635 | 30b-smoke | qwen3-30b-a3b-2507-awq | full | 26 | 2 | 34/52 | 35/52 | submitted 50, inconclusive 1, step_cap 1 | 8c0ebec | full set |
| 20260928-161531 | baseline | qwen3-8b-awq | sliced | 26 | 2 | 23/52 | 27/52 | submitted 43, inconclusive 4, context_budget 4, step_cap 1 | 8c0ebec | full set |
| 20260927-220614 | baseline | qwen3-8b-awq | sliced | 1 | 2 | 0/2 | n/a | step_cap 2 | n/a | partial: subset; profile/topology assumed (pre D-40) |

One result isn't in these files. The first full baseline (2026-09-27, 8B, sliced) scored 9/26 on v1 in its first pass, but the crash fixed in D-39 lost its run file. Only D-39 records it.

## 5. Adding a model

1. Write `deploy/models/<name>.json` with the pinned revision, the architecture from its `config.json`, the vLLM args and the sampling from its model card.
2. Run `make fit MODEL=<name> TOPO=full`, and `TOPO=sliced`, to see the paper fit and the gate.
3. On the GPU, run `make deploy MODEL=<name> TOPO=<topology>`, then `make kv MODEL=… TOPO=…`, then `make golden MODEL=… TOPO=… TAG=<name> REPEAT=3 CONC=4`.
4. Run `make matrix` and commit `metrics/` and this file.
