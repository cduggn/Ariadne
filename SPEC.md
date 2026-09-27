# SPEC — cluster-doctor

Living specification of what the system **is**, precise enough to maintain it or re-implement it in
another language without reading the history.

| | |
|---|---|
| Last updated | 2026-09-27 (LangGraph agent over LangChain tools: D-33; Lambda test plan: D-34) |
| Why things are the way they are | [`design/decisions.md`](design/decisions.md) |
| Course mapping | [`design/course-objectives.md`](design/course-objectives.md) |
| Capacity and measurements | [`design/capacity-qwen3-8b.md`](design/capacity-qwen3-8b.md) |
| Architecture | [`design/architecture.md`](design/architecture.md) |

**Status legend:** ✅ built and tested offline · 🟡 built, not yet run on a GPU or live cluster · ⬜ planned.
**Update rule:** a change is not done until this file matches the code. Why → `decisions.md`; numbers →
the capacity file. Schemas in `doctor/schemas/` are the contracts: link them, never copy them here.

---

## 1. Purpose, scope, non-goals
**Purpose.** A read-only Kubernetes "cluster doctor". Given a user's report about a namespace
(*investigate*), several namespaces (*audit*) or a cost question (*rightsize*), a tool-using LLM agent
gathers evidence, follows the causal chain from symptom to root cause, and returns a grounded diagnosis:
one finding per root cause, with the victims it affects, cited evidence and a suggested fix it never
applies. It is also the app-shaped workload for a self-hosted inference stack (vLLM on a sliced A100
behind a Go gateway), which is what the course grades.

**In scope.** Read-only investigation of pods, deployments, replicasets, services, endpointslices, jobs,
events, nodes, ConfigMap keys and *public* certificates, LimitRanges, ResourceQuotas, NetworkPolicies,
PVCs, Ingresses, container logs (including init containers and sidecars) and usage; inference/GPU metrics
through fixed presets; right-sizing; cost signals (OpenCost same-day, AWS Cost Explorer next-day, one S3 lab
bucket); deterministic evaluation against injected faults; deploy files for the Lambda GPU node.

**Non-goals.** Changing anything in a cluster from the diagnosis path (no apply/patch/delete/exec/scale — ever; self-healing, if built, is a separate write-scoped path, D-35). Reading Secrets
or non-certificate ConfigMap values. Model-written PromQL or shell. Sending cluster data to third-party APIs.

## 2. Components

### C1 — Fault catalogue ✅ (D-20, D-31, D-32)
`faults/<id>/manifest.yaml` (+ `update.yaml` / `notes.md`) and `faults/<id>/scenario.json` (schema:
`doctor/schemas/scenario.schema.json`). One namespace per scenario, **neutral team names** (INV-7).

| Tier | Scenarios (namespace) |
|---|---|
| easy (12) | crashloop (orders) · oom (reports) · imagepull (storefront) · pending-resources (ml) · pending-constraints (cache) · probe (web) · no-endpoints (checkout) · config-missing (billing) · job-failed (exports) · rollout (catalog) · healthy (status) · mixed (ledger) |
| multi_hop (6) | tls-expired (identity: auth-api cert expired, errors on session-svc) · cascade-db (inventory: stock-db OOM, stock-api victim) · limitrange-oom (finance: LimitRange injects 32Mi) · quota-exhausted (batch: ResourceQuota pods=2 blocks scale-out) · init-wait (accounts: init waits for renamed service) · eviction (media: sidecar fills ephemeral storage) |
| red_herring (4) | tls-truststore (payments: checkout-web trusts legacy CA; payments-api healthy) · throttled-liveness (search: exec probe starved by 10m CPU) · port-mismatch (pricing: Service targetPort 8080 vs 80) · dns-misconfig (notifications: dead nameserver; smtp-relay healthy) |
| rightsizing (1) | rightsizing (analytics: reporting-api over-provisioned; ingest-worker throttled trap; cache right-sized control) |
| live-only (4) | gpu-unavailable · gpu-slice-oom · kv-saturation · runaway-s3-writer (recorded on the Lambda node / AWS later) |

