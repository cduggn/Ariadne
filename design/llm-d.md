# llm-d 1.0 behind the gateway (D-54)

**Status.** Opt-in (`make deploy LLMD=1`, `make llm-d`, `make gateway ROUTER=1`). The per-pod mode the gateway has run
since D-42 stays the default until a GPU A/B says otherwise (see **Next step**).

llm-d 1.0 (released 2026-10-20) ships, as stable or beta plugins, three things our gateway built by hand. It places a
request by what each engine has really cached. It queues by priority and fairness. And it moves KV to host memory when
the GPU fills. It ships none of the things that make this product safe to run for several tenants. It has no token
budget, no data class, no rule about what may leave the box, and no answer contract the doctor's retry loop
understands. So the split follows that line. llm-d decides which pod. The gateway decides whether the request may run
at all, who pays for it, and where it goes when the box is full.

```
doctor ──► gateway (guard, trust headers, tenant quota, stay-or-leave, refusal contract, orch_* metrics)
              │  x-llm-d-inference-fairness-id = tenant, x-llm-d-inference-objective = doctor-interactive|doctor-batch
              ▼
           llm-d router: Envoy ─ext-proc─► EPP (precise prefix, sticky until saturated, token load, flow control)
              │                                ▲ KV events (ZMQ :5556/:5559)
              ▼                                │
           vLLM pods (prefix caching + OffloadingConnector, GPU → /dev/shm CPU tier)
```

Every upstream fact below is from the v1.0.0 tag or a page marked "Version: v1.0", read on 2026-10-21. Facts we could
not confirm are marked **unverified** and listed again at the end.

## Replaced by llm-d

| Ours (fleet mode) | Where it lives | llm-d 1.0 replacement | Why llm-d is better at it | What we give up |
|---|---|---|---|---|
| `prefix_then_load` placement | `gateway/internal/decide/pick.go:360` | `precise-prefix-cache-producer` + `prefix-cache-affinity-filter` + `token-load-scorer` | Ours never sees the cache. Its "prefix index" is the run table (`fleet/gate.go:157`), keyed by run id, so it knows only that a run was last on a pod. EPP indexes the blocks vLLM reports it stored and evicted, so it routes on what is cached now, across runs. The cluster card that every task on a cluster shares (D-23) now pulls those tasks to one pod. Before, warm-up copied it to every pod and the router never used it | The run-level `X-Sticky` outcomes and the `orch_sticky_total` series. EPP's `llm_d_epp_prefix_cache_affinity_filter_decisions_total{outcome}` is the nearest equivalent |
| Stickiness slack and D-52's `LoadWithout` | `decide/pick.go:112`, `:129` | `prefix-cache-affinity-filter` ("sticky until saturated") | D-52 fixed a bug in our estimate of how much KV the run's own history occupies (F48). The filter compares the predicted TTFT penalty of leaving against staying, using a calibrated prefill rate. That is the same question, asked with the engine's numbers rather than ours | `GW_POLICY` and the slack knob. The filter's knob is `peakPrefillThroughput`, rendered from the same prefill rate `serving/fit.py` already computes for the hop |
| Bytes ÷ 3.5 token estimate for placement | `decide/shed.go` `EstimateTokens` | `token-producer` renders the request through vLLM's own tokenizer endpoint | An estimate cannot match a block hash. Precise routing needs the real tokens | Nothing. The estimate stays for tenant charging, where an estimate corrected at settle is enough |
| Two-lane per-pod queue (interactive first, FIFO, displace batch) | `fleet/queue.go:200` | Flow control: priority bands 100 (`doctor-interactive`) and 0 (`doctor-batch`), round-robin fairness by tenant | Ours has priority and no fairness, so one tenant's burst of audits fills the batch lane for everyone. Bands give the same interactive-first order plus per-tenant round robin inside each band | Displacement of the newest batch waiter by an interactive arrival. Bands are strict priority, so batch still waits behind interactive, but a full queue now rejects the arrival rather than evicting a waiter (eviction is `enableEviction`, off by default) |
| Per-pod KV admission line, p99 spread, queue deadline, `no_eligible_pod` | `decide/shed.go:14-135`, `pick.go:164` | `concurrency-detector` saturation + band TTLs + `rejected-no-endpoints` | Our KV line reads a scrape up to 500 ms old plus our own reservations. EPP reads the same vLLM metrics and owns the queue the line protects. Two admission controllers in series would each see half the picture | The `kv_free` and `p99_spread` reasons. D-43's sizing survives (see **Changed to fit together**) |
| Per-pod scrape loop for placement | `fleet/scrape.go`, `fleet/worker.go:90` | EPP `metrics-data-source` | The scrape fed placement and admission, and both move to EPP | `orch_replica_*` gauges in router mode. vLLM's own metrics are still scraped by Prometheus, and EPP exports `llm_d_epp_per_endpoint_queue_size` |
| KV hop on a broken stick (Mooncake, gateway-orchestrated) | `gateway/internal/hop/*` | Fewer broken sticks (affinity filter) + native CPU offload | The hop moved a run's KV when the gateway moved the run. Behind llm-d the gateway no longer chooses the pod, so it cannot know the destination before the forward. The hop was also our most fragile path (F35, F45, F48 to F51). Offload keeps evicted prefixes on the same pod's host tier, which covers the commoner case of a run returning to a pod that had to evict it | A broken stick now recomputes the history. That costs one prefill of the history, about 4.4 s for a 12k history at h100-half's paper prefill rate of 2,721 tokens/s (estimate, `python -m serving.fit qwen3.8-27b-fp8 h100-half --gateway-env`). Breaks should be rarer, because the filter leaves only when staying costs more than that prefill (inferred from the filter's design, not measured). The hop stays available in fleet mode |

