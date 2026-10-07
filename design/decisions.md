# Decisions log

One entry per decision: what we chose, why, what we checked, and what would make us revisit it.
Numbering continues from the predecessor project (trip planner, `cduggn/trip-planner-inference`),
whose decisions still govern the serving stack this repo inherits.

What the measurements showed, with evidence for each, is in [`findings.md`](findings.md).

## Inherited from the trip-planner project (D-1 … D-18, summary)

| Id | Decision | Why it still matters here |
|---|---|---|
| D-2, D-14–D-16 | Model path Qwen2.5-3B → 7B-AWQ → **Qwen3-8B-AWQ** (thinking disabled per request, `hermes` tool parser) | Same model and serving flags; its golden history (3.4 % → 69 %) is the model-choice evidence |
| D-10, D-13 | Generic agent loop with harness guards: `tool_choice=required`, duplicate refusal, budgets, submit-now nudge, expect-blind validation + repairs | Ported unchanged in spirit (D-22) |
| D-11 | Packaging: cloud-init bootstrap (k3s, HAMi, pinned everything), plain manifests, Makefile over the `lam` CLI | Reused; extended with OpenCost (D-26) |
| D-12 | Static per-key context before the task (city brief) for prefix caching | Becomes the cluster card (D-23) |
| D-17 | The model chooses, code computes facts and arithmetic | Diagnoses cite tool refs; nothing numeric is model-authored (D-24) |
| D-18 | Known gap: after repairs the loop accepted a failing answer | Fixed here: fail closed (D-24) |

---

### D-19 — Product: a read-only Kubernetes cluster doctor served by self-hosted inference (2026-09-27)
**Choice:** the final-project app becomes a cluster doctor: given a report ("orders-api keeps restarting") or an audit request, it investigates a cluster read-only (pods, events, specs, logs, metrics, cost) and returns grounded findings with a suggested fix it never applies. The trip planner is kept in its own repo as the predecessor and model-choice evidence.
**Because:** the lecturer (office hours, ~00:17) called out that nobody builds tool-calling agents for cloud/platform work; the user works on a platform team and sees a product. The serving story is stronger than the trip planner's: log-heavy tool results create real KV pressure, alert storms are real bursts, incidents (interactive) and audits (batch) are real priority classes, and "cluster data never leaves your infrastructure" is a real stay-local rule and the startup thesis.
**Side effects checked:** Track B requirements unchanged (tool-using agent, app-shaped traffic). Existing tools (K8sGPT, HolmesGPT, kagent) cover the agent idea — differentiation is self-hosted serving and grounding, not the agent itself.
**Revisit when:** competitor research (post-course) shows the self-hosting angle is already served.

### D-20 — Environments and the fault catalogue (2026-09-27)
**Choice:** develop and record on a local **kind** cluster (Kubernetes v1.36.4, node image pinned by digest — the same version as the Lambda k3s); run live on the Lambda k3s cluster; no EKS (control-plane cost and 15–20 min create/delete for no course benefit). `faults/` holds 16 scenarios, each a manifest plus `scenario.json` answer key: 12 recordable on kind (crash loop, OOM kill, image pull, unschedulable on resources, unschedulable on constraints, readiness probe, service with no endpoints, missing ConfigMap, failed job, stuck rollout, healthy control, two faults at once) and 4 live-only (GPU slice too large, model not fitting its slice, vLLM KV saturation, runaway S3 writer). Namespaces use neutral team names (`orders`, `reports`, `checkout`…), never the fault name.
**Because:** faults injected on purpose give ground truth; kind costs nothing; neutral names stop the task text from leaking the answer.
**Side effects checked:** metrics-server installed in the lab (pinned v0.9.0) so `resource_usage` has data. `kind`/`kubectl` are pinned, checksum-verified binaries in `.bin/` (not Homebrew).

### D-21 — Backend abstraction and read-only tools (2026-09-27)
**Choice:** tools talk to a `Backend` interface the model never sees: `SnapshotBackend` (recorded dumps; tests and golden set) and `KubectlBackend` (live, `kubectl -o json` with a fixed verb allowlist `get|logs|top|version`). Eight read tools plus `submit_diagnosis`; every argument required, enums where possible, names validated against RFC 1123; results size-capped; Secrets never requested; ConfigMap data dropped. Prometheus is reached only through fixed query presets, never model-written PromQL. An AWS/EKS backend can be added later without changing tools. Runtime is the Python standard library only (amended by D-33: the agent layer uses pinned LangGraph + LangChain; tools, backends and validation stay stdlib).
**Because:** one tool implementation serves fixtures and live clusters identically; read-only by construction is the only acceptable agency for a diagnostic tool (OWASP LLM06); SSH is only transport to the API, never the tool layer.
**Side effects checked:** kubectl returns exit 0 with "unable to retrieve container logs" when a previous container was garbage-collected — the backend now reports that as unavailable (real behaviour kept in fixtures).

### D-22 — Agent loop: ported harness, two task types (2026-09-27)
**Choice:** `doctor/agent.py` keeps the trip planner's guards (forced tool calls, exact-repeat refusal, per-tool budgets scaled by namespaces, submit-now nudge, repairs) with two task types: `investigate` (one namespace, user report, ≤ 16 steps) and `audit` (several namespaces, ≤ 30 steps).
**Because:** the harness was the difference between 3 % and 69 % on the predecessor; audits create the batch tenant the gateway needs.

