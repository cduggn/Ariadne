# cluster-doctor

A read-only Kubernetes **cluster doctor**: tell it what looks wrong in a namespace, ask it to audit
several, or ask where resources are wasted. A tool-using agent gathers evidence (pod status, events,
specs, logs, certificates, policies, usage, cost), follows the causal chain from the symptom to the object
that actually needs changing — the 502 on the frontend that is really an expired certificate upstream, the
crash-looping API that is really an OOM-killed database — and returns one grounded finding per root cause,
naming the victims, citing the exact tool results, and suggesting a fix it never applies. Inference is self-hosted (vLLM on a sliced A100 behind a Go gateway), so cluster data
never leaves your infrastructure.

Final project for *AI Inference Engineering & Systems Design* (Track B), and the seed of a product.
Start with [`SPEC.md`](SPEC.md); decisions in [`design/decisions.md`](design/decisions.md); how it
answers the brief in [`design/course-objectives.md`](design/course-objectives.md).

## Quick start (offline, no GPU)
```
make tools          # pinned kind + kubectl into .bin/ (checksums verified)
make lint test      # 32 tests over recorded fault snapshots
make golden-build   # 26 golden tasks (easy, multi-hop, red-herring, right-sizing); every reference must pass
```

## Record the fault lab yourself
```
make lab-up         # kind cluster (Kubernetes v1.36.4) + metrics-server
make lab-record     # inject 23 faults in batches, wait until each settles, record redacted snapshots (~25 min)
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
| `faults/` | 27 injected faults in tiers (easy, multi-hop, red-herring, right-sizing, live-only) with answer keys |
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
