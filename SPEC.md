# SPEC — cluster-doctor

Living specification of what the system **is**, precise enough to maintain it or re-implement it in
another language without reading the history.

| | |
|---|---|
| Last updated | 2026-09-27 (initial build, D-19 … D-30) |
| Why things are the way they are | [`design/decisions.md`](design/decisions.md) |
| Course mapping | [`design/course-objectives.md`](design/course-objectives.md) |
| Capacity and measurements | [`design/capacity-qwen3-8b.md`](design/capacity-qwen3-8b.md) |
| Architecture | [`design/architecture.md`](design/architecture.md) |

**Status legend:** ✅ built and tested offline · 🟡 built, not yet run on a GPU or live cluster · ⬜ planned.
**Update rule:** a change is not done until this file matches the code. Why → `decisions.md`; numbers →
the capacity file. Schemas in `doctor/schemas/` are the contracts: link them, never copy them here.

---

## 1. Purpose, scope, non-goals
**Purpose.** A read-only Kubernetes "cluster doctor": given a user's report about a namespace
(*investigate*) or a list of namespaces (*audit*), a tool-using LLM agent gathers evidence and returns a
grounded diagnosis — findings with category, affected object, root cause, cited evidence and a
suggested fix. It is also the app-shaped workload for a self-hosted inference stack (vLLM on a sliced
A100 behind a Go gateway), which is what the course grades.

**In scope.** Read-only investigation of workloads (pods, deployments, replicasets, services,
endpointslices, jobs, events, nodes, configmap names, pod logs, usage); inference/GPU metrics through
fixed presets; cost signals (OpenCost same-day, AWS Cost Explorer next-day, one S3 lab bucket);
deterministic evaluation against injected faults; deploy files for the Lambda GPU node.

**Non-goals.** Changing anything in a cluster (no apply/patch/delete/exec/scale — ever). Reading Secrets
or ConfigMap data. Model-written PromQL or shell. Sending cluster data to third-party model APIs.

## 2. Components

### C1 — Fault catalogue ✅
- `faults/<id>/manifest.yaml` (+ optional `update.yaml` / `notes.md`) and `faults/<id>/scenario.json`
  (schema: `doctor/schemas/scenario.schema.json`).
- 12 recordable on kind: `crashloop, oom, imagepull, pending-resources, pending-constraints, probe,
  no-endpoints, config-missing, job-failed, rollout, healthy, mixed`. 4 live-only (`live_only: true`):
  `gpu-unavailable, gpu-slice-oom, kv-saturation, runaway-s3-writer`.
- Each scenario owns one namespace with a **neutral team name** (INV-7). Answer key fields: `category`,
  `allowed` (acceptable categories), `objects` (the objects to change; per-object `category` for
  multi-fault scenarios), `evidence` (acceptable evidence types), `must_call`, `settle` (recording rule).
- Images pinned by digest: `busybox:1.37.0@sha256:bdf57e5…`, `nginx:1.29-alpine@sha256:5616878…`.

### C2 — Lab and recorder ✅
- `lab/kind-config.yaml`: one node, `kindest/node:v1.36.4@sha256:099e049…` (matches the Lambda k3s version).
- `lab/metrics-server-v0.9.0.yaml` (sha256 `1cec29a5…`), patched with `--kubelet-insecure-tls` for kind.
- `lab/get-tools.sh`: kind v0.33.0 and kubectl v1.37.1 into `.bin/`, checksums verified.
- `lab/record.py`: applies **all** selected scenarios into one cluster (follow-up manifests after
  `rollout status`), polls every 5 s until each scenario's `settle` rule holds (≤ 300 s), waits 20 s for
  metrics-server, then writes `fixtures/cluster.json` and `fixtures/snapshots/<id>.json`, redacting every
  string first. Settle rules: `restarts>=2`, `oom` (≥ 2 restarts and last state OOMKilled),
  `waiting:<Reason>`, `event:<Reason>`, `ready_all`, `job_failed`, `deploy_stalled`
  (Progressing=ProgressDeadlineExceeded). Deletes the namespaces afterwards unless `--keep`.