Answer-key fields: `tier`, `task_type`, `category`, `allowed`, `objects` (roots: `kind`, `name`, optional
per-object `category`, `affects` = victims, `alternatives` = equally valid roots, `resize_band`),
`red_herrings`, `also_ok` (may be named without penalty, optionally per category), `forbidden` (traps:
object + categories), `evidence` (acceptable evidence types), `must_call`, `settle`, `tls_setup`,
`usage_window_s`, `hints` (reference-solver only; never shown to the model). Images by digest:
`busybox:1.37.0@sha256:bdf57e5…`, `nginx:1.29-alpine@sha256:5616878…`. All error text comes from real
software (nginx, busybox, kubelet) — none is authored.

### C2 — Lab and recorder ✅
- `lab/kind-config.yaml`: one node, `kindest/node:v1.36.4@sha256:099e049…` (matches the Lambda k3s version).
- `lab/metrics-server-v0.9.0.yaml` (sha256 `1cec29a5…`), patched with `--kubelet-insecure-tls` for kind.
- `lab/get-tools.sh`: kind v0.33.0 and kubectl v1.37.1 into `.bin/`, checksums verified.
- `lab/make_certs.py` (`uv run --with cryptography==50.0.1`): throwaway PKI — internal CA, legacy CA,
  payments-api (valid 1 y), auth-api (expired yesterday); ECDSA P-256; written to a temp dir only.
- `lab/record.py`: (1) creates every scenario namespace, records `fixtures/cluster.json`; (2) runs scenarios
  in batches (default 4): TLS setup (Secrets and public-CA ConfigMaps), apply, poll every 5 s until **all**
  settle rules hold (≤ 420 s), wait 20 s, sample usage every 15 s for `usage_window_s`, dump with
  redaction, delete and recreate the batch's namespaces. Unsettled scenarios are skipped and reported.
  Settle rules: `restarts>=N`, `oom`, `waiting:<Reason>`, `event:<Reason>`, `log:<substring>`, `ready_all`,
  `job_failed`, `deploy_stalled`, `init_stuck` (an init container running ≥ 60 s), `evicted`.

**Snapshot dump format** (`fixtures/snapshots/<id>.json`, merged with `fixtures/cluster.json`):
```
{"cluster":     {"version", "nodes": [Node…], "namespaces": [str…]},                 # cluster.json only
 "namespaces":  {"<ns>": {"<kind>": [objects…]}}   kinds: pods deployments replicasets services endpointslices jobs
                configmaps events limitranges resourcequotas networkpolicies persistentvolumeclaims ingresses
 "logs":        {"<ns>/<pod>/<container>/current|previous": "text"}   # every container incl. init; previous only if retrievable
 "usage":       {"<ns>": [{"pod","container","cpu","memory"}]},
 "usage_series":{"<ns>": [{"t": "…Z", "rows": [{"pod","container","cpu","memory"}]}]},   # right-sizing scenarios
 "metrics":     {"<preset>/<ns>": {...}},  "aws": {"s3/<bucket>": {...}, "cost/<query>": {...}},
 "recorded":    {"scenario", "context", "time", "kubernetes"}}
```
Objects are `kubectl get -o json` items without `managedFields`, `resourceVersion`, `selfLink`,
last-applied and `deployment.kubernetes.io/*` annotations. ConfigMaps keep `dataKeys` and, only for values
that contain `-----BEGIN CERTIFICATE-----` and no `PRIVATE KEY`, `publicCertificates` (INV-11).

### C3 — Backends ✅ (`doctor/backends.py`)
Interface: `objects(kind, ns)`, `logs(ns, pod, container, previous)`, `usage(ns)`, `usage_series(ns)`,
`namespaces()`, `cluster_info()`, `now()`, `metric(preset, ns)`, `s3_bucket_stats(bucket)`, `cost(query)`.
- `SnapshotBackend.load(*paths)` merges dumps; `now()` = the latest recorded time (certificate expiry is judged
  as recorded); `usage_series` falls back to one sample of `usage`.