## Kept in our gateway

| Ours | Where it lives | Why llm-d does not replace it |
|---|---|---|
| Request guard (body size, shape, `stream_unsupported`, default `max_tokens`) | `decide/request.go:171-242` | EPP parses requests to route them, not to refuse them. Nothing in v1.0 rejects a 1 MiB body or an unbounded `max_tokens` |
| Refusing client `kv_transfer_params` | `decide/request.go:203` | This matters more now. llm-d's P/D and connectors steer vLLM through that field, so a client that can set it can ask a pod to pull KV from an address of its choosing. Only the ingress can stop it |
| Header trust boundary | `serve/server.go` `forwarded` | llm-d's flow-control guide says the ingress must strip client `x-llm-d-*` headers and set fairness after authenticating the caller. Our whitelist already drops every header it does not name, so a client cannot pick its own fairness id, objective or `x-gateway-destination-endpoint`. The gateway now sets the first two itself |
| Tenant token budget (429 `tenant_tokens`) | `fleet/gate.go` buckets | Flow-control fairness divides capacity among the tenants who are waiting. It does not cap how many tokens a tenant may spend. Those are different promises, and the quota is the one a platform team bills against |
| Data class and stay-or-leave (`MayLeave`, the `Offboxable` permit) | `decide/overflow.go:30` | llm-d has no off-box path. "Restricted never leaves" is the product's premise (D-53), and it is enforced by a type no other package can forge. The decision has to sit where the data class is parsed |
| Overflow to Superlinked behind a probed circuit | `gateway/internal/overflow` | Same reason. llm-d's answer to a full box is to queue, then reject |
| Refusal contract (503 for capacity, 429 for quota, `Retry-After`, `X-Gateway-Reason`, OpenAI error body) | `serve/server.go:580` | The doctor's retry loop (D-44) reads this contract. llm-d answers saturation with 429, which the doctor would read as its own quota. See **Changed to fit together** |
| `orch_*` request metrics, cached-token split, one JSON line per request | `gateway/internal/metrics`, `serve/server.go:452` | They are per tenant and per data class, which EPP cannot label. The dashboards and five alerts (D-45) read them |
| KV fit calculator | `serving/fit.py` | llm-d has no capacity model. It still runs on vLLM, so pool size, KV per token and hybrid state per sequence are unchanged. Its outputs now feed EPP's config instead of only `GW_MAX_INFLIGHT` |
| Fleet mode itself | `gateway/` | Kept, so that the A/B that would justify deleting it can still be run |

## Changed to fit together

- **llm-d's saturation 429 becomes our capacity 503.** `decide/router.go` maps `x-llm-d-request-dropped-reason` to a
  verdict. `rejected-saturated`, `rejected-ttl-expired` and `evicted` arrive as 429. Passed through, they would bypass
  overflow, because `MayLeave` lets only a 503 leave, and the doctor would read them as its own quota. They become 503
  `router_saturated`, `router_ttl_expired` and `router_evicted`. `rejected-no-endpoints` and `rejected-shutting-down`
  keep 503 under a `router_*` reason. Any other drop reason becomes `router_unknown`. A drop without the header
  (`rejected-internal`, a vLLM error) is relayed unchanged, so it is never mistaken for capacity.
- **The tenant charge no longer assumes overflow.** `ChargeOffbox`/`SettleOffbox` became `ChargeTenant`/`SettleTenant`.
  They charge any request the gate did not place, either an off-box answer or one the router placed. A request llm-d
  refused is refunded and then may overflow, where it is charged again exactly once.
