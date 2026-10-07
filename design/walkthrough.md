# Presentation walkthrough: the code and the dashboards

The brief asks for a walk through the code, the reasons behind each choice, and a short tour of the Grafana dashboards
in this order. Each section names the code to open, the choice and why, the panel to show, and the evidence. Grafana
has three dashboards: `Ariadne · cluster`, `Ariadne · gateway` and `Ariadne · vLLM engine + GPU`.

## 1. Cluster

**Code.** `deploy/cloud-init.yaml` boots one Lambda node: k3s, HAMi to split the GPU, Prometheus, Grafana, DCGM,
OpenCost and KEDA, every chart and image pinned. `make up` (`lab/up.sh`) takes the first of H100, GH200 and A100 with
capacity, and one cloud-init detects the card and picks the model (`deploy/serving.json`).

**Why.** The brief's app is one team's diagnosis tool, so one node with the GPU split into two workers gives routing
and admission something to decide without a second card. Cluster data is restricted, so the model runs on our own GPU.

**Dashboard.** `Ariadne · cluster`, row *Cluster*: node Ready, pods by namespace and phase, restarts in the last
hour, node CPU and memory, GPU memory for the card and each HAMi slice.

## 2. Success and failures

**Code.** `gateway/internal/metrics/metrics.go` records each request once, as answered (`orch_completed_total` by status
and finish reason) or refused (`orch_shed_total` by reason). A client that leaves is `client_gone`, not a worker error
(`gateway/internal/serve/server.go:154`, D-44). The doctor retries a 429 or 503 after `Retry-After`
(`doctor/agent.py:206`), so a refusal is not yet a failed run.

**Why.** "Did the request work" and "did the diagnosis work" are different questions. The dashboard answers the first;
the golden set answers the second (report §2, §5).

**Dashboard.** `Ariadne · cluster`, row *Outcomes*: every request stacked by outcome, the share answered with a
200, upstream errors by pod.

**Evidence.** Qwen3.8 diagnosed 86.5% of 26 faults (F1). Under load, refusals rather than wrong answers caused the
failures (F27, F36).

## 3. Gateway and admission: sheds by reason

**Code.** `gateway/internal/decide/request.go:171` (`Inspect`, the guard: 400s), then
`gateway/internal/decide/shed.go:135` (`ShouldShed`): the tenant token quota (`tenant_tokens`, 429, stays), the KV line
(`kv_free`, 503), the queue deadline (`timeout_queue`), tail-latency spread (`p99_spread`), and a full queue
(`queue_full`). `gateway/internal/fleet/gate.go:311` (`Admit`) runs them against live worker state. The decisions are
pure functions, tested without a network.

**Why.** The client only understands 429 and 503, so the gateway is the one place that can decide who gets in and say
why. The in-flight cap per worker comes from the measured KV pool, `serving/fit.py:111` (D-43): 4 on an H100 half.

**Dashboard.** `Ariadne · gateway`, row *Admission*: requests by priority, admitted against shed by reason.

**Evidence.** Up to 16 concurrent runs the `kv_free` shed kept vLLM from preempting (F25). Sized to KV, overload moved
into the gateway's queue as `timeout_queue` (F32), with ~95% fewer preemptions but fewer completed runs (F36).

## 4. Router

**Code.** `gateway/internal/decide/pick.go:156` (`Pick`). `prefix_then_load` keeps a run on the worker that holds its
history unless that worker is more than 0.25 load units busier. `least_loaded` and `p2c` are the controls.

**Why.** Each agent step resends the whole conversation, so the worker that served the last step already has all but
the newest few hundred tokens cached. Sending the run back there saves prefill.

**Dashboard.** `Ariadne · gateway`: *Placement: pod A vs pod B*, *Stickiness outcomes*, and *Prompt tokens:
shared prefix vs run history vs uncached*.

**Evidence.** The A/B: without stickiness, vLLM recomputed 10% more prompt tokens and steps were 8–11% slower, with the
same answers (F29).

## 5. Queue depth by pod

**Code.** `gateway/internal/fleet/queue.go:94` (`Acquire`): one queue per worker, interactive ahead of batch, each
request with a deadline (10 s interactive, 30 s batch). A request that would miss its deadline is refused before it
waits.

**Why.** Queueing at the gateway, in priority order, is cheaper than letting vLLM preempt: a preempted request
recomputes its prompt.

**Dashboard.** `Ariadne · gateway`, *In flight and queued per pod* and *Queue wait p50/p95*. The vLLM dashboard's
*Requests running vs waiting* shows the engine side, which should stay near 0 waiting.

## 6. vLLM

**Code.** `deploy/k8s/vllm.yaml`, rendered from `deploy/models/*.json` and `deploy/serving.json` by `make render`:
prefix caching on, chunked prefill at 8,192 tokens per step, 32 sequences, a 24,576-token context, two workers on HAMi
halves. A worker takes traffic only after two warm-up probes replay the doctor's real first request (C13).

**Why.** The settings follow the workload: long shared prompts that grow by a few hundred tokens per step.

**Dashboard.** `Ariadne · vLLM engine + GPU`: KV usage, preemptions, prefix hit rate, TTFT and inter-token latency, tokens per
engine step, prefill against decode time, GPU power.

**Evidence.** 88% of prompt tokens came from the cache (F9). KV runs out first: about two full-length runs per H100 half
(F6). Chunked prefill rarely binds (F15).

## 7. Mooncake KV hop

**Code.** `gateway/internal/hop/policy.go:135` (`Decide`): hop only when the history is at least 8,192 tokens and the
copy is cheaper than the recompute. `gateway/internal/hop/mooncake.go:76` (`Send`) does the transfer through vLLM's
`MooncakeConnector`. Any failure falls back to recomputing. Transfer ids are random and the guard refuses
client-supplied `kv_transfer_params`.

**Why.** When the router has to move a run, the destination recomputes the run's history. With long histories the copy
is cheaper. With today's short ones it rarely is, so the threshold keeps most moves as recomputes.

**Dashboard.** `Ariadne · gateway`, *KV hops by result* and the hop latency p95. It has data only with `HOP=1`.

**Evidence.** 8 hops completed the protocol on an H100 with none failed. Nobody has yet confirmed that the destination
pulled the KV rather than recomputing it (F35).

## 8. Pods, replicas and KEDA: which pool scales

**Code.** `deploy/autoscale/keda-vllm.yaml` is a KEDA ScaledObject on the `vllm` StatefulSet, applied by
`make autoscale`. It scales between 1 and the topology's `max_replicas` (2 on the halves) on two signals, recording rules
in `deploy/observability/alerts.yaml` that promtool tests:
- `doctor:vllm_demand_requests`, in flight plus queued, divided by the per-worker cap (4), so it reads as workers
  needed;
- `doctor:gateway_capacity_sheds_per_minute`, refusals another worker would have prevented. A tenant quota does not
  count.

**Why.** The only pool is the vLLM worker pool, and KV per worker is the limit (F6, F24), so more workers is the lever.
A new worker loads the model and warms up for minutes, so scaling follows sustained load. It scales up after a minute
of demand and down only after 10 quiet minutes, never to zero. The gateway needs no change: it already knows both
worker addresses and routes to a worker only once it is warm.

**Dashboard.** `Ariadne · cluster`, row *Scaling*: KEDA's desired workers against ready ones, demand per ready
worker, capacity sheds per minute, and each worker's phase (down, warming, ready).

**Evidence.** Rehearsed on kind with a fake gateway (F38). Session 5 of `design/lambda-test-plan.md` measures it on the
H100.
