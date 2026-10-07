# cluster-doctor

An autonomous, read-only **root-cause detector for Kubernetes**. It watches a cluster continuously. A cheap
scan with no model call looks at pod and controller status, Services without endpoints, warning events and
error counts in logs. When something changes, a tool-using agent investigates it. The agent follows the
causal chain from the symptom to the object that actually has to change: the frontend's 502 turns out to
be an expired certificate upstream, and the crash-looping API turns out to be an OOM-killed database. It
reports one grounded finding per root cause, naming the victims and citing the tool results. It suggests
fixes but never applies them. Scheduled audits and right-sizing catch what never fails loudly. Inference is
self-hosted (vLLM on HAMi-sliced GPUs behind a Go inference gateway), so cluster data never leaves your
infrastructure.

```
 every 60 s   scan (no model)  ──new or changed symptom──►  investigate (agent, interactive)  ──►  JSON line + /metrics
 nightly      audit (batch, 4 namespaces per task)                                             ──►  JSON line + /metrics
 weekly       right-sizing (batch)                                                              ──►  JSON line + /metrics
```

Final project for *AI Inference Engineering & Systems Design* (Track B), and the seed of a product.
Start with [`SPEC.md`](SPEC.md). Decisions are in [`design/decisions.md`](design/decisions.md), measured findings in
[`design/findings.md`](design/findings.md), and how
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
make lint test                 # Python tests over recorded fault snapshots, gateway go vet + Go tests, ruff
make golden-build              # 26 golden tasks (easy, multi-hop, red-herring, right-sizing); references must pass both scores
make lab-up lab-record lab-down   # re-record the fault lab on kind (~25 min)
make demo                      # the gateway in front of two fake vLLM workers, golden set at concurrency 8
```

## Models, GPUs and topologies
Two independent settings choose which model serves and how the card is carved (D-40). A topology names its GPU, so a
run's results record the hardware too.

| GPU | Topologies (HAMi) | Boots with (`make up`) |
|---|---|---|
| A100 40 GB | `sliced` 2 × 20 GiB · `full` | Qwen3-8B-AWQ on `sliced`, KV hop off (the measured gateway A/B) |
| H100 80 GB | `h100-half` 2 × 39 GiB · `h100-full` | Qwen3.8-27B-FP8 on `h100-half`, KV hop on |
| GH200 96 GB | `gh200-half` 2 × 46 GiB (HAMi unverified) · `gh200-full` | Qwen3.8-27B-FP8 on `gh200-full` |

```
make models                                    # the profiles in deploy/models/ (pinned checkpoint, vLLM args, sampling)
make fit-all                                   # every model × every topology: KV pool, sequences at 24k, gate
make fit MODEL=qwen3.8-27b-fp8 TOPO=h100-half  # one pair with the derivation
make deploy MODEL=qwen3-14b-awq TOPO=sliced    # render, fetch weights, serve (refuses pairs that cannot start)
make golden MODEL=qwen3-14b-awq TOPO=sliced TAG=14b REPEAT=3 CONC=4
make matrix                                    # design/model-matrix.md: fit, results with 95 % intervals, ranking
```
Every golden row is scored twice (D-41). v1 is the original score. v2 differs in four ways:
- a cited ref must have been shown to the model in that run;
- a category that the evidence supports equally well also counts;
- the explanation must state the real mechanism, so a port-mismatch answer that reverses the ports fails;
- a missing required tool call is reported as a warning instead of failing the task.

## On the Lambda GPU
The full test plan is in [`design/lambda-test-plan.md`](design/lambda-test-plan.md). The node is billed
from `make up` to `make down`.

`make up` takes the first with capacity in any region of H100 PCIe, H100 SXM5, GH200 and A100 (`TYPES=…` to choose). One cloud-init
serves every GPU: it reads the hardware, fetches that GPU's boot model and starts the next one in the background.
`make up` then writes `.cache/node.env` (GPU, model, topology, hop), so the commands below need no flags; anything on
the make line overrides it.
`make help` prints this playbook.
```
make preflight && make up                # prints node: GPU=… MODEL=… TOPO=… HOP=…
make bringup                             # deploy → scale to the topology's workers → kv → gateway → dashboards
make tunnel                              # terminal 2: the gateway at localhost:8000
make grafana                             # terminal 5: Grafana at localhost:3000
make check                               # pods, both workers Ready through the gateway, KV hop on or off
make kubeconfig && make k8s-tunnel       # terminal 3: k3s API at localhost:6443
make watch                               # terminal 4: the doctor, autonomous
make inject FAULTS=crashloop,cascade-db,port-mismatch,tls-truststore STAGGER=60   # break things, watch it find them
make bench TAG=gw                        # golden set + concurrency sweep + metrics, through the gateway
make gateway POLICY=least_loaded && make bench TAG=gw-ll   # control arm of the stickiness A/B
git add metrics && git commit            # before make down: the node's Prometheus keeps nothing
make heal && make down
```
The gateway guards, admits, places and queues every request: tenant quotas (429), KV and queue shedding (503),
priority, per-run stickiness for the prefix cache, warm-up before a worker is Ready, and restricted data never leaving
the box. With the KV hop on, a run moved off its worker has its cache copied over vLLM's MooncakeConnector instead of
recomputed ([`design/gateway.md`](design/gateway.md)). After changing gateway code, run `make gateway-image` and
commit the new digest.

## Layout
| Path | What |
|---|---|
| `doctor/` | watcher; agent (LangGraph over LangChain tools); read-only tools with evidence refs; backends (kubectl, snapshots); validation; redaction; certificates; CLI |
| `faults/` | 27 injected faults in tiers (easy, multi-hop, red-herring, right-sizing, live-only) with answer keys |
| `lab/` | kind config, pinned tool fetcher, snapshot recorder, fault injector, GPU launcher with fallback (`up.sh`), gateway demo |
| `fixtures/` | recorded, redacted cluster snapshots |
| `evals/` | golden-set builder with reference solver and trajectories, v1 + v2 checker, runner (also the load generator) |
| `gateway/` | the Go inference gateway: guard, admit, place, queue (`decide`, `fleet`), proxy (`serve`), `orch_*` metrics, KV hop (`hop`), fake vLLM for tests and `make demo` |
| `serving/` | model profiles → manifests, fit calculator, model matrix, gateway warm-up body |
| `deploy/` | Lambda bootstrap for any GPU (k3s, HAMi, Prometheus, Grafana, DCGM, OpenCost), model profiles, GPUs and topologies, vLLM and gateway manifests, dashboards, doctor RBAC, AWS lab scripts |
| `design/` | decisions, architecture, capacity, test plan, course mapping |

## Safety
- **Read-only.** The doctor is read-only by construction: the only kubectl verbs it uses are
  `get|logs|top|version`, and its RBAC has no Secrets and no writes. Only `lab/` writes, and only to lab
  clusters.
- **Secrets.** Secrets are redacted before anything is stored or shown to a model.
- **Prompt injection.** Log lines that try to instruct the model are flagged, not obeyed.
- **Grounding.** Every tool result the model receives goes into an observation ledger. A diagnosis that cites a ref
  the model was never shown is rejected, even if the object exists. After two repairs the answer is `inconclusive`,
  never a guess. A model that cannot ground a diagnosis may also say so (`inconclusive`), and "healthy" requires
  having checked every namespace.