### D-23 — Cluster card as the shared per-cluster prefix (2026-09-27)
**Choice:** a fixed "cluster card" (Kubernetes version, nodes with capacity/GPUs/taints, namespaces) sits between the ruleset and the task. It carries inventory only, never workload state.
**Because:** identical for every task on a cluster → prefix-cacheable and a routing key for the gateway (the city-brief pattern); inventory-only means it cannot give away an answer.

### D-24 — Evidence refs, grounding validation, fail closed (2026-09-27)
**Choice:** every item a tool returns carries a stable ref (`st-/ev-/ds-/lg-/rs-/mt-/cs-`). A diagnosis must cite refs; the validator recomputes every possible ref from the same backend and rejects invented refs, invented objects and out-of-scope namespaces, with up to two repairs. After that the loop **fails closed**: the user gets `inconclusive` ("escalate to a human"), never an ungrounded diagnosis.
**Because:** a confident wrong diagnosis on a production cluster is worse than none; this closes the predecessor's D-18 gap and makes "0 ungrounded answers delivered" true by construction.

### D-25 — What the gateway sees (2026-09-27)
**Choice:** every request carries `X-Request-Id` (task-run-step), `X-Tenant`, `X-App: cluster-doctor`, `X-Priority: interactive|batch` (investigate|audit) and `X-Data-Class: restricted`.
**Because:** the gateway's admission, priority and overflow policy need them; `restricted` means the request may never overflow to a vendor.

### D-26 — Same-day cluster cost via OpenCost (2026-09-27)
**Choice:** install OpenCost (chart 2.5.32, app 1.121.3, pinned) in the bootstrap, reading the existing Prometheus, with custom pricing that puts the whole instance's $1.99/h on the GPU. The doctor reads it through `cost_report(cluster_by_namespace_24h)`.
**Because:** Cost Explorer lags ~24 h and never sees a non-AWS cluster; OpenCost gives a same-day signal. It likely prices HAMi half-GPU slices as whole GPUs — check on first boot and correct by slice share if so.

### D-27 — AWS: S3 runaway-writer scenario and Cost Explorer via ccexplorer (2026-09-27)
**Choice:** scripts (not executed) create one private bucket with 1-day object expiry, a read-only IAM user for the doctor (Cost Explorer read + list on that bucket) and a put-only user for the writer pod; the live-only `runaway-s3-writer` scenario writes ≤ 20,000 tiny objects. The doctor reads bucket stats same-day and real AWS cost/anomalies next day through the user's `ccexplorer` CLI (flags verified against its source).
**Because:** end-to-end behaviour on real AWS for well under $1 per run; AWS keys never reach the model.

### D-28 — Golden set, checker and reference solver (2026-09-27)
**Choice:** `evals/build_golden.py` builds 12 investigate tasks (one per recorded scenario) and 2 audits from the answer keys. The checker adds, to validation: correct status, every injected fault found on the right object (or a pod it owns) with an allowed category, evidence of an expected type, no finding on a healthy object, tools called, step cap. A reference solver builds a grounded diagnosis per task from tool results only; every reference must pass before a model is scored (enforced in CI).
**Because:** the answer key comes from injection, not a model; the solver proves key, tools and checker agree.

### D-29 — Token measurements and capacity (2026-09-27)
**Measured:** prefix 2,488 tokens, cluster card 90 (kind), investigation unique median 1,932 (max 2,908), audit 4,763–8,175, context ≤ 10.8k. With Qwen3-8B's 144 KiB/token and ≈ 79.7k KV tokens per slice, investigations alone bind KV and decode slots together near 32; audits make KV the first limiter. See `design/capacity-qwen3-8b.md`.

### D-30 — Engineering practices (2026-09-27)
**Choice:** stdlib-only runtime; pinned tools (kind, kubectl, metrics-server manifest checksum, node image digest, container images by digest, Helm charts, vLLM image digest, model revisions, ruff, CI actions by commit SHA); ruff lint; offline pytest suite; CI rebuilds the golden set and fails if references drift; fixtures redacted at record time and scanned in tests; SPEC.md as the hand-off contract.