- `KubectlBackend(context, kubectl)`: verbs ∈ `{get, logs, top, version}` only (`PermissionError` otherwise),
  20 s timeout; logs `--tail=200`; `unable to retrieve container logs` → `LookupError`; `usage` via
  `top pods --containers`; `usage_series` = 24 h at 10-min steps from Prometheus
  (`rate(container_cpu_usage_seconds_total[5m])`, `container_memory_working_set_bytes`) when
  `DOCTOR_PROMETHEUS_URL` is set, else one metrics-server sample. Other optional sources: presets
  `vllm_kv, vllm_queue, vllm_preemptions, gpu_util, gpu_memory`; `DOCTOR_OPENCOST_URL`; `DOCTOR_S3_BUCKET`;
  `DOCTOR_CCEXPLORER` (`ccexplorer get aws -g DIMENSION=SERVICE -s … -e …`, `ccexplorer get aws anomalies -s … -e …`).
- Names validated with RFC 1123 `^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$` before any call.

### C4 — Tools and refs ✅ (`doctor/tools.py`, schemas `doctor/schemas/tools.json`)
| Tool | Arguments (all required) | Returns |
|---|---|---|
| `list_problem_pods` | namespace | pods where phase ∉ {Running, Succeeded}, restarts > 0, not all ready, or a reason is set; reason precedence: pod-level (`Evicted`) > init container not completed (`Init:<reason>`) > container waiting (with last exit) > terminated > `Restarted after <reason>` > `NotReady`; `{ref st-<pod>, pod, phase, ready, restarts, reason, detail, owner}` |
| `get_events` | namespace, object_name ("any" or prefix), limit 1–20 | sorted (Warning first, −count, lastTimestamp); `{ref ev-<h6>, type, reason, object, message, count, last_seen}` |
| `describe` | kind ∈ {pod, deployment, replicaset, service, job, node, configmap, limitrange, resourcequota, networkpolicy, persistentvolumeclaim, ingress}, namespace, name | `ref ds-<kind>-<name>`; pod: containers (image, command, ports, resources, env *names*, envFrom, mounts, probes incl. exec command and timeouts, state), init containers, volumes, DNS policy/config, status reason/message, conditions, owner; service: selector, ports (targetPort), matching pods, ready endpoints; configmap: keys and certificate keys only; limitrange: limits; resourcequota: hard/used |
| `pod_logs` | namespace, pod, container ("" = first app container), previous, tail 1–80 | `{ref lg-<pod>-<container>-<c\|p><lineIndex>, text, suspicious?}`, plus the pod's container list |
| `list_resources` | kind ∈ {deployments, services, jobs, pods, configmaps, limitranges, resourcequotas, networkpolicies, persistentvolumeclaims, ingresses}, namespace | `{ref rs-<singular>-<name>, name, one-line status}` (services: `port->targetPort`; configmaps: keys, has_certificate) |
| `resource_usage` | namespace, preset ∈ {pods, vllm_*, gpu_*} | `pods`: `{ref mt-<ns>-<pod>-<container>, cpu, memory}`; presets: `{ref mt-<ns>-<preset>, series}` or `{unavailable}` |
| `inspect_certificate` | namespace, configmap, key | public certs in the value: `{subject_cn/o, issuer_cn/o, not_before, not_after, dns_names, is_ca, expired, days_left}` judged at `backend.now()`; `ref ct-<configmap>-<key>` |
| `rightsizing` | namespace | per (owner, container) of Running pods: requests/limits (m, Mi), usage p50/p95/max over the series, samples, `cpu_at_limit_share` (samples ≥ 90 % of the CPU limit), `idle_request` = request − p95, `est_monthly_idle_usd` = (idle cores × $0.031611 + idle GiB × $0.004237) × 730 h × replicas; `ref rz-<ns>-<owner>-<container>` |
| `s3_bucket_stats`, `cost_report` | bucket / query | `ref cs-…` or `{unavailable}` |
| `submit_diagnosis` | status, findings, summary (`doctor/schemas/diagnosis.schema.json`) | validated by the loop (C7) |

