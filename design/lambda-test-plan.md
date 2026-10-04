# First Lambda session — test plan (D-34, D-37)

Goal: turn the paper and tokenizer numbers into measured ones, get the first model results per tier,
and produce the telemetry the rubric asks for. About 2 h of A100 time (~$4). There is no gateway in this
run yet: the laptop talks to vLLM through an SSH tunnel.

## Terminals
| Terminal | Command | Serves |
|---|---|---|
| 1 | commands below | — |
| 2 | `make tunnel TUNNEL=pod/vllm-0` (sessions 1–2), `make tunnel` (session 3) | vLLM, or the gateway in session 3, at `http://localhost:8000` (`/v1`, `/metrics`) |
| 3 | `make grafana` (prints the admin password) | Grafana `http://localhost:3000` |
| 4 | `make kubeconfig && make k8s-tunnel` | k3s API `https://127.0.0.1:6443` (context `lambda`) |
| 5 | `make watch` | the doctor, autonomous; its metrics at `http://localhost:9109/metrics` |
| optional | `make prom` · `make opencost` | Prometheus `http://localhost:9090` · OpenCost API `http://localhost:9003/allocation/compute?window=1d&aggregate=namespace` |

## Steps
| # | Command | What it proves |
|---|---|---|
| 0 | `make preflight` | lint, Python and gateway tests, the gateway's linux build, golden references committed, fit gate for MODEL/TOPO, lam has an API key |
| 1 | `make up` (~15 min) | the node bootstraps k3s, HAMi, Prometheus, Grafana, DCGM, OpenCost and the weights (`ready.json`) |
| 2 | `make deploy && make kv` | **KV pool per worker**: `GPU KV cache size` compared with the paper's 79,700 tokens |
| 3 | `make dashboards`, then terminals 2 and 3 | the dashboard "vLLM · engine and GPU" is live |
| 4 | `make golden TAG=baseline ONLY=dx-crashloop` | smoke test: agent, hermes parser, tools end to end |
| 5 | `make golden TAG=baseline` | **pass rate per tier** (v1 and v2, D-41), prompt tokens per step, **cached share** (`metrics/golden-baseline-*.summary.json`) |
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

## Session 2: model selection (D-40, D-41)
This session produces a model matrix a grader can follow. Each model runs the same 26 golden tasks on the same engine
settings, and the matrix reports v1 and v2 scores with 95% intervals. Pass rates can be compared across topologies,
because slicing changes speed but not answers. Latencies can't be compared across topologies. Run `make fit-all` before
paying to see the paper numbers, and read `design/model-matrix.md` for the plan. Allow about 2 hours of A100 time.

Keep `make tunnel TUNNEL=pod/vllm-0` running in terminal 2. Each `make deploy` restarts vllm-0, which ends the
port-forward, so restart the tunnel after every deploy. Pass the deployed `MODEL` and `TOPO` to every `make kv` and `make golden`, because both
record them. A `make kv` without them files the log under the default 8B name.

| # | Command | What it produces |
|---|---|---|
| 0 | `make preflight` | The tests pass, the references pass v1 and v2, and the default pair passes the fit gate |
| 1 | `make up`, or reuse a running node | A node with the 8B weights prefetched and served on one slice |
| 2 | `make kv MODEL=qwen3-8b-awq TOPO=sliced` | The 8B's measured pool (79,056 tokens on 2026-09-28) |
| 3 | `make golden MODEL=qwen3-8b-awq TOPO=sliced TAG=8b REPEAT=3 CONC=4` | The 8B baseline: 26 tasks × 3, both scores, 95% interval |
| 4 | `make deploy MODEL=qwen3-30b-a3b-2507-awq TOPO=full && make kv MODEL=qwen3-30b-a3b-2507-awq TOPO=full` | Fetches about 17 GiB and reports the measured pool (185,136 tokens on 2026-09-28) |
| 5 | `make golden MODEL=qwen3-30b-a3b-2507-awq TOPO=full TAG=30b-smoke ONLY=dx-crashloop,dx-port-mismatch REPEAT=1` | Confirms the community quantization and the hermes parser work: tool calls parse and `finish_reason` isn't `length` |
| 6 | `make golden MODEL=qwen3-30b-a3b-2507-awq TOPO=full TAG=30b REPEAT=3 CONC=4` | The 30B-A3B row |
| 7 | `make deploy MODEL=qwen3-14b-awq TOPO=sliced`, `make kv MODEL=qwen3-14b-awq TOPO=sliced`, then `make golden MODEL=qwen3-14b-awq TOPO=sliced TAG=14b REPEAT=3 CONC=4` | The 14B row. It shows whether a bigger dense model closes the gap to the 30B-A3B |
| 8 | If time allows, a smoke run and then a full run of `qwen3.5-9b` on `TOPO=full` | The hybrid row. `make kv` shows vLLM's own hybrid pool; check that prefix caching gets hits |
| 9 | `make matrix` | The ranking. Overlapping intervals mean a difference isn't established yet |
| 10 | `make metrics TAG=models` and `make matrix`, commit `metrics/` and the matrix, then `make down` | Billing stops |

Without the gateway the tunnel reaches vllm-0 only, so `make scale N=2` would pay for a second slice that sits idle.
Session 3 puts the gateway in front of both.

For each model, look at these values:
- the v2 pass rate and its interval;
- `parts_v2`, to see whether the root was found and, if it was, whether the category or the mechanism was wrong;
- `abstained`, which should be about 0 on these faults because the tools can observe all of them;
- `finish_reasons`, where `length` means the 768-token cap cut off a submission;
- `cached_share_of_prompt` and step latency p50 and p95;
- vLLM's `kv_cache_usage_perc` in Grafana at CONC=4.

