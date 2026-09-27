# First Lambda session — test plan (D-34, D-37)

Goal: turn the paper and tokenizer numbers into measured ones, get the first model results per tier,
and produce the telemetry the rubric asks for. About 2 h of A100 time (~$4). There is no gateway in this
run yet: the laptop talks to vLLM through an SSH tunnel.

## Terminals
| Terminal | Command | Serves |
|---|---|---|
| 1 | commands below | — |
| 2 | `make tunnel` | vLLM `http://localhost:8000` (`/v1`, `/metrics`, `/health`) |
| 3 | `make grafana` (prints the admin password) | Grafana `http://localhost:3000` |
| 4 | `make kubeconfig && make k8s-tunnel` | k3s API `https://127.0.0.1:6443` (context `lambda`) |
| 5 | `make watch` | the doctor, autonomous; its metrics at `http://localhost:9109/metrics` |
| optional | `make prom` · `make opencost` | Prometheus `http://localhost:9090` · OpenCost API `http://localhost:9003/allocation/compute?window=1d&aggregate=namespace` |

## Steps
| # | Command | What it proves |
|---|---|---|
| 0 | `make preflight` | lint, 47 tests, golden references committed, lam has an API key |
| 1 | `make up` (~15 min) | the node bootstraps k3s, HAMi, Prometheus, Grafana, DCGM, OpenCost and the weights (`ready.json`) |
| 2 | `make deploy && make kv` | **KV pool per worker**: `GPU KV cache size` compared with the paper's 79,700 tokens |
| 3 | `make dashboards`, then terminals 2 and 3 | the dashboard "vLLM · engine and GPU" is live |
| 4 | `make golden TAG=baseline ONLY=dx-crashloop` | smoke test: agent, hermes parser, tools end to end |
| 5 | `make golden TAG=baseline` | **pass rate per tier**, prompt tokens per step, **cached share** (`metrics/golden-baseline-*.summary.json`) |
| 6 | `make sweep REPEAT=2` | concurrency 1/4/8/16/32: TTFT, KV usage, waiting, preemptions — **does KV bind before 32?** |
| 7 | terminals 4–5, then `make inject STAGGER=60` | **autonomous detection**: time from injection to detection to diagnosis, and whether the root cause is correct |
| 8 | `make inject FAULTS=all-easy` with `WATCH_ARGS="--max-parallel 12"` | **incident storm**: about 12 investigations at once, so KV and queue go under stress |
| 9 | `make watch WATCH_ARGS="--audit-every 300"` while step 6 runs | **interactive and batch mixed**: investigations slow down behind audits, because nothing orders them yet (motivates the gateway) |
| 10 | `make scale N=2 && make golden TAG=w2` | a second HAMi slice serves; KV per worker is the same |
| 11 | `make metrics TAG=end`, `make watch-metrics`, `make heal`, `make down` | save scrapes; billing stops |

## What to look at, and what we expect
| Metric (where) | Expected from our numbers | What it answers |
|---|---|---|
| `GPU KV cache size` (log, step 2) | ≈ 79,700 tokens per worker | Part 1: is the capacity arithmetic right? |
| `vllm:kv_cache_usage_perc` (Grafana, KV cache usage) | about 0.8 at ~24 easy or ~6 audits; > 1.0 (preemptions) at 32 easy | Part 1 and the PQ "what limited concurrency?": KV comes first |
| `vllm:num_requests_running` / `_waiting` | waiting > 0 only when KV is full, not at 32 running | engine queue vs gateway queue |
| `vllm:num_preemptions_total` | 0 until KV saturates | where KV must be protected (the 0.80 line) |
| `vllm:prefix_cache_hits_total ÷ queries_total`; golden `cached_share_of_prompt` | high: 3,787 of the ~4–15k prompt tokens are shared | shared vs unique tokens; why affinity routing pays |
| `vllm:time_to_first_token_seconds` p50/p95 | rises with concurrency and with uncached prompt length | Telemetry row: TTFT across levels |
| `vllm:inter_token_latency_seconds` | flat until the batch is large | decode-bound or not |
| `vllm:request_queue_time_seconds` | near 0 until the knee | where work waits |
| `vllm:request_prompt_tokens` p50/p95 | investigations 4–9k, audits 8–15k | context growth of an agent |
| `DCGM_FI_DEV_POWER_USAGE`, `DCGM_FI_PROF_SM_ACTIVE` | higher during audits and storms (prefill), lower during long decodes | Telemetry row: power, prefill vs decode |
| `DCGM_FI_DEV_FB_USED` | two slices at ≈ 18 GiB each after `make scale N=2` | the GPU split |
| golden `pass_rate_by_tier` | easy highest; multi-hop and red-herring show where the model matters | the tier story |
| golden `step_latency_s_p50/p95`, `steps_per_task_mean` | a few seconds per step; 4–8 steps per investigation | task latency, not only token latency |
| `doctor_detections_total`, `doctor_diagnoses_total{status,stop}` | one detection per injected fault; status `issue`; stop `submitted` | autonomous loop working |
| `doctor_diagnosis_seconds_sum ÷ _count` | tens of seconds per investigation | time to a root cause |
| `doctor_skipped_total{reason="retry"}` | 0 without a gateway | later: gateway sheds, visible to the app |
| `doctor_prompt_tokens_total`, `doctor_cached_tokens_total` | cached share as in golden | the watcher's traffic is the same shape |
| OpenCost `/allocation/compute` | the GPU cost split by namespace | cost per diagnosis (total ÷ diagnoses) |

## How to generate the numbers
- **Model quality and tokens:** `make golden`, which runs the 26 recorded tasks sequentially. It is deterministic on the cluster side.
- **Capacity knee:** `make sweep` (1 → 32 concurrent). Plot KV usage, waiting, TTFT p95 and preemptions per level from `metrics/vllm-sweep-*.prom` and the golden summaries.
- **Real incidents:** `make inject FAULTS=<ids> STAGGER=<s>` breaks the Lambda cluster on purpose, and the watcher finds and diagnoses the faults. `make faults` lists the catalogue. `FAULTS=all-multi_hop` or `all-red_herring` stresses the reasoning.
- **Storm:** `FAULTS=all-easy STAGGER=0` with `--max-parallel 12` produces a burst of interactive traffic.
- **Batch pressure:** `--audit-every 300` produces long prefill-heavy tasks every 5 minutes.
- **Mixed:** run the sweep and the watcher together to see interactive work competing with batch.

Record results in `metrics/` (commit them), in the capacity file's measured rows, and in D-29/D-34/D-37.

**What would change the design:**
- a KV pool far from 79.7k;
- a cached share well below the shared fraction;
- multi-hop or red-herring pass rates near zero, which moves the reviewer role (C17) up the list;
- detection-to-diagnosis latency over ~2 minutes in a storm, which makes gateway admission and priority urgent.
