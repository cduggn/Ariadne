# Model matrix

`make matrix` (serving/matrix.py) generates this file from `deploy/models/*.json`, `deploy/serving.json` and `metrics/`, and CI fails if it is stale. Don't edit it by hand. To change a number, rerun whatever produced it. D-40 covers the profiles, topologies and this matrix, and D-41 covers the v2 score. The review behind both is `design/model-and-golden-review-2026-09-28.md`, and `design/model-architecture-guide.md` explains the architecture.

The GPU is an A100 40 GB (Lambda gpu_1x_a100). Every model runs with the same engine settings: `vllm-openai:v0.29.0-cu129`, context 24,576, at most 32 sequences, 8,192 batched tokens, prefix caching, 0.9 of the memory HAMi exposes, and 16-bit KV. Sampling follows each model's card, as set in the profile's `client` block.

The two topologies are:
- **sliced**, N workers on HAMi slices of 20 GiB and half the SMs each: GPU slicing, routing, affinity and the KV hop between workers.
- **full**, one worker with the whole card: the model-quality benchmark, and the larger models that do not fit a slice.

## 1. Where each model can run

`serving/fit.py` computes the paper numbers, and the measured tokens come from vLLM's own startup report (`make kv`). Sequences at 24k are full-length requests that fit at once. At 12k, each worker caches the 3,787-token shared prefix once. Prefill is the uncached time for 12k tokens on the topology's share of the SMs. Hybrid models also keep a recurrent state per sequence, which is an estimate.

| Model | Plan | Topology | Weights GiB | KV/token KiB | State/seq MiB | KV pool GiB | Tokens (paper) | Tokens (measured) | Seqs @24k | Seqs @12k, prefix shared | Prefill 12k s | Verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| qwen3-8b-awq | baseline | sliced | 5.7 | 144 | 0.0 | 10.9 | 79,699 | 79,056 (-0.8%) | 3.2 | 9.2 | 3.15 | fits |
| qwen3-8b-awq | baseline | full | 5.7 | 144 | 0.0 | 28.9 | 210,771 | n/a | 8.6 | 25.2 | 1.57 | fits |
| qwen3-14b-awq | round-1 | sliced | 9.3 | 160 | 0.0 | 7.1 | 46,637 | n/a | 1.9 | 5.2 | 5.68 | fits, tight: 1.9 requests of 24,576 tokens (< 2) |
| qwen3-14b-awq | round-1 | full | 9.3 | 160 | 0.0 | 25.1 | 164,601 | n/a | 6.7 | 19.6 | 2.84 | fits |
| qwen3-30b-a3b-2507-awq | round-1 | sliced | 16.9 | 96 | 0.0 | 0.1 | 955 | n/a | 0.0 | 0.0 | 1.27 | below the gate: 0.0 < 1 requests of 24,576 tokens |
| qwen3-30b-a3b-2507-awq | round-1 | full | 16.9 | 96 | 0.0 | 18.1 | 197,563 | 185,136 (-6.3%) | 7.5 | 22.1 | 0.63 | fits |
| qwen3.5-9b | round-2 | sliced | 18.0 | 32 | 49.1 | -1.4 | 0 | n/a | 0.0 | 0.0 | 3.71 | does not fit: weights + overhead exceed the budget |
| qwen3.5-9b | round-2 | full | 18.0 | 32 | 49.1 | 16.6 | 545,423 | n/a | 20.9 | 55.4 | 1.86 | fits |
| ministral-3-14b | paper-only | sliced | 26.0 | 160 | 0.0 | -9.5 | 0 | n/a | 0.0 | 0.0 | 5.37 | does not fit: weights + overhead exceed the budget |
| ministral-3-14b | paper-only | full | 26.0 | 160 | 0.0 | 8.5 | 55,492 | n/a | 2.3 | 6.3 | 2.68 | fits |
| qwen3-coder-30b-a3b-awq | paper-only | sliced | 16.9 | 96 | 0.0 | 0.1 | 955 | n/a | 0.0 | 0.0 | 1.27 | below the gate: 0.0 < 1 requests of 24,576 tokens |
| qwen3-coder-30b-a3b-awq | paper-only | full | 16.9 | 96 | 0.0 | 18.1 | 197,563 | n/a | 8.0 | 23.6 | 0.63 | fits |
| qwen3.6-35b-a3b-awq | paper-only | sliced | 23.2 | 20 | 61.4 | -6.4 | 0 | n/a | 0.0 | 0.0 | 1.15 | does not fit: weights + overhead exceed the budget |
| qwen3.6-35b-a3b-awq | paper-only | full | 23.2 | 20 | 61.4 | 11.6 | 609,484 | n/a | 22.0 | 53.3 | 0.58 | fits |

## 2. Results on the golden set (26 tasks)