### D-31 — Faults a dashboard would misattribute: tiers, causal chains, red herrings (2026-09-27)
**Context:** the first catalogue (D-20) was single-hop — the pod's own status names the cause (`OOMKilled`, `ImagePullBackOff`). A dashboard or K8sGPT-style rules find those without a model; they do not justify an LLM.
**Choice:** the catalogue is tiered. **easy** (the 12 originals) · **multi_hop** — the report points at a victim and the cause is elsewhere: an expired upstream certificate surfacing as 502s on the caller; a database OOM-killed while its API crash-loops; a namespace LimitRange injecting a 32Mi limit nobody set; a ResourceQuota silently stopping a scale-out (no pod looks unhealthy); an init container waiting for a renamed service; a sidecar filling ephemeral storage until the pod is evicted · **red_herring** — the obvious suspect is healthy: a frontend trusting the wrong CA bundle while "payments-api" is fine; a liveness probe starved by a 10m CPU limit ("restarts, no errors"); a Service `targetPort` mismatch while endpoints look ready; a pod `dnsConfig` pointing at a dead nameserver while "smtp-relay" is fine · **rightsizing** (D-32). Every error message comes from real software (nginx, busybox, kubelet), none authored. TLS uses a throwaway PKI generated at record time (`lab/make_certs.py`); private keys live only in lab Secrets, never in git or in front of the model.
**Diagnosis contract:** one finding per ROOT cause; `affects` lists the victims; evidence should cover the chain. **Answer key:** roots (with accepted alternatives, e.g. the Deployment or the ConfigMap publishing an expired cert), victims, red herrings, `also_ok` (e.g. naming the workload alongside the LimitRange), traps. **Checker:** root found with an allowed category and evidence type; `chain:` every victim named in `affects`; `no-false-positive` explains whether a wrong finding "blames a victim", "blames a red herring" or "has nothing wrong"; results reported per tier.
**Tools added (still read-only):** LimitRange, ResourceQuota, NetworkPolicy, PVC and Ingress views; container ports, init containers, volumes and DNS config in `describe`; logs per container (init containers and sidecars); `inspect_certificate` over **public** certificates in ConfigMaps (a stdlib DER reader, `doctor/x509.py`, checked against `cryptography`). ConfigMap data stays dropped except values that are public certificates.
**Because:** this is where a model earns its place over rules — connecting symptoms across objects and rejecting the obvious suspect — and the tiers make that measurable: "rules handle tier 1, the model is needed for tiers 2–3" is testable, not a claim.
**Revisit when:** live runs show the model passing multi-hop by luck (e.g. naming the root with the wrong chain) — then weight `chain:` higher or require evidence per link.

