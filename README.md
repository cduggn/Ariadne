# cluster-doctor

An autonomous, read-only **root-cause detector for Kubernetes**. It watches a cluster continuously. A cheap
scan with no model call looks at pod and controller status, Services without endpoints, warning events and
error counts in logs. When something changes, a tool-using agent investigates it. The agent follows the
causal chain from the symptom to the object that actually has to change: the frontend's 502 turns out to
be an expired certificate upstream, and the crash-looping API turns out to be an OOM-killed database. It
reports one grounded finding per root cause, naming the victims and citing the tool results. It suggests
fixes but never applies them. Scheduled audits and right-sizing catch what never fails loudly. Inference is
self-hosted (vLLM on a sliced A100, with a gateway to come), so cluster data never leaves your
infrastructure.

```
 every 60 s   scan (no model)  ──new or changed symptom──►  investigate (agent, interactive)  ──►  JSON line + /metrics
 nightly      audit (batch, 4 namespaces per task)                                             ──►  JSON line + /metrics
 weekly       right-sizing (batch)                                                              ──►  JSON line + /metrics
```

Final project for *AI Inference Engineering & Systems Design* (Track B), and the seed of a product.
Start with [`SPEC.md`](SPEC.md). Decisions are in [`design/decisions.md`](design/decisions.md), and how
the project answers the brief is in [`design/course-objectives.md`](design/course-objectives.md).

## Run it autonomously
```
uv run python -m doctor watch --context <ctx>              # scan every 60 s, diagnose what changed, /metrics on :9109
uv run python -m doctor watch --snapshot crashloop,cascade-db --once --metrics-addr ""   # recorded faults, one cycle
```
What it does:
- **Filter.** Each symptom is fingerprinted as `namespace|Kind/name`. A namespace is diagnosed only when
  a fingerprint is new since its last diagnosis. Each namespace has a 15-minute cool-down, and a relapse
  after recovery triggers again.
- **Retries.** A gateway refusal (429/503) or an unreachable model is retried on the next scan.
- **Coverage.** The scan detects 21 of the 23 recorded faults and stays quiet on the healthy namespace.
  Over-provisioning is left to the schedule.
- **Safety.** Only CamelCase status reasons and counts reach the prompt, never free text from the cluster.
- **Output.** One JSON line per diagnosis goes to stdout and `--out`. Prometheus metrics include
  `doctor_detections_total`, `doctor_diagnoses_total{mode,status,stop}`, `doctor_findings_total{category}`,
  diagnosis seconds, and prompt/cached/completion tokens.

## Ask it directly
```
uv run python -m doctor investigate -n inventory "stock-api keeps restarting"
uv run python -m doctor audit -n orders,pricing,finance --context lambda
uv run python -m doctor rightsize -n analytics --json --out run.json
```
Exit codes are 0 healthy, 1 issue, 2 no grounded diagnosis. The model is at `DOCTOR_BASE_URL` (default
`http://127.0.0.1:8000/v1`).

## Offline (no GPU)
```
make tools && uv sync          # pinned kind + kubectl; pinned LangGraph/LangChain (uv.lock)
make lint test                 # 47 tests over recorded fault snapshots
make golden-build              # 26 golden tasks (easy, multi-hop, red-herring, right-sizing); references must pass
make lab-up lab-record lab-down   # re-record the fault lab on kind (~25 min)
```

## On the Lambda GPU
The full test plan is in [`design/lambda-test-plan.md`](design/lambda-test-plan.md). The node is billed
from `make up` to `make down`.
```
make preflight && make up && make deploy && make kv
make tunnel             # terminal 2: model at localhost:8000
make kubeconfig && make k8s-tunnel     # terminal 3: k3s API at localhost:6443
make watch              # terminal 4: the doctor, autonomous
make inject FAULTS=crashloop,cascade-db,port-mismatch,tls-truststore STAGGER=60   # break things, watch it find them
make golden TAG=baseline && make sweep && make metrics
make heal && make down
```

## Layout
| Path | What |
|---|---|
| `doctor/` | watcher; agent (LangGraph over LangChain tools); read-only tools with evidence refs; backends (kubectl, snapshots); validation; redaction; certificates; CLI |
| `faults/` | 27 injected faults in tiers (easy, multi-hop, red-herring, right-sizing, live-only) with answer keys |
| `lab/` | kind config, pinned tool fetcher, snapshot recorder, fault injector |
| `fixtures/` | recorded, redacted cluster snapshots |
| `evals/` | golden-set builder with reference solver, checker, runner (also the load generator) |
| `deploy/` | Lambda bootstrap (k3s, HAMi, Prometheus, Grafana, DCGM, OpenCost), vLLM manifest, doctor RBAC, AWS lab scripts |
| `design/` | decisions, architecture, capacity, test plan, course mapping |

## Safety
- **Read-only.** The doctor is read-only by construction: the only kubectl verbs it uses are
  `get|logs|top|version`, and its RBAC has no Secrets and no writes. Only `lab/` writes, and only to lab
  clusters.
- **Secrets.** Secrets are redacted before anything is stored or shown to a model.
- **Prompt injection.** Log lines that try to instruct the model are flagged, not obeyed.
- **Grounding.** A diagnosis that cites evidence the tools never returned is rejected. After two repairs
  the answer is `inconclusive`, never a guess.