Use this rule to choose:
- A model wins if its v2 interval sits clearly above the 8B's, unless it produces fewer correct diagnoses per GPU-hour
  and the quality gap is small.
- If the best model doesn't fit a slice, report two models: the best whole-card model for answer quality, and the best
  model that fits a slice for the slicing, routing and KV-hop demo.
- Record the choice as a decisions entry.

Results from 2026-09-28, at REPEAT=2 and CONC=1: the 8B on one slice scored 52% on v2 (interval 39–65%) and the
30B-A3B on the whole card scored 67% (54–78%). `design/model-architecture-guide.md` §8 interprets them.

## Session 3: the gateway (C11, D-42)
This session runs the doctor through the gateway on two 8B slices and compares the two placement policies. Rehearse it
on the laptop first with `make demo`, which runs the same pipeline against two fake workers. Allow about 1.5 hours of A100
time. The cloud-init fetches the 14B in the background after `ready.json` (`/var/log/prefetch.log` on the node), so
step 9 usually finds its weights already on disk.

The tunnel now ends at `svc/gateway`, so a `make deploy` or `make scale` no longer breaks it. Restart it only after
`make gateway`, which restarts the gateway pod. Use a different `TAG` per arm, because `make golden` files results by tag.

| # | Command | What it proves |
|---|---|---|
| 0 | `make preflight`, then `make demo` | The gateway builds for linux and its tests pass; the laptop demo shows 0 refusals and `orch_restricted_offbox_total 0` |
| 1 | `make up TYPES=gpu_1x_a100_sxm4`, or reuse a running node | An A100 node with the 8B fetched. A plain `make up` takes the first of GH200, H100 and A100 with capacity and writes `.cache/node.env`; on an H100 or GH200 use `TOPO=h100-half` or `gh200-half` for two workers |
| 2 | `make deploy && make kv && make scale N=2` | Two 8B workers on 20 GiB slices; `make kv` should report about 79,056 tokens, as on 2026-09-28. vLLM stays on v0.29.0: the v0.30.0-cu129 image crashes on start (torch cu130 with a cu129 torchvision, vllm-project/vllm#59157) |
| 2b | `make tunnel TUNNEL=pod/vllm-0` in terminal 2, then `make golden TAG=8b-direct REPEAT=2 CONC=1` | The 8B without the gateway, on today's node: the baseline step 5 is compared with |
| 3 | `make gateway`, then `make tunnel` in terminal 2 | The node pulls the pinned gateway image from Docker Hub without credentials, the warm-up ConfigMap is created, and the rollout finishes |
| 4 | `curl -s localhost:8000/debug/workers` | Both pods report `Ready`. A pod stays `Warming` until two warm-up probes pass; check `kubectl logs deploy/gateway` if it stays there |
| 5 | `make golden TAG=gw-ptl WORKERS=2 CONC=8 REPEAT=2` | Pass rate through the gateway, which should match step 2b, since the gateway changes placement, not answers |
| 6 | `make sweep WORKERS=2` | The gateway's and both pods' `/metrics` at each concurrency level, under `metrics/gateway-sweep-*` and `metrics/vllm-{0,1}-sweep-*` |
| 7 | `make gateway POLICY=least_loaded`, restart the tunnel, then repeat 5 and 6 with `TAG=gw-ll` | The control arm of the stickiness A/B |
| 8 | `make metrics TAG=gw` | A final scrape of the gateway and both pods |
| 9 | `make deploy MODEL=qwen3-14b-awq && make scale N=2 && make gateway MODEL=qwen3-14b-awq`, restart the tunnel, then `make golden MODEL=qwen3-14b-awq TAG=14b-gw WORKERS=2 CONC=4 REPEAT=3` | The 14B row on two slices. `make gateway` needs the same `MODEL`, because its warm-up body names the served model |
| 10 | commit `metrics/`, then `make down` | Billing stops |

For each arm, look at these values in `metrics/gateway-*.prom`:
- `orch_sticky_total{outcome}`, where `hit` should dominate under `prefix_then_load` and fall under `least_loaded`;
- `orch_prompt_tokens_total{kind}`, the split between `shared_hit`, `run_hit` and `miss`, against
  `cached_share_of_prompt` in the golden summary;
- `orch_pick_total{pod}`, to check that both pods take traffic;
- `orch_shed_total{reason,code}` and `orch_overflow_total{result}`, which should be near 0 at CONC=8;
- `orch_restricted_offbox_total`, which must be 0;
- `orch_replica_kv_free_ratio` and `orch_replica_snapshot_age_seconds` in Grafana, to see whether KV or staleness drove a
  pick;
- step latency p50 and p95 from the golden summary, compared between the two arms.

If something goes wrong:
- The gateway pod is stuck in `ImagePullBackOff`: check the digest in `deploy/k8s/gateway.yaml` exists on Docker Hub; after a
  gateway code change, run `make gateway-image` and commit the new digest first.
- The gateway pod never becomes ready: `/readyz` needs one warm worker, so check that the vLLM pods are `Running` and
  read `kubectl logs deploy/gateway` for the warm-up probe result.
- Many 429s: the tenant quota scales with the worker count (`gateway/internal/fleet/gate.go`); `kubectl logs deploy/gateway`
  prints one line per request with its pod, sticky outcome and refusal reason.
