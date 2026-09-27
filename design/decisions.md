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
**Choice:** tools talk to a `Backend` interface the model never sees: `SnapshotBackend` (recorded dumps; tests and golden set) and `KubectlBackend` (live, `kubectl -o json` with a fixed verb allowlist `get|logs|top|version`). Eight read tools plus `submit_diagnosis`; every argument required, enums where possible, names validated against RFC 1123; results size-capped; Secrets never requested; ConfigMap data dropped. Prometheus is reached only through fixed query presets, never model-written PromQL. An AWS/EKS backend can be added later without changing tools. Runtime is the Python standard library only.
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