### D-32 — Right-sizing (over-provisioning) with traps (2026-09-27)
**Choice:** a `rightsizing` tool reports, per workload container, requests and limits against observed usage (p50/p95/max over a sampled window — 150 s on the lab, 24 h from Prometheus live), the share of samples at the CPU limit, the idle request, and an estimated monthly cost of it (OpenCost's on-prem default prices, labelled as an estimate). A `rightsize` task type (batch priority) asks which workloads are over-provisioned and what their requests should be; findings carry `resize` (new requests). The lab scenario has one genuinely over-provisioned workload (300m/256Mi requested, ~1m/3Mi used), one **trap** that looks cheap but is pinned at its CPU limit (throttled — cutting it would hurt; flagging it as `cpu_throttling` is fine), and one right-sized control.
**Checker:** the over-provisioned workload must be found with `resize` inside a safe band (below the current request, not below observed usage); the throttled workload flagged `overprovisioned` fails (`trap:`); the control flagged at all is a false positive.
**Because:** KRR, Goldilocks and OpenCost's efficiency view compute numbers; the model's value is judgement — throttling vs idleness, startup spikes, cost materiality — and linking waste to other teams' pending pods.
**Revisit when:** live 24 h windows are available — widen the scenarios (startup spikes, weekly peaks, autoscaler targets).
**D-29 amended 2026-09-27 (after D-31/D-32):** prefix 3,787 tokens (ruleset 1,202 + tools 2,525), card 112; unique tokens per task median 2,459 (easy) / 3,570 (multi-hop) / 4,768 (red herring) / 5,200 (rightsize), audits 7.7k–11.2k; the multi-hop audit peaks at ~15.1k context. `--max-model-len` raised 16,384 → 24,576 (Qwen3-8B supports 40,960; admission-only limit, KV on demand) and the loop's context stop to 23,000. KV is now unambiguously the first limiter: 32 easy investigations alone would need ~103 % of a worker's pool; the 0.80 line is ≈ 24 easy / 17 multi-hop / 6 audits per worker.

### D-33 — LangGraph orchestration over LangChain tools (2026-09-27; amends D-21's stdlib-only runtime)
**Choice:** the agent is a LangGraph `StateGraph` (`agent` → `act` → back, with `limit` and END exits) over LangChain `StructuredTool`s generated from `tools.json`, calling vLLM or the gateway through `langchain-openai`'s `ChatOpenAI` bound to the **raw `tools.json` dicts** (`tool_choice="required"`, `strict=True`, `enable_thinking:false` in the body, retries off). Guards, expect-blind validation, repairs and fail-closed live in the `act` node; per-step headers (`X-Request-Id`, tenant, app, priority, data class) are set through an httpx request hook. Pins: `langgraph==1.2.12`, `langchain-core==1.6.5`, `langchain-openai==1.6.6` (MIT), locked with `uv.lock`; dev `pytest==9.1.1`. Hosted tracing (LangSmith) is forced off in code.
**Because:** the user wants enterprise appeal and a base for multi-agent work: LangGraph brings explicit bounded control flow, checkpointing (durable, resumable investigations, audit trail), human-in-the-loop interrupts (approval before any remediation) and parallel fan-out — which we would otherwise re-implement. Our domain layer (backends, refs, redaction, validation, checker, golden set) is what differentiates and stays ours.
**Side effects checked (tests, over the real client and a mock server):** request tools byte-identical to `tools.json`; message order system → card → task unchanged (prefix cacheable); headers per step; `max_tokens` is sent as `max_completion_tokens` (vLLM accepts both); gateway 429/503 surface as named stops (retries disabled so sheds are not hidden); malformed tool-call JSON handled via `invalid_tool_calls`. Runtime is no longer stdlib-only — pinned and locked instead.
**Revisit when:** multi-agent roles land (collectors in parallel via fan-out, reviewer, approval interrupt, solutioner) — the graph grows nodes, the tools stay.

### D-34 — First Lambda session: what to measure to back the numbers (2026-09-27)
**Choice:** one GPU session runs `make preflight` → `up` → `deploy` → `kv` → `tunnel` → `golden TAG=baseline` → `sweep` → `metrics` → `down` (steps in `design/lambda-test-plan.md`). Evidence it must produce: measured KV pool vs the paper 79,700 tokens; prompt tokens per step vs the measured prefix 3,787 and per-tier unique sizes; cached share of prompt tokens; golden pass rate per tier; TTFT/latency p50/p95 and `vllm:kv_cache_usage_perc`, waiting and preemptions at concurrency 1/4/8/16/32; DCGM power across prefill-heavy (audits) and decode-heavy phases. The gateway comes later; this run talks to vLLM directly through the tunnel.
**Because:** every capacity and first-limiter claim in `design/capacity-qwen3-8b.md` is paper or tokenizer-measured until vLLM and the GPU confirm it.

### D-35 — Self-healing of the serving stack: planned stretch (2026-09-27)
**Choice (not built):** after the Kubernetes tiers are solid, add remediation for the stack this project runs — vLLM (restart a wedged worker, scale the slice count, roll back a bad flag change, drain a worker whose KV is saturated) and the gateway (restart, shed-threshold rollback). It will run as a separate LangGraph path with a **separate, write-capable identity scoped to the doctor's own namespaces**, every action behind a human-approval interrupt, a dry-run diff first, and a post-action verification step; the read-only investigator is unchanged.
**Because:** "doctor heals its own inference stack" is a strong demo and product story, but write access must never leak into the read-only diagnosis path (INV-3).

### D-36 — The product surface is a command line; a web UI waits (2026-09-27)
**Choice:** `python -m doctor {investigate|audit|rightsize} -n <namespaces> [report]` against a live cluster
(`--context`, `--kubeconfig`; kubectl read-only verbs) or a recorded fault (`--snapshot`). Progress per model call
(latency, prompt/cached/output tokens) and per tool call (arguments, errors, repairs) on stderr; the report or
`--json` run record on stdout; exit 0 healthy · 1 issue · 2 no grounded diagnosis · 64 bad usage. Snapshot runs use
the golden cluster name so their prefix matches golden runs (cache hits). Namespaces are checked against the
cluster before any model call, because kubectl answers a typo with an empty list that would read as "healthy".
**Because:** the live demo needs "diagnose this namespace now"; the rubric scores serving and observability, not
app UI; a CLI is scriptable (cron audits, CI gates by exit code) and adds no server, port or dependency. The agent
gained an `on_update(node, update)` hook (graph `stream` instead of `invoke`, same final record) and tool-call
records now keep their arguments and a rejected submission's validation errors.
**Revisit when:** there is time after the presentation — a localhost-only page that streams the same hook.

### D-37 — Autonomous mode: detect cheaply, diagnose what changed (2026-09-27)
**Choice:** `python -m doctor watch` runs a loop. Every 60 s it scans without a model: problem pods (fingerprinted
by owner), Deployments short of ready replicas, failed Jobs, Services with no ready endpoints, recent Warning events
on non-Pod objects, and running pods with ≥ 3 error-looking lines in their last 30 log lines (a count only). A
namespace is investigated (interactive) when it has a fingerprint (`namespace|Kind/name`) that was not present at
its last diagnosis and its 15-minute cool-down has passed. Resolved fingerprints are forgotten, so a relapse
triggers again; a gateway refusal or transport error is retried on the next scan. Audits (4 namespaces per task,
nightly) and right-sizing (per namespace, weekly) run on a schedule as batch. Output: one JSON line per diagnosis,
and Prometheus text on `:9109` (stdlib server, no dependency). `lab/inject.py` applies faults to a lab cluster (only
namespaces it labels `doctor.lab/managed=true` are ever deleted) to simulate incidents, storms and relapses.
**Because:** the product is autonomous root-cause detection, not a chat box. A cheap scan gates the expensive
model, so GPU time and KV are spent only on change. It also produces realistic app-shaped traffic for the brief
(Part 8): bursts of interactive investigations when many things break at once, and batch audits in the background.
**Side effects checked:** the scan detects 21 of 23 recorded faults and is quiet on the healthy namespace; the
remaining one is over-provisioning, which the schedule covers. Only CamelCase status reasons and counts reach the
report (a user turn), never free text from the cluster; tool results remain the only path for cluster text. The log
count needs one `kubectl logs` per running container per scan (~7 s for 24 namespaces on kind); at larger scale use
`get -A` and a log pipeline or Alertmanager as the trigger. The watcher's metrics are local to wherever it runs;
Prometheus on the node scrapes them only once the watcher runs in-cluster.
**Revisit when:** running in-cluster (image + Deployment with the read-only ServiceAccount + scrape annotation),
Alertmanager webhook as a trigger, notification sink (Slack), and the gateway's priority ordering is in place.

### D-38 — tool_choice "auto" and Qwen3 sampling; "required" broke decoding on vLLM (2026-09-27, first GPU run)
**Observed:** the first Lambda smoke test (`dx-crashloop`, twice) ended `step_cap` with no diagnosis. From step 4 on,
every model call returned exactly `max_tokens` (768) with `finish_reason=length` in ~5.4 s: a tool call that starts
correctly and then collapses into newlines and spaces. It repeats even at temperature 0.7, because the output is
forced by a grammar, not sampled. Cause: `tool_choice="required"` makes vLLM constrain decoding to the tool-call
schema; on vLLM 0.29 + hermes + Qwen3-8B-AWQ that constraint degenerates into whitespace. Five tasks per arm, same
server (`metrics/golden-baseline-20260927-220614.*`, `metrics/golden-fix-check-*`):

| Arm | Pass | Steps at the 768 cap | Time per task |
|---|---|---|---|
| required + strict, greedy (as shipped) | 0/5 | 54 | ~120 s |
| required + strict, Qwen sampling (+ presence penalty 1.5) | 0/5 (1/5) | 26 (16) | ~40–140 s |
| required, no strict, greedy | 0/5 | 54 | ~110 s |
| auto, greedy | 1/5 | 0 | ~10 s |
| auto, Qwen sampling (chosen) | 2/5, then 3/5 on the real code path | 0 | ~9 s |

**Choice:** `tool_choice="auto"`; `strict` removed from `tools.json` (it was never enforced as a schema); Qwen3's
recommended non-thinking sampling `temperature 0.7, top_p 0.8, top_k 20, min_p 0` (its model card warns that greedy
decoding causes endless repetition). The act node already nudges a reply without a tool call; our validators, not a
grammar, guard arguments and the diagnosis.
**Side effects:** runs are no longer deterministic, so golden results need `REPEAT ≥ 2` and are reported as rates.
The serving-side signature of this failure (completion tokens pinned at max_tokens, `finish_reason=length`, flat
~5 s steps) is worth an alert and a slide: it looked like a slow model, but it was a decoding constraint.
**Open:** `dx-crashloop` now answers `config_missing` (the app logs `FATAL: DATABASE_URL is not set`), which the answer
key does not allow. That may be a labelling question, not a model error; left unchanged pending review.

### D-39 — Robustness after the first full baseline; what the baseline says (2026-09-27)
**Observed:** the first full `make golden` (26 tasks × 2) crashed in the second pass. The model sent tool-call
arguments as a JSON *string* holding a Python dict repr (`"{'kind': 'service', …}"`). LangChain accepted it as a tool
call with string args, and message construction raised a pydantic error that escaped the agent. The runner wrote
results only at the end, so the whole run's file was lost.
**Choice:** (1) the chat model turns arguments that are not a JSON object into an invalid tool call, so the model is
told "send exactly one JSON object" and continues; any other unparseable response ends that task with stop
`bad_response`. (2) The runner records a crashing task as a failed row (`stop error_<Type>`) and appends every row as
it finishes. (3) The watcher survives a crashing diagnosis. (4) Scheduled audits group 3 namespaces, not 4
(`dx-audit-2`, 4 namespaces, reached the 23k context budget; 3-namespace audits finished).
**First baseline (pass 1, complete, from the console):** 9/26 = 35 % — easy 6/14, multi-hop 1/7, red-herring 2/4,
right-sizing 0/1. Pass 2 (24 tasks before the crash): 9/24. Failures by kind (pass 1):

| Kind | Tasks | Reading |
|---|---|---|
| Named the victim or a Service, not the root | cascade-db, dns-misconfig, limitrange-oom, mixed, tls-expired, audit-3 | the multi-hop gap: where a stronger model, a reviewer role (C17) or ruleset work pays |
| Category label differs | crashloop → config_missing, no-endpoints → service_misconfig, probe → service_misconfig, port-mismatch → probe_failure | the first two are defensible readings (answer-key question); the last two are wrong |
| Right diagnosis, missed a required tool | init-wait (pod_logs), quota-exhausted (get_events) | `must_call` is a process rule; is it pass/fail or advisory? |
| Failed closed after repairs | imagepull (pass 2: also healthy, job-failed) | grounding rejected the answer — working as designed; inspect the validation errors |
| Context budget / step cap | audit-2 (4 namespaces), rollout | audit size; step efficiency |
| Right-sizing outside the safe band | rightsizing | judgement on the numbers |

**Open (the user decides):** accept `config_missing` for crashloop and `service_misconfig` for no-endpoints; make
`must_call` advisory. Model-side levers, measured one at a time: ruleset wording for the category boundaries, a
larger model (Qwen3-14B-AWQ fits a 20 GiB slice with a smaller KV pool), the reviewer role.

### D-40 — Model profiles, two topologies, a fit calculator and a model matrix (2026-09-28)
**Context:** the first baseline (D-39) raised the question of which model to serve, and a separate review
(`design/model-and-golden-review-2026-09-28.md`) proposed larger candidates. The course asks for GPU slicing and a KV
hop, which need small workers, while the best answers may need a model that only fits the whole card. The model was
hard-coded in four places (manifest, cloud-init, Makefile and agent sampling), so every comparison meant hand edits.
**Choice:**
- **Profiles.** Each model has one file in `deploy/models/`. It holds the pinned checkpoint, the architecture from the
  model's `config.json` (used by the fit), the model-specific vLLM args (tool parser, reasoning parser, load formats) and
  the sampling from the model card. The engine settings every model shares (context 24,576, 32 sequences, 8,192 batched
  tokens, prefix caching, 0.9 memory) live once in `deploy/serving.json`, and a profile may not override them. Comparing
  two profiles therefore compares two models, not two engine configurations.
- **Topologies.** `sliced` runs N workers on HAMi slices of 20 GiB and 50% of the SMs each. It is where we show slicing,
  routing, affinity and the KV hop. `full` runs one worker on the whole card. It is where we compare models on quality
  and where the larger models run. Slicing changes capacity and latency but not the answers, because the weights,
  sampling and context stay the same. So we compare quality on `full`, then place the chosen model on `sliced` if it fits.
- **Fit calculator.** `serving/fit.py` (`make fit`, `make fit-all`) computes the KV pool, KV per token, the recurrent
  state per sequence for hybrid models, how many sequences fit at 24k and at 12k with the prefix shared, and decode and
  prefill floors from active parameters and the SM share. On the 8B slice it predicted 79,699 tokens, and vLLM measured
  79,056 (0.8% lower).
- **Gate.** `make deploy` refuses a pair where one 24k request doesn't fit, because vLLM wouldn't start. A pair that
  fits fewer than two such requests is marked tight.
- **Rendering.** `make deploy MODEL=… TOPO=…` renders the manifest and a weight-prefetch script for the pair. The
  committed `deploy/k8s/vllm.yaml` is the rendered default pair, and a test keeps it and the cloud-init pins equal to
  the default profile.
- **Matrix.** `serving/matrix.py` (`make matrix`) writes `design/model-matrix.md`, and CI fails if it is stale. It is
  generated from the profiles, the `make kv` logs and the golden summaries, never typed in. It has four parts: where
  each model fits, the newest full-set result per model and topology with a 95% interval, a ranking, and every run on
  file. The headline efficiency number is correct diagnoses per GPU-hour, which lets two sliced workers and one
  whole-card worker be compared directly.
- **Plan.** The baseline is Qwen3-8B-AWQ. Round 1 is Qwen3-14B-AWQ, the only upgrade that still fits a slice, and
  Qwen3-30B-A3B-Instruct-2507 in 4-bit, which fits only the whole card. Round 2, if time allows, is Qwen3.5-9B. The
  Qwen3 Coder 30B, Qwen3.6-35B and Ministral 3 14B are paper-only. The matrix lists each with its fit and the reason it
  isn't scheduled.
**Paper fit (24k context, 16-bit KV):**

| Model | Slice | Full card |
|---|---|---|
| 8B | 79.7k tokens (3.2 requests at 24k) | 8.6 |
| 14B | 46.6k (1.9, tight) | 6.7 |
| 30B-A3B | does not start | 8.0; with 3.3B active parameters, its MLP compute is about 2.5× lower than the 8B's, or about 1.7× lower for a whole 15k prefill including attention |
| Qwen3.5-9B, Qwen3.6-35B, Ministral | do not fit | 21, 23, 2.3 (the hybrid state per sequence is an estimate) |

**Because:** the grader should be able to see what we considered, what fits where, what we measured and why we chose
the final pair. Switching models should change one variable, not require four edits.
**Revisit when:**
- a measured pool differs from the paper figure by more than about 5% (recalibrate the activation or CUDA-context estimate);
- we serve a hybrid model (replace the state estimate with vLLM's own report);
- the gateway lands (golden runs through it should record `WORKERS` and the routing policy in the summary).

**Update 2026-09-28, after the first matrix runs:**
- The 30B-A3B pool measured 185,136 tokens against 197,563 on paper, 6.3% lower. That passes the recalibration trigger
  above. The calculator underestimates MoE overhead (fused-MoE workspace, CUDA graphs) by about 1.1 GiB.
- The hybrid state estimate now stores the recurrent state in fp32 and counts (kernel − 1) conv positions, following the
  vLLM v0.29 Qwen3.5 layout. That makes about 49 MiB per sequence for Qwen3.5-9B, up from 25.
- The architecture analysis behind the plan is in `design/model-architecture-guide.md`.

### D-41 — The v2 score and the observation ledger; legacy score kept (2026-09-28)
**Context:** the review reproduced three scoring faults:
- A wrong explanation passed. The port-mismatch answer had the ports reversed and blamed a "Service readiness probe",
  which doesn't exist.
- A defensible reading failed. The crashloop answer said `config_missing`, and the log does say `DATABASE_URL is not set`.
- A cited ref only had to exist. An unrelated Deployment's ref supported a pricing finding, so the README's claim that
  "evidence the tools never returned is rejected" wasn't true.
The step-cap nudge also pushed uncertain runs toward "healthy". Choosing a model on that score would have rewarded the
wrong behaviour.
**Choice (runtime):**
- **Observation ledger.** The agent records every tool result the model receives, per namespace: the refs, and which
  tools succeeded. A cited ref must be in the ledger; existing in the cluster is no longer enough. This also removes the
  validator's re-read of every log at submission, which added unmetered API calls in live mode.
- **Full schema validation.** A small generic validator checks `diagnosis.schema.json` completely: types, enums,
  patterns, lengths and unknown keys. `findings: [null]` now produces a repair message instead of a TypeError.
- **One finding per root.** The validator rejects two findings on the same object, counting a pod or ReplicaSet as
  its owner.
- **`inconclusive` is a valid answer.** It takes no findings and needs a summary of what the model couldn't check.
  `healthy` requires a successful `list_problem_pods` in every namespace. The step-cap nudge now offers `inconclusive`,
  and the ruleset says that running out of steps is never evidence of health.
- **Stops.** A model that chooses `inconclusive` stops as `abstained`, which is kept apart from a fail-closed
  `inconclusive`. Both exit with code 2.
**Choice (scoring):**
- **Both scores on every row.** `pass` is v1, with its rules unchanged, for continuity. `pass_v2` differs in four ways:
  - it judges evidence against the run's ledger;
  - it also accepts a root's `also_accept` categories, and the answer key records why. Only two exist: crashloop
    accepts `config_missing` and no-endpoints accepts `service_misconfig`. Confusing the probe with the port stays wrong;
  - it checks mechanism facts and contradictions, as regexes over `root_cause` and `fix`, for 11 scenarios where a
    wrong explanation is plausible and checkable. For example, a port-mismatch answer must name `targetPort` and 8080,
    and must not move targetPort to 8080 or blame a Service probe;
  - it reports `must_call` and the step count as advisory, so a different valid path doesn't fail but efficiency stays
    visible.
- **Reported with the scores.** Each row carries sub-scores (`parts`: submitted, grounded, status, root, category,
  mechanism, chain, no false positive), abstentions and replayable call records. The summary adds a 95% Wilson interval.
- **Reference trajectories.** Each reference diagnosis records the tool calls that return every ref it cites. The
  build replays that trajectory through the same dispatch and ledger, within the step cap, and the reference must pass
  both scores (INV-8). This proves the answer key is consistent and reachable. It doesn't prove a model could find the
  path unaided.
**Deferred from the review:**
- rebuilding the crashloop and job-failed fixtures so their stated fix would really work (they still `echo` their
  error, which goes against the intent of INV-12);
- requiring evidence for each link of a causal chain;
- held-out variants;
- a separate, larger output budget for the final submission (768 tokens can clip audits);
- evaluations of watch mode.
The v2 mechanism checks are regexes. They are transparent and deterministic but narrow, so a correct explanation in
unusual words can fail. When that happens, widen the regex rather than adding a model as judge.
**Because:** the matrix should rank models on what an operator needs: the right object, a true explanation and a safe
fix, grounded in what the model actually saw. It shouldn't penalise a different route to the same answer.
**Revisit when:** a v2 failure on a live run turns out to be a false negative (widen the regex), or a run shows many
abstentions on faults the tools can observe (tighten the nudge).

### D-42 — The inference gateway (C11): guard, admit, place, queue in Go (2026-10-01, recorded 2026-10-06)
**Context:** the doctor's agent makes 5–16 chained calls per run, and each step's prompt extends the last, so a run's
history is cached only on the worker that served it (95% of prompt tokens on one worker). The brief asks for a
gateway that decides what is refused, where work goes and who waits, with `orch_*` telemetry, and that never sends
restricted data off the box.
**Choice:** a Go gateway in front of the vLLM workers (`gateway/`, design in `design/gateway.md`):
- **Four ordered decisions,** pure and tested in `decide`: guard (400), tenant token quota (429, stays), KV and queue
  shedding (503, may leave), placement, then a per-worker priority queue with deadlines.
- **Placement `prefix_then_load`:** keep a run on the worker holding its history unless that worker is more than 0.25
  load units busier; `least_loaded` and `p2c` are the A/B controls.
- **Warm-up before Ready:** a worker takes traffic only after two probes replay the doctor's real first request, which
  also puts the shared prefix in its cache.
- **Stay or leave:** only a 503 may overflow, a restricted request never does (a value only `MayLeave` builds, plus a
  fuzz test), and the overflow backend is null until one is configured.
- **Delivery:** a public, digest-pinned Docker Hub image (`make gateway-image`); one replica, because the run table,
  reservations and quotas live in memory.
**Because:** the client only understands 429 and 503, so the gateway is where admission, placement and priority can
be decided and measured; keeping a run on its worker is what keeps its history cached.
**Measured:** routing changes placement, not answers (F2, F29); stickiness saves 10% of prefill and 8–11% of step
time at concurrency 4 (F29); up to 16 concurrent runs the KV shed protected vLLM from preemption (F25).
**Revisit when:** more than one gateway replica is needed (partition runs by id, or share the run table), or an
overflow backend becomes available (Superlinked, F22).

### D-43 — Admission sized to each worker's measured KV pool (2026-10-06)
**Context:** the gateway allowed 16 requests in flight per worker, a constant that suited the 8B on an A100 slice
(79,056 tokens of KV). On H100 halves Qwen3.8-27B has 51,092 tokens per worker, about two full contexts, and at 32
concurrent runs vLLM preempted 3 requests per worker and thrashed (findings F24–F26). The constant described the
first card we used, not the one serving.
**Choice:** `make gateway` sets `GW_MAX_INFLIGHT` per model and topology from `serving.fit --gateway-env`: the number of
typical runs (the 12k-token app length, with the 3.8k shared prefix held once) one worker's KV pool holds, between 1
and 16. The pool is vLLM's own measurement once `make kv` has recorded one, the paper estimate before that. That gives
9 for the 8B on an A100 slice, 4 for Qwen3.8 on an H100 half, and the ceiling of 16 on a whole card. The KV shed line
(0.80) and the continuing-run exemption (down to 5% free) are unchanged: with the cap sized to the pool, the
exemption keeps runs alive mid-investigation without overfilling KV.
**Because:** the gateway should hold back what vLLM cannot fit, so overload waits in the gateway's priority queue
instead of being preempted inside vLLM, where it costs recomputation and throughput for everyone.
**Revisit when:** the re-run of the knee (backlog) still shows vLLM preemptions with the sized cap (then tighten the
continuing-run exemption), or runs grow well past the 12k-token app length (size to a larger typical run).
**Measured 2026-10-07 (findings F36), revisit triggered:** on the same H100 SXM5 node, the sized cap (4) cut vLLM
preemptions by ~95% and kept the cache and tail latency better than the old 16, but fewer runs finished at 16 and 32
concurrent runs, because queued requests hit the gateway's queue deadline before the doctor's retries outlasted the
queue. The formula sizes for "no preemption", which is stricter than the workload needs. Next: size for a small
preemption budget (e.g. a cap of about 6–8 on a half), or keep 4 and lengthen the queue deadline for interactive work,
and choose by pass rate and preemptions together.

### D-44 — The doctor waits out gateway refusals, and a client that leaves is not a worker failure (2026-10-06)
**Context:** a 429 or 503 ended the run at once: the client ran with `max_retries 0` (SPEC, the agent's client) so that
"a gateway refusal must surface, not be hidden". So at the knee every shed counted as a failed, undiagnosed run (F27),
and pass rates measured how often the gateway said
"not now" rather than how well the model diagnosed. Separately, cancelling a client mid-request was logged as a worker
`upstream_error` 502, which overstated worker faults (F28).
**Choice:**
- **Retry refusals, visibly.** The agent retries a 429 or 503 on the same step after `max(Retry-After, 1, 2, 4, 8 s)`,
  up to `DOCTOR_REFUSAL_RETRIES` (default 4, about 15 s at most), with the same request id, so the gateway still sees
  the same run and step. Every refusal is recorded on its step with the gateway's reason (`kv_free`, `queue_full`,
  `tenant_tokens`, …) and the wait; the golden summary reports `refusal_reasons` and how many runs waited one out.
  Other errors are not retried. `DOCTOR_REFUSAL_RETRIES=0` restores failing on the first refusal.
- **Client gone.** When the forward fails because the client's own request was cancelled, the gateway records
  `client_gone` with no status (its documented meaning), not `upstream_error` 502.
**Because:** the gateway's refusal already says why and when to come back; an agent that honours it rides out a spike,
and the results keep the refusal visible instead of either hiding it or turning it into a lost run.
**Revisit when:** waits of ~15 s are too long for interactive use (lower the retries for interactive priority), or a
refusal storm suggests retries are amplifying load (add jitter or a per-tenant retry budget).

### D-45 — Five production alerts, thresholds from the system, tested with promtool (2026-10-07)
**Context:** the brief asks for production alerts. The measurements gave each one a reason: KV is the first limiter
(F6, F24), queueing moved into the gateway once admission was sized (F32), vLLM preempted when admission let in too
much (F25, F36), and the restricted-data invariant must never break.
**Choice:** `deploy/observability/alerts.yaml`, embedded verbatim in the node's Prometheus values (a test keeps the copy
equal), with Alertmanager off so firing alerts show on Prometheus's Alerts page:
- `KVCacheSaturated`: KV usage above 0.80, the gateway's own shed line, for 5 minutes;
- `GatewayQueueWaitHigh`: queue wait p95 above 5 s, half the 10 s interactive deadline, for 5 minutes;
- `VLLMPreempting`: any preemption rate for 5 minutes;
- `DoctorInconclusiveRateHigh`: more than 20% inconclusive over an hour, about twice the measured rate;
- `RestrictedRequestOffBox` (critical): any restricted request routed off the box, at once.
Each carries the action to take. `alerts_test.yaml` checks that every alert fires on its condition and stays quiet just
below it, with a pinned, checksum-verified `promtool` 3.14.0 (the node's Prometheus version) in `make test` and CI.
**Because:** an alert is only useful if its threshold means something and it has been seen to fire; thresholds taken
from the gateway's own limits move with them when those are retuned.
**Revisit when:** Alertmanager gets a receiver (route the critical alert to a pager), or the KV cap is retuned (D-43)
and the queue alert starts firing in normal operation.