- **Evidence types by prefix:** st status · ev event · ds describe · lg log · rs resources · mt metrics · ct certificate · rz rightsizing · cs cost.
- **Event ref:** `ev-` + first 6 hex of SHA-1 over `ns|involvedObject.kind|involvedObject.name|reason|message`.
- **Owner resolution:** pod → ownerReferences[0]; ReplicaSet → its owner; none → the pod.
- **Dispatch** never raises: unknown tool, `TypeError`, `ValueError`, `LookupError`, `PermissionError` → `{"error"}` (redacted, ≤ 240 chars).
- **Registry** `all_refs(backend, ns)`: every ref any tool can return for `ns` (per pod: st/ds/rs/mt, mt per container,
  every log line of every container that has logs; ds/rs for every listable kind; ct per public-cert key;
  ds per replicaset; every event; node describes; every rightsizing ref; recorded metric/AWS keys).
  `objects_in(backend, ns)`: existing (Kind, name) for Pod, Deployment, ReplicaSet, Service, Job, ConfigMap,
  LimitRange, ResourceQuota, NetworkPolicy, PersistentVolumeClaim, Ingress, Node.

### C5 — Redaction, injection flags, certificates ✅
`doctor/redact.py`: masks PEM private keys, AWS key ids, GitHub/HF/`sk-` tokens, JWTs, `Bearer` tokens,
`scheme://user:pass@`, `…password|secret|token|api_key|access_key|private_key…=value`; applied at record
time and in every tool result. Log lines matching injection phrases get `suspicious: true`.
`doctor/x509.py`: stdlib DER reader (issuer/subject CN and O, validity, SAN dNSNames, basicConstraints CA),
no signature verification; checked against `cryptography` on the lab PKI. `doctor/quantity.py`: CPU → m,
memory → MiB, percentiles as the sorted sample at index round(p × (n − 1)).

### C6 — Cluster card ✅ (`doctor/card.py`)
`<cluster_card>`: cluster name and version; per node role, cpu, memory, GPUs, taints; namespaces minus
`kube-node-lease, kube-public, local-path-storage`. Inventory only (INV-2).

### C7 — Agent: LangGraph over LangChain tools ✅ (`doctor/agent.py`, `doctor/lc_tools.py`, D-33)
- **Graph:** `START → agent → act → (agent | limit | END)`; `agent` errors → END. State: `messages` (add_messages),
  `n`, `trace`, `steps`, `calls` (append), `repairs`, `seen`, `used`, `diagnosis`, `stop`, `last_tokens`. The backend,
  task, bound model, tools and run id travel in `config["configurable"]`; `recursion_limit = 3 × max_steps + 10`.
  `build_graph(checkpointer=…)` accepts a LangGraph checkpointer (durable runs, future approval interrupts).
- **Tools:** `make_tools(backend)` builds one `StructuredTool` per `tools.json` entry (args_schema = its JSON schema,
  executes `tools.call`). The model is bound to the **raw `tools.json` dicts** — byte-identical on the wire (tested).
- **Model:** `build_llm(base_url, model)` = `ChatOpenAI` (`temperature 0`, `max_tokens 768` → sent as
  `max_completion_tokens`, `max_retries 0`, `extra_body.chat_template_kwargs.enable_thinking=false`) `.bind_tools(TOOLS,
  tool_choice="required", strict=True)`; API key from `VLLM_API_KEY`. Per-step headers via an httpx request hook
  (context variable): `X-Request-Id: <task>-<run8>-s<n>`, `X-Tenant` (default `platform`), `X-App: cluster-doctor`,
  `X-Priority: interactive` (investigate) | `batch` (audit, rightsize), `X-Data-Class: restricted`.