**Snapshot dump format** (`fixtures/snapshots/<id>.json`, merged with `fixtures/cluster.json`):
```
{"cluster":   {"version": str, "nodes": [Node…], "namespaces": [str…]},          # cluster.json only
 "namespaces":{"<ns>": {"pods":[…], "deployments":[…], "replicasets":[…], "services":[…],
                        "endpointslices":[…], "jobs":[…], "configmaps":[…(no data)], "events":[…]}},
 "logs":      {"<ns>/<pod>/<container>/current|previous": "text"},   # previous only if it was retrievable
 "usage":     {"<ns>": [{"pod", "cpu", "memory"}]},
 "metrics":   {"<preset>/<ns>": {...}},  "aws": {"s3/<bucket>": {...}, "cost/<query>": {...}},
 "recorded":  {"scenario", "context", "time", "kubernetes"}}
```
Raw objects are `kubectl get -o json` items with `metadata.managedFields`, `resourceVersion`,
`selfLink`, `kubectl.kubernetes.io/last-applied*` and `deployment.kubernetes.io/*` annotations removed.

### C3 — Backends ✅ (`doctor/backends.py`)
Interface `Backend`: `objects(kind, ns)`, `logs(ns, pod, container, previous)`, `usage(ns)`,
`namespaces()`, `cluster_info()`, `metric(preset, ns)`, `s3_bucket_stats(bucket)`, `cost(query)`.
Readable kinds: `pods, deployments, replicasets, services, endpointslices, jobs, configmaps, events, nodes`.
- `SnapshotBackend.load(*paths)` merges dumps. Missing logs → `LookupError`; missing metrics/AWS → `Unavailable`.
- `KubectlBackend(context, kubectl)`: runs `kubectl [--context C] <verb> …` with verb ∈ `{get, logs, top, version}`
  (anything else → `PermissionError`), 20 s timeout, `-o json`; logs `--tail=200`; output starting
  `unable to retrieve container logs` → `LookupError`. Optional sources by environment:
  `DOCTOR_PROMETHEUS_URL` (presets `vllm_kv, vllm_queue, vllm_preemptions, gpu_util, gpu_memory`, fixed
  PromQL, namespace substituted after validation), `DOCTOR_OPENCOST_URL` (`/allocation/compute?window=24h&aggregate=namespace`),
  `DOCTOR_S3_BUCKET` (only that bucket; `aws s3api list-objects-v2` count/bytes), `DOCTOR_CCEXPLORER`
  (`ccexplorer get aws -g DIMENSION=SERVICE -s … -e …`; `ccexplorer get aws anomalies -s … -e …`).
- Names are validated with RFC 1123 `^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$` before any call.

### C4 — Tools and refs ✅ (`doctor/tools.py`, schemas `doctor/schemas/tools.json`)
| Tool | Arguments (all required) | Returns |
|---|---|---|
| `list_problem_pods` | namespace | pods where phase ∉ {Running, Succeeded} **or** restarts > 0 **or** not all containers ready **or** a reason is set; each `{ref st-<pod>, pod, phase, ready "r/n", restarts, reason, detail, owner{kind,name}}`, ≤ 20 |
| `get_events` | namespace, object_name ("any" or name prefix), limit 1–20 | events sorted by (non-Warning last, −count, lastTimestamp); `{ref ev-<h6>, type, reason, object "Kind/name", message ≤240, count, last_seen}` |
| `describe` | kind ∈ {pod, deployment, replicaset, service, job, node}, namespace, name | condensed object with `ref ds-<kind>-<name>`; service adds `pods_matching_selector`, `ready_endpoints`, label sets in the namespace |
| `pod_logs` | namespace, pod, previous, tail 1–80 | first container's last lines `{ref lg-<pod>-<c|p><lineIndex>, text ≤300, suspicious?}` |
| `list_resources` | kind ∈ {deployments, services, jobs, pods, configmaps}, namespace | `{ref rs-<singular>-<name>, name, one-line status}` (configmap names only) |
| `resource_usage` | namespace, preset ∈ {pods, vllm_kv, vllm_queue, vllm_preemptions, gpu_util, gpu_memory} | `pods`: `{ref mt-<ns>-<pod>, pod, cpu, memory}`; presets: `{ref mt-<ns>-<preset>, series}` or `{unavailable}` |
| `s3_bucket_stats` | bucket | `{ref cs-s3-<bucket>, count, bytes}` or `{unavailable}` |
| `cost_report` | query ∈ {aws_by_service_7d, aws_anomalies_30d, cluster_by_namespace_24h} | `{ref cs-<query>, source, …}` or `{unavailable}` |
| `submit_diagnosis` | status, findings, summary (`doctor/schemas/diagnosis.schema.json`) | validated by the loop (C7) |