Each row is the newest full-set run for that model, topology and worker count. v1 is the original score, kept for continuity. v2 is the corrected score (D-41): it counts only evidence the model was shown, accepts equally supported categories, checks that the mechanism is stated, and treats required tools as advisory. The tier, root-found, category and mechanism columns are v2 rates over all task runs.

| Model | Topology | Workers | Tasks × repeat | v2 pass [95 % CI] | v1 pass | easy | multi-hop | red-herring | rightsizing | Root found | Category | Mechanism | Abstained / failed closed | Steps / task | Step p50 / p95 s | Cached share | Correct / GPU-hour | Concurrency | Run |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| qwen3-30b-a3b-2507-awq | full | 1 | 26 × 2 | 67% [54%–78%] | 65% | 75% | 50% | 75% | 50% | 77% | 73% | 77% | 0 / 1 | 9.73 | 0.444 / 2.502 | 97% | 326 | 1 | 30b-smoke 20260928-164635 8c0ebec |
| qwen3-8b-awq | sliced | 1 | 26 × 2 | 52% [39%–65%] | 44% | 68% | 43% | 25% | 0% | 62% | 62% | 58% | 0 / 4 | 10.98 | 0.436 / 6.481 | 95% | 267 | 1 | baseline 20260928-161531 8c0ebec |

## 3. Ranking and status

Models are ranked by v2 pass rate, then by correct diagnoses per GPU-hour. Quality is compared on the full-GPU topology, because slicing changes capacity and latency but not the answers. The chosen model then serves the slicing, routing and KV-hop demonstration on the sliced topology.

1. **qwen3-30b-a3b-2507-awq** on full × 1: v2 67% [54%–78%], 326 correct diagnoses per GPU-hour. Not yet distinguishable from the next model, because the intervals overlap
2. **qwen3-8b-awq** on sliced × 1: v2 52% [39%–65%], 267 correct diagnoses per GPU-hour
- Not run: **qwen3-14b-awq**, plan round-1, scheduled for the next GPU session. Fit: sliced: fits, tight: 1.9 requests of 24,576 tokens (< 2); full: fits.
- Not run: **qwen3.5-9b**, plan round-2, to run if time allows after round 1. Fit: sliced: does not fit: weights + overhead exceed the budget; full: fits.
- Not run: **ministral-3-14b**, plan paper-only, not scheduled. Fit: sliced: does not fit: weights + overhead exceed the budget; full: fits.
- Not run: **qwen3-coder-30b-a3b-awq**, plan paper-only, not scheduled. Fit: sliced: below the gate: 0.0 < 1 requests of 24,576 tokens; full: fits.
- Not run: **qwen3.6-35b-a3b-awq**, plan paper-only, not scheduled. Fit: sliced: does not fit: weights + overhead exceed the budget; full: fits.

## 4. Every run on file

| When | Tag | Model | Topology | Tasks (unique) | Repeat | v1 | v2 | Stops | Commit | Note |
|---|---|---|---|---|---|---|---|---|---|---|
| 20260928-164635 | 30b-smoke | qwen3-30b-a3b-2507-awq | full | 26 | 2 | 34/52 | 35/52 | submitted 50, inconclusive 1, step_cap 1 | 8c0ebec | full set |
| 20260928-164539 | 30b-smoke | qwen3-30b-a3b-2507-awq | full | 2 | 1 | 1/2 | 1/2 | submitted 2 | 8c0ebec | partial: dx-crashloop,dx-port-mismatch |
| 20260928-161531 | baseline | qwen3-8b-awq | sliced | 26 | 2 | 23/52 | 27/52 | submitted 43, inconclusive 4, context_budget 4, step_cap 1 | 8c0ebec | full set |
| 20260928-161019 | baseline | qwen3-8b-awq | sliced | 1 | 2 | 0/2 | 0/2 | transport_error 2 | 8c0ebec | partial: dx-crashloop |
| 20260927-221924 | fix-check | qwen3-8b-awq | sliced | 5 | 1 | 3/5 | n/a | submitted 5 | n/a | partial: subset; profile/topology assumed (pre D-40) |
| 20260927-220614 | baseline | qwen3-8b-awq | sliced | 1 | 2 | 0/2 | n/a | step_cap 2 | n/a | partial: subset; profile/topology assumed (pre D-40) |

One result isn't in these files. The first full baseline (2026-09-27, 8B, sliced) scored 9/26 on v1 in its first pass, but the crash fixed in D-39 lost its run file. Only D-39 records it.

## 5. Adding a model

1. Write `deploy/models/<name>.json` with the pinned revision, the architecture from its `config.json`, the vLLM args and the sampling from its model card.
2. Run `make fit MODEL=<name> TOPO=full`, and `TOPO=sliced`, to see the paper fit and the gate.
3. On the GPU, run `make deploy MODEL=<name> TOPO=<topology>`, then `make kv MODEL=… TOPO=…`, then `make golden MODEL=… TOPO=… TAG=<name> REPEAT=3 CONC=4`.
4. Run `make matrix` and commit `metrics/` and this file.