- **Prompt order (INV-1):** `SystemMessage(triage.md)` → `HumanMessage(cluster card)` → `HumanMessage(task)` → tool turns.
- **act node:** `invalid_tool_calls` → error tool messages; no tool call → nudge; `submit_diagnosis` → validate (C8):
  pass → accept; fail → repair (≤ 2) → **fail closed** to `inconclusive`; other tools → guards (exact-repeat refusal on
  `name + json.dumps(args, sort_keys=True)`; budgets × namespaces: `list_problem_pods 2, get_events 3, describe 5,
  pod_logs 5, list_resources 3, resource_usage 2, inspect_certificate 3, rightsizing 1, s3_bucket_stats 1, cost_report 2`)
  → `StructuredTool.invoke`; submit-now nudge at `max_steps − 2`.
- **Limits:** `max_steps` 16 investigate / 12 rightsize / 30 audit; context stop at prompt + completion > 23,000 tokens.
- **Stops:** `submitted | inconclusive | step_cap | context_budget | http_<code> | transport_error`.
- **Hosted tracing** (LangSmith) env vars are forced to `false` at import (INV-13).

### C8 — Validation (expect-blind) ✅ (`doctor/validate.py`)
Schema (incl. `affects` items and `resize` keys); healthy ⇔ no findings; finding namespace ∈ task; root object
exists; every `affects` object exists in the task's namespaces; every evidence ref ∈ `all_refs`; non-empty
fix; `overprovisioned` needs a parseable `resize`. Error prefixes: `schema`, `consistency`, `scope`,
`object-exists`, `evidence-exists`, `fix`, `resize`. Never reads the answer key (INV-4).

### C9 — Evaluation ✅ (`evals/`)
- `build_golden.py`: 22 `investigate` (one per recorded scenario, 16 steps), 1 `rightsize` (12 steps),
  3 `audit` (30 steps: `audit-1` orders+status+checkout, `audit-2` reports+ml+web+billing — easy;
  `audit-3` inventory+pricing+finance — multi-hop). 26 tasks: 14 easy, 7 multi-hop, 4 red-herring,
  1 right-sizing. The reference solver cites refs of the expected evidence types from tool results (using
  solver-only hints for which ConfigMap/container), names victims in `affects`, and sets `resize` to
  max(10m, 2 × p95 rounded up to 5m) / max(16Mi, 2 × p95); every reference must pass or the build fails.
- `checker.py`: validation + `status` + `found:<root>` (root, an alternative, or a pod/ReplicaSet it owns) +
  `category:` + `evidence-type:` + `chain:` (victims named in `affects`) + `resize:` (inside the band) +
  `no-false-positive` ("blames a victim" / "blames a red herring" / "has nothing wrong"; `also_ok` exempt) +
  `trap:` + `tools-called` + `max-steps`. `inconclusive` and missing diagnoses fail.
- `run_golden.py`: runs tasks against `--base-url` (vLLM or gateway) with snapshot backends; `--concurrency`,
  `--repeat`; summary adds `pass_rate_by_tier` and `pass_rate_by_type`.

### C10 — Serving and node ✅ written · 🟡 not yet run for this repo
- `deploy/cloud-init.yaml`: k3s v1.36.4+k3s1, Helm v3.22.0, HAMi 2.9.0 (split 4), Prometheus chart 29.33.0
  (5 s scrape), Grafana (grafana-community) 13.2.5, DCGM exporter 4.5.2-4.8.1, OpenCost 2.5.32 (GPU $1.99/h),
  vLLM `v0.29.0-cu129` by digest, `Qwen/Qwen3-8B-AWQ @ 4da05a8…` prefetched; `KUBECONFIG` via `/etc/environment`.
- `deploy/k8s/vllm.yaml`: StatefulSet vllm-0/1, headless Service, `hami-scheduler`, `nvidia.com/gpumem 20480`,
  `gpucores 50`; flags `--max-model-len 24576 --gpu-memory-utilization 0.9 --max-num-seqs 32
  --max-num-batched-tokens 8192 --enable-prefix-caching --enable-auto-tool-choice --tool-call-parser hermes
  --enable-prompt-tokens-details --enable-request-id-headers`.
