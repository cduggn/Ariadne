# Decisions log

One entry per decision: what we chose, why, what we checked, and what would make us revisit it.
Numbering continues from the predecessor project (trip planner, `cduggn/trip-planner-inference`),
whose decisions still govern the serving stack this repo inherits.

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
