# cluster-doctor

A read-only Kubernetes **cluster doctor**: tell it what looks wrong in a namespace, or ask it to audit
several, and a tool-using agent gathers evidence (pod status, events, specs, logs, usage, cost) and
returns a grounded diagnosis — each finding cites the exact tool results it rests on and suggests a fix
it never applies. Inference is self-hosted (vLLM on a sliced A100 behind a Go gateway), so cluster data
never leaves your infrastructure.

Final project for *AI Inference Engineering & Systems Design* (Track B), and the seed of a product.
Start with [`SPEC.md`](SPEC.md); decisions in [`design/decisions.md`](design/decisions.md); how it
answers the brief in [`design/course-objectives.md`](design/course-objectives.md).

## Quick start (offline, no GPU)
```
make tools          # pinned kind + kubectl into .bin/ (checksums verified)
make lint test      # 20 tests over recorded fault snapshots
make golden-build   # 14 golden tasks; every reference diagnosis must pass the checker
```

## Record the fault lab yourself
```
make lab-up         # kind cluster (Kubernetes v1.36.4) + metrics-server
make lab-record     # inject 12 faults, wait until each settles, record redacted snapshots to fixtures/
make lab-down
```

## Run against a model
```
make up && make deploy          # Lambda A100 via the `lam` CLI — billed from launch
make tunnel                     # terminal 2: localhost:8000 → vllm-0
make golden TAG=baseline        # or BASE=<gateway url>, CONC=8 REPEAT=5 for load
make down
```

## Layout
| Path | What |
|---|---|
| `doctor/` | backends (snapshot, kubectl), read-only tools with evidence refs, redaction, cluster card, agent loop, validation, schemas, triage ruleset |
| `faults/` | injected-fault manifests and their answer keys |
| `lab/` | kind config, pinned tool fetcher, snapshot recorder |
| `fixtures/` | recorded, redacted cluster snapshots |
| `evals/` | golden-set builder with reference solver, checker, runner |
| `deploy/` | Lambda bootstrap (k3s, HAMi, Prometheus, Grafana, DCGM, OpenCost), vLLM manifest, doctor RBAC, AWS lab scripts |
| `design/` | decisions, architecture, capacity, course mapping |

## Safety
Read-only by construction (kubectl verbs `get|logs|top|version`; RBAC without Secrets or writes).
Secrets are redacted before anything is stored or shown to a model. Log lines that try to instruct the
model are flagged, not obeyed. A diagnosis that cites evidence the tools never returned is rejected;
after two repairs the answer is `inconclusive`, never a guess.