- `deploy/doctor/rbac.yaml`: read-only ClusterRole (get/list/watch on the readable kinds incl. limitranges,
  resourcequotas, networkpolicies, persistentvolumeclaims, ingresses; `pods/log`; `metrics.k8s.io`) — no Secrets,
  no writes, no exec.
- `deploy/aws/`: dry-run-by-default `setup.sh` / `teardown.sh`, 1-day lifecycle, read-only and put-only policies.

### Planned ⬜
| Id | Component | Summary |
|---|---|---|
| C11 | Gateway (Go, `cduggn/inference-gateway`) | guard, admit (KV 0.80, deadline, tenant quota), pick (pack:cluster affinity + per-run stickiness, bounded, P2C), per-worker queue, overflow never for restricted, class-9 `orch_*` metrics |
| C12 | Live fixtures | record the 4 live-only scenarios on the Lambda cluster / AWS |
| C13 | Warm-up proof | vllm-1 not routable until warm; TTFT re-quoted |
| C14 | Notebook + plots + DESIGN.md | concurrency sweep, sheds by reason, pod A/B, DCGM power, results per tier, recommendations |
| C15 | CVE rehydration (batch tenant) | post-course |
| C16 | Self-healing of vLLM and the gateway (stretch, D-35) | separate write-scoped identity, approval interrupt, dry-run diff, post-action verification; the read-only path never gains write access |
| C17 | Multi-agent roles | orchestrator (code) → parallel collectors → diagnoser → reviewer → approval → solutioner, as graph nodes over the same tools |

## 3. Invariants (change only with a decisions entry)
| Id | Invariant | Enforced by |
|---|---|---|
| INV-1 | Prompt order: ruleset+tools → cluster card → task → tool turns; first two identical across tasks on a cluster | `tests/test_agent.py::test_prompt_layout_and_headers` |
| INV-2 | The cluster card holds inventory only | `doctor/card.py` review |
| INV-3 | Read-only: kubectl verbs ∈ {get, logs, top, version}; no Secrets | `tests/test_tools.py::test_live_backend_is_read_only_by_construction`, RBAC |
| INV-4 | In-loop validation never reads the answer key | `doctor/validate.py` has no access to `expect` |
| INV-5 | No ungrounded diagnosis is delivered; after 2 repairs → `inconclusive` | `tests/test_agent.py::test_repair_then_fail_closed`, checker tests |
| INV-6 | Secrets never reach fixtures, models or logs | redaction at record and read; `tests/test_redact.py` |
| INV-7 | Task text never names the fault (neutral namespaces, user-voice reports) | catalogue review |
| INV-8 | Every reference diagnosis passes the checker | `evals/build_golden.py`, CI |
| INV-9 | Everything pinned | §4, CI |
| INV-10 | `X-Data-Class: restricted` on every request; the gateway must never overflow it | `doctor/agent.py`; gateway tests (planned) |
| INV-11 | ConfigMap values are dropped unless they are public certificates (no private keys) | `doctor/backends.strip`, `tests/test_tools.py::test_configmap_data_is_dropped_except_public_certs` |
| INV-12 | Fault evidence is produced by real software, never authored text | catalogue review (`faults/*/manifest.yaml`) |
| INV-13 | No hosted tracing or telemetry that could ship cluster data off-site | `doctor/agent.py` forces LangSmith env off; `tests/test_agent.py::test_hosted_tracing_is_forced_off` |
| INV-14 | The model is bound to `tools.json` verbatim; request shape (prompt order, headers) is stable | `tests/test_agent.py::test_wire_format_tools_prompt_order_and_headers` |