- **D-43's KV-sized cap moves into EPP.** `serving/llmd.py` renders `concurrency-detector.maxConcurrency` from the same
  in-flight number fit.py computes from the measured pool: 9 for the 8B on `sliced`, 4 for Qwen3.8 on `h100-half`. The
  detector's v1.0.0 README defines pool saturation as in-flight requests over the sum of every endpoint's
  `maxConcurrency`, so the number is per endpoint, like `GW_MAX_INFLIGHT`. It also renders `peakPrefillThroughput`
  from the topology's prefill rate. Both come from `fit.max_inflight` and `fit.prefill_rate`, which the gateway's env
  now uses too, so no number is copied by hand.
- **Flow-control bands wait no longer than the gateway would have.** `doctor-interactive` is priority 100 with a 10 s
  TTL and `doctor-batch` is priority 0 with 30 s, the gateway's own queue budgets (`decide.DefaultBudgets`). A test
  keeps them equal. The bands cap waiting time, not queue length, so a burst waits its turn rather than being shed.
- **Fairness follows the quota, not the tenant name.** The gateway sends the name of the bucket it charged as
  `x-llm-d-inference-fairness-id`. Every tenant without its own bucket shares `*`, so a client cannot win extra
  round-robin turns by inventing tenant names (found by the commit security review).
- **The workers get a NetworkPolicy in llm-d mode.** Port 8000 accepts the router's proxy and EPP, the gateway and
  Prometheus. The KV-event ports accept only the EPP, because vLLM's `BlockStored` events carry the token ids of the
  prompt. Fleet mode without the hop had no policy, and llm-d mode would otherwise have published every prompt to any
  pod in the cluster (found by the commit security review).
- **The KEDA capacity signal counts router sheds.** The `doctor:gateway_capacity_sheds_per_minute` rule (D-49, D-51) now
  matches the `router_*` reasons, so autoscaling still sees a full box in router mode.
- **Prefix mode is chosen per model, not globally.** Precise routing hashes 64-token blocks, which must match the
  engine's block size. Hybrid models cannot use 64. vLLM v0.29 aligns their attention block to the recurrent state, at
  528 tokens for Qwen3.5 (`design/model-architecture-guide.md`). So full-attention profiles get precise routing with
  `--block-size=64` and KV events. Hybrid and sliding-window profiles get approximate prefix scoring, which needs no
  engine change. The table is `profiles.ROUTER_PREFIX`. This matters for the H100: its boot default, Qwen3.8-27B-FP8,
  is hybrid, so on that node llm-d routes approximately. Precise routing applies to the full-attention profiles, which
  include the A100 default, Qwen3-8B.
- **Hop and offload are exclusive.** Both are KV connectors, and vLLM takes one unless they are wrapped in a
  MultiConnector. The hop also needs the gateway to pick the pod. The render refuses `hop` with `llmd`, and the gateway
  refuses `GW_HOP` with `GW_ROUTER_URL`.

## Not adopted from llm-d, yet