- **Event ref:** `ev-` + first 6 hex of SHA-1 over `ns|involvedObject.kind|involvedObject.name|reason|message`.
- **Owner resolution:** pod → ownerReferences[0]; ReplicaSet → its owner (Deployment) looked up in replicasets; no owner → the pod.
- **Container problem precedence:** waiting (with last-exit detail) > terminated > "Restarted after <lastState reason>" > NotReady.
- **Dispatch:** `call(backend, name, args)` never raises: unknown tool, `TypeError`, `ValueError`, `LookupError`,
  `PermissionError` → `{"error": "…"}` (redacted, ≤ 240 chars).
- **Registry:** `all_refs(backend, ns)` = every ref any tool could return for `ns` (status/describe/list/usage per pod,
  every log line index for retrievable logs, describe/list for workloads, configmap list refs, every event ref,
  node describes, recorded metric presets, recorded AWS keys). `objects_in(backend, ns)` = existing (Kind, name).

### C5 — Redaction and injection flags ✅ (`doctor/redact.py`)
Masks (with `[REDACTED]`): PEM private keys; AWS key ids `(AKIA|ASIA)[0-9A-Z]{16}`; GitHub, Hugging Face and
`sk-` style tokens; JWTs; `Bearer <token>`; `scheme://user:pass@`; `<…password|secret|token|api_key|access_key|private_key…>=value`.
Applied at record time and in every tool result. Log lines matching injection phrases (e.g. "ignore previous
instructions", "you are an AI", "system prompt", "call the tool", "submit_diagnosis") get `suspicious: true` — never removed, never obeyed.

### C6 — Cluster card ✅ (`doctor/card.py`)
`<cluster_card>` block: `cluster: <name> · Kubernetes <version>`, one line per node (role, cpu, memory,
`nvidia.com/gpu` if any, taints), and the namespace list minus `kube-node-lease, kube-public, local-path-storage`.
Inventory only — no workload state (INV-2).

### C7 — Agent loop ✅ (`doctor/agent.py`)
- **Prompt order (INV-1):** `system` = `doctor/packs/triage.md` (tool schemas are rendered into system by
  the chat template) → `user` = cluster card → `user` = task (`{task_type, namespaces, report}`) → tool turns.
- **Request:** `POST {base}/chat/completions` with `tools`, `tool_choice: "required"`, `temperature: 0`,
  `max_tokens: 768`, `chat_template_kwargs: {enable_thinking: false}`; headers `X-Request-Id: <task>-<run8>-s<n>`,
  `X-Tenant` (default `platform`), `X-App: cluster-doctor`, `X-Priority: interactive|batch`,
  `X-Data-Class: restricted`, `Authorization: Bearer $VLLM_API_KEY` if set.
- **Guards (harness on):** exact-repeat refusal keyed on `name + json.dumps(args, sort_keys=True)`; budgets per
  namespace in the task — `list_problem_pods 2, get_events 3, describe 4, pod_logs 4, list_resources 3,
  resource_usage 2, s3_bucket_stats 1, cost_report 2`; at `max_steps − 2` a user message says to submit;
  context stop when prompt+completion tokens > 15,000.
- **Submit:** validate (C8). Pass → accept. Fail → `{accepted:false, errors[≤8], fix}` up to 2 repairs. Third failure →
  **fail closed**: diagnosis = `{status: "inconclusive", findings: [], summary, rejected_submission, validation_errors}`.
- **Stops:** `submitted | inconclusive | step_cap | context_budget | http_<code> | transport_error`.
- `harness=False` disables guards and validation (comparison runs only).

### C8 — Validation (expect-blind) ✅ (`doctor/validate.py`)
Schema; `healthy` ⇔ no findings; each finding's namespace ∈ task namespaces; (kind, name) exists; every evidence
ref ∈ `all_refs(ns)`; non-empty fix. Never reads the answer key (INV-4).

### C9 — Evaluation ✅ (`evals/`)
- `build_golden.py`: 12 `investigate` tasks (`dx-<scenario>`, max 16 steps) + 2 `audit` tasks (`dx-audit-1`:
  orders, status, checkout; `dx-audit-2`: reports, ml, web, billing; max 30 steps). Expected findings from
  answer keys. Reference solver picks refs of the expected evidence types from tool results; every reference
  must pass the checker or the build fails. Writes `evals/golden/tasks.jsonl`, `reference_diagnoses.json`.
- `checker.py`: validation + status + `found:<obj>` (finding on the object or a pod/ReplicaSet it owns) +
  `category:<obj>` + `evidence-type:<obj>` + `no-false-positive` + `tools-called` + `max-steps`; `inconclusive`
  and missing diagnoses fail.
- `run_golden.py`: runs tasks against `--base-url` (vLLM or gateway) with snapshot backends; `--concurrency`,
  `--repeat` make it the load generator; writes `metrics/golden-<tag>-<ts>.jsonl` and `.summary.json`
  (pass rate by type, stop reasons, inconclusive, HTTP refusals, failed rules, tokens, cached share, latency p50/p95).

### C10 — Serving and node ✅ written · 🟡 not yet run for this repo
- `deploy/cloud-init.yaml` (from the predecessor, run there on 2026-09-24/25): k3s v1.36.4+k3s1, Helm v3.22.0,
  HAMi 2.9.0 (split 4), Prometheus chart 29.33.0 (5 s scrape), Grafana (grafana-community) 13.2.5, DCGM exporter
  4.5.2-4.8.1, **OpenCost 2.5.32** (custom pricing GPU $1.99/h), vLLM image `v0.29.0-cu129` by digest,
  model `Qwen/Qwen3-8B-AWQ @ 4da05a8…` prefetched. `KUBECONFIG` exported via `/etc/environment`.
- `deploy/k8s/vllm.yaml`: StatefulSet (vllm-0/1), headless Service, `hami-scheduler`, per pod `nvidia.com/gpumem 20480`,
  `gpucores 50`; flags `--max-model-len 16384 --gpu-memory-utilization 0.9 --max-num-seqs 32 --max-num-batched-tokens 8192
  --enable-prefix-caching --enable-auto-tool-choice --tool-call-parser hermes --enable-prompt-tokens-details --enable-request-id-headers`.
- `deploy/doctor/rbac.yaml`: ServiceAccount + ClusterRole (get/list/watch on the readable kinds, `pods/log`,
  `metrics.k8s.io`; **no Secrets, no writes, no exec**).
- `deploy/aws/`: `setup.sh`/`teardown.sh` (dry-run unless `APPLY=1`), bucket lifecycle (1-day expiry), IAM policies
  (doctor read-only; writer put-only on `runaway/*`).

### Planned ⬜
| Id | Component | Summary |
|---|---|---|
| C11 | Gateway (Go, `cduggn/inference-gateway`) | guard, admit (KV 0.80, deadline, tenant quota), pick (pack:cluster affinity + per-run stickiness, bounded, P2C), per-worker queue, overflow (never for restricted), `orch_*` metrics compatible with the class-9 dashboards |
| C12 | Live fixtures | record the 4 live-only scenarios on the Lambda cluster / AWS |
| C13 | Warm-up proof | vllm-1 not routable until warm; TTFT re-quoted |
| C14 | Notebook + plots + DESIGN.md | concurrency sweep, sheds by reason, pod A/B, DCGM power, recommendations |
| C15 | CVE rehydration (batch tenant) | recorded scanner reports → proposed base-image bumps (post-course) |

## 3. Invariants (change only with a decisions entry)
| Id | Invariant | Enforced by |
|---|---|---|
| INV-1 | Prompt order: ruleset+tools → cluster card → task → tool turns; the first two identical across tasks on a cluster | `tests/test_agent.py::test_prompt_layout_and_headers` |
| INV-2 | The cluster card holds inventory only, never workload state | code review (`doctor/card.py`) |
| INV-3 | Read-only: kubectl verbs ∈ {get, logs, top, version}; no Secrets; ConfigMap data dropped | `tests/test_tools.py::test_live_backend_is_read_only_by_construction`, `deploy/doctor/rbac.yaml` |
| INV-4 | In-loop validation never reads the answer key | `doctor/validate.py` has no access to `expect` |
| INV-5 | No ungrounded diagnosis is delivered: invented refs/objects fail; after 2 repairs → `inconclusive` | `tests/test_agent.py::test_repair_then_fail_closed`, `tests/test_checker.py` |
| INV-6 | Secrets never reach fixtures, models or logs | redaction at record + in tools; `tests/test_redact.py` |
| INV-7 | Task text never names the fault (neutral namespaces, user-voice reports) | fault catalogue review |
| INV-8 | Every reference diagnosis passes the checker | `evals/build_golden.py` (CI) |
| INV-9 | Everything pinned: images/charts/models/binaries/actions | files in §2, CI |
| INV-10 | `X-Data-Class: restricted` on every request; the gateway must never overflow it | `doctor/agent.py`, gateway tests (planned) |

## 4. Pins and key numbers
| Item | Value |
|---|---|
| Model / engine | Qwen/Qwen3-8B-AWQ @ `4da05a8edb55c6046cce958586c33b61da07bb79` · vLLM `v0.29.0-cu129` @ `sha256:7ef5a35d…` |
| Lab | kind v0.33.0 · kubectl v1.37.1 · node v1.36.4 · metrics-server v0.9.0 |
| Tokens (measured) | prefix 2,488 · card 90 (kind) · investigation unique median 1,932 / max 2,908 · audit 4,763–8,175 |
| KV (paper) | 144 KiB/token · ≈ 79,700 tokens per 20 GiB slice · 0.80 line ≈ 32 investigations ≈ 7.5 audits |
| Golden set | 14 tasks (12 investigate, 2 audit); 20 offline tests |

## 5. How to verify
```
make tools                 # pinned kind + kubectl into .bin/
make lint test             # ruff + 20 offline tests over recorded snapshots
make golden-build          # rebuild the golden set; fails if any reference diagnosis fails its checker
make lab-up lab-record     # re-record fixtures on kind (then make golden-build)
make up deploy kv          # Lambda A100 (costs money: ask first), then: make tunnel; make golden TAG=…; make down
```

## 6. Security
Read-only RBAC; no Secrets; redaction at record and read; injection-flagged log lines; AWS keys only in the
environment or a Kubernetes Secret, never in git or prompts; every endpoint ClusterIP behind SSH tunnels;
`restricted` data class never leaves self-hosted inference; CI has `contents: read` only.

## 7. Porting notes (re-implementing in another language)
Preserve exactly: ref formats and the event-ref hash input; owner resolution; the problem-pod predicate and
reason precedence; event sort key; guard key canonicalisation (`name + JSON with sorted keys`); budgets ×
namespace count; validation order and messages' prefixes (`schema`, `consistency`, `scope`, `object-exists`,
`evidence-exists`, `fix`) — the checker and tests key on these prefixes; fail-closed shape. The guard key is
Python `json.dumps(args, sort_keys=True)` with default separators (`", "`, `": "`) and `ensure_ascii=True`;
any canonical form works as long as identical arguments map to identical keys.

## 8. Open issues
1. No model run yet for this app (golden baseline pending on the GPU, via the gateway).
2. Live-only scenarios need recording on the Lambda cluster and AWS (C12).
3. OpenCost pricing units and HAMi half-GPU attribution unverified (D-26).
4. Live logs are longer than lab logs: re-measure unique tokens (D-29).

## 9. Change log
| Date | Change | Decisions |
|---|---|---|
| 2026-09-27 | Initial build: faults, lab recorder, backends, tools, agent, validation, evals, deploy, docs | D-19 … D-30 |