## 4. Pins and key numbers
| Item | Value |
|---|---|
| Model / engine | Qwen/Qwen3-8B-AWQ @ `4da05a8edb55c6046cce958586c33b61da07bb79` (40,960 positions) · vLLM `v0.29.0-cu129` @ `sha256:7ef5a35d…` |
| Lab | kind v0.33.0 · kubectl v1.37.1 · node v1.36.4 · metrics-server v0.9.0 · cryptography 50.0.1 (lab only) |
| Tokens (measured) | prefix 3,787 · card 112 · unique per task median: easy 2,459, multi-hop 3,570, red herring 4,768, rightsize 5,200, audits 7.7k–11.2k · max context 15.1k |
| KV (paper) | 144 KiB/token · ≈ 79,700 tokens per 20 GiB slice · 0.80 line ≈ 24 easy / 17 multi-hop / 6 audits |
| Golden set / tests | 26 tasks (14 easy, 7 multi-hop, 4 red-herring, 1 right-sizing) · 34 offline tests |
| Agent stack | langgraph 1.2.12 · langchain-core 1.6.5 · langchain-openai 1.6.6 (locked in `uv.lock`); dev pytest 9.1.1 |

## 5. How to verify
```
make tools                 # pinned kind + kubectl into .bin/
uv sync                    # pinned agent stack (LangGraph, LangChain) from uv.lock
make preflight             # lint + 34 tests + golden references committed + lam API key — before paying for a GPU
make lint test             # ruff + 34 offline tests over recorded snapshots
make golden-build          # rebuild the golden set; fails if any reference diagnosis fails its checker
make lab-up lab-record     # re-record fixtures on kind (~25 min, batches of 4), then make golden-build
make up deploy kv          # Lambda A100 (costs money: ask first) — full sequence in design/lambda-test-plan.md
make golden TAG=… / make sweep   # golden set; concurrency sweep with a vLLM /metrics scrape per level
make kubeconfig k8s-tunnel record-live ONLY=gpu-unavailable   # live-only faults on the Lambda k3s cluster
```

## 6. Security
Read-only RBAC; no Secrets; ConfigMap values dropped except public certificates; redaction at record and
read; injection-flagged log lines; lab private keys only in a temp dir and lab Secrets; AWS keys only in the
environment or a Kubernetes Secret; endpoints ClusterIP behind SSH tunnels; `restricted` never leaves
self-hosted inference; CI has `contents: read` only.

## 7. Porting notes (re-implementing in another language)
Preserve exactly: ref formats (including `lg-<pod>-<container>-<c|p><i>` and `rz-<ns>-<owner>-<container>`)
and the event-ref hash input; owner resolution; the problem-pod predicate and reason precedence; event sort
key; quantity parsing and the percentile rule (sorted sample at index round(p × (n − 1))); the idle-cost formula; `inspect_certificate` judging
expiry at `backend.now()`; guard keys (Python `json.dumps(args, sort_keys=True)` — any canonical form works if
identical arguments map to identical keys); budgets × namespace count; validation and checker message
prefixes (tests key on them); the fail-closed shape; the DER walk order in `doctor/x509.py`.

## 8. Open issues
1. No model run yet for this app — first run planned in `design/lambda-test-plan.md` (D-34), direct to vLLM; the gateway follows.
2. Live-only scenarios need recording on the Lambda cluster and AWS (C12).
3. OpenCost pricing units and HAMi half-GPU attribution unverified (D-26).
4. Live logs are longer than lab logs: re-measure tokens (D-29).
5. In cascade-db the client's error line has no reason text (busybox prints nothing on refused-after-timeout); the database's `OOMKilled` status carries the proof.

## 9. Change log
| Date | Change | Decisions |
|---|---|---|
| 2026-09-27 | Initial build: faults, lab recorder, backends, tools, agent, validation, evals, deploy, docs | D-19 … D-30 |
| 2026-09-27 | Tiered catalogue (multi-hop, red-herring, right-sizing), causal chains, certificate inspection, new kinds, `--max-model-len 24576` | D-29 (amended), D-31, D-32 |
| 2026-09-27 | Agent on LangGraph + LangChain tools; preflight, sweep, live-recording targets; GPU power panel; test plan; self-healing planned | D-33, D-34, D-35 |