| llm-d 1.0 feature | Why not now | Revisit when |
|---|---|---|
| Gateway API chart (`llm-d-router-gateway`) with Istio, agentgateway or Envoy Gateway | The node runs k3s with traefik disabled, and our gateway is already the ingress. The standalone chart (proxy as a Service) needs no gateway controller and no HTTPRoute | A second app shares the cluster and needs routes we would otherwise hand-write |
| P/D disaggregation (NIXL, DisaggregatedSet, LWS) | One GPU node, two workers on HAMi slices, tcp only. Splitting prefill from decode needs a fast interconnect to pay for the KV transfer on every request. Our own hop moves KV over tcp only above 8,192 tokens (its cost model, not a measurement), and the destination's pull is still unconfirmed (F35, F45) | A multi-GPU node with NVLink or an RDMA fabric |
| `session-affinity-filter` / `x-session-id` | A hard pin keeps a run on a hot pod (the impact table in the v1.0 docs says so). The affinity filter already keeps a run where its prefix is until staying costs more than moving | Never, unless a stateful backend needs a hard pin |
| `probabilistic-admitter` | Beta. Its rejection status code is not documented (**unverified**), and our contract depends on the code | Its code is documented, and we have negative-priority traffic worth shedding early |
| Turn-priority and `agent-identity` | Alpha, behind `--allow-experimental-plugins` | They reach beta. The doctor is the agentic workload they target |
| Predicted-latency routing | No configuration in the v1.0 docs we could cite | Documented with a config surface |
| Multi-tier offload to filesystem (`TieringOffloadingSpec`) | The node's NVMe holds the weights cache, and the CPU tier alone covers our working set (the shared prefix plus about 24 runs per worker, D-29) | Working sets outgrow host memory |
| vLLM v0.30 (the version llm-d 1.0 ships) | The v0.30.0 `-cu129` image crashes on start (vllm-project/vllm#59157, in `backlog.md`). The v0.29.0 pin is verified on GPU for every profile (D-40). `OffloadingConnector`, KV events and the offload metrics already exist in v0.29, and llm-d's v1.0.0 precise-prefix guide notes v0.29 serves the render endpoints the `token-producer` calls | A fixed v0.30 image exists. Bump in its own commit, then rerun `make golden` |

## Gaps router mode opens

- **No warm-up gate.** In fleet mode the gateway holds a pod out until two warm probes answer in under 1 s. EPP admits
  any Ready pod. A restarted pod can take the first requests while CUDA graphs are still being captured. The fix is a
  vLLM `startupProbe` that sends the warm body. It is in the backlog and not built here.
- **`/readyz` is always ready in router mode.** Pool and endpoint readiness belong to EPP now. A probe of the router is
  a follow-up.
- **Which pod served a request.** llm-d strips `x-gateway-destination-endpoint-served` before the answer leaves Envoy
  (measured in `lab/llm-d/e2e.sh`, and it is on llm-d's own strip list). So `X-Pod` reads `router` and
  `orch_pick_total{pod}` has one series in router mode. Per-pod placement is in Envoy's per-host counters and
  `llm_d_epp_per_endpoint_queue_size`.
- **Prometheus cannot scrape the EPP yet.** EPP's `/metrics` checks the caller's token (`--metrics-endpoint-auth`,
  default true) and answers 401 without one (measured on kind). Our Prometheus scrapes by pod annotation and sends no
  token, so the `llm_d_epp_*` series this doc relies on will not reach Grafana until a scrape job sends a ServiceAccount
  token allowed `get` on `/metrics`. `lab/llm-d/kind-smoke.sh` reads them that way. We keep the auth on.
- **Usage is not remembered between steps.** Fleet mode keeps each run's last usage on its run entry, and the next
  step's charge starts from it. In router mode `Admit` never creates the run entry, so every step is estimated as a
  first step (body bytes ÷ 3.5 plus max output) and corrected at settle. The quota still balances. Only the up-front
  hold is less exact.
- **FailOpen.** The rendered pool uses `FailOpen`, so if EPP is down, Envoy still routes, without flow control. That is
  acceptable only because the tenant quota stays in our gateway. Under `FailClose`, an EPP restart would refuse every
  request.

## Unverified

- The exact `InferenceObjective` v1 schema. The v1.0.0 guide still ships `llm-d.ai/v1alpha2` with `poolRef`, so we
  render what the guide ships.
- Whether the precise producer counts CPU-tier hits through KV events in v1.0. The tiered-prefix guide scores the CPU
  tier with a second, approximate producer instead.
- The probabilistic admitter's rejection code.
- Every llm-d number in this doc (vendor TTFT and throughput claims) is from the release announcements, not measured
  here.

## Verification

| Claim | How | Result |
|---|---|---|
| Router mode guards, charges, maps drops and keeps stay-or-leave | `go test -race ./...` (`decide/router_test.go`, `serve/router_test.go`) | Pass |
| The real EPP's saturation reaches the doctor as 503 `router_saturated` with Retry-After, restricted stays, internal overflows | `make llm-d-lab` (llm-d v1.0.0 EPP and Envoy, two fakes, file discovery) | 6 `RejectedCapacity` at the EPP, 3 of 8 restricted answered 503 and none left, 3 of 8 internal answered off-box |
| A spoofed `x-gateway-destination-endpoint` does not steer | same | Pass |
| Repeats of one prefix stay on one worker | same, from Envoy's per-host `rq_total` | 4 of 4 on one worker from a cold lab |
| Our CRD URLs, chart values and objectives install and work | `make llm-d-kind` | InferencePool, both InferenceObjectives and the chart install; the EPP finds both pods by label; interactive lands in band 100 and batch in band 0; a burst at `maxConcurrency` 1 queues and all 8 are served |
| The worker NetworkPolicy shuts out other pods | same, a pod with no allowed label calls a worker's 8000 | No answer (`000`), while the proxy still serves |
| Render, tests and alert rule | `make test` (promtool, pytest, Go), ruff, `helm template` against the v1.0.0 chart | Pass |
| Precise routing on KV events, CPU offload, TTFT and cache hit rates | Needs real vLLM on a GPU | **Not run** |

## Next step

One GPU session runs the A/B that decides whether fleet mode's placement code can be deleted: `make golden` and
`make sweep` at concurrency 8, 16 and 32, fleet mode against router mode, on the same model and topology. Compare
cached share of prompt tokens, TTFT p50/p95, preemptions and golden pass rate. If router mode is no worse, delete
`pick.go`'s policies, the queue, the scrape-for-placement and the hop in one wave
(principle: migrate callers, then delete).
