# Agent guide — cluster-doctor

Read [`SPEC.md`](SPEC.md) first — it is the source of truth. Why each choice was made:
[`design/decisions.md`](design/decisions.md). Numbers: [`design/capacity-qwen3-8b.md`](design/capacity-qwen3-8b.md).

## Commands
- `uv sync` once; `make lint test` — must pass before any commit; `make preflight` before any GPU session. The product surface is `python -m doctor watch` (autonomous, D-37) plus the one-shot CLI (D-36); keep exit codes, JSON-line fields and metric names stable. Only CamelCase reasons and counts may enter a report built from cluster data. `make golden-build` after changing tools, schemas, faults, fixtures or the checker.
- `make lab-up lab-record` re-records fixtures on the local kind cluster only.
- Models and topologies are data in `deploy/models/*.json` and `deploy/serving.json` (D-40). Never hand-edit `deploy/k8s/vllm.yaml`, which is rendered, or `design/model-matrix.md`, which `make matrix` writes. Every model shares the engine settings, and a profile may not override them (INV-15).
- Keep the v1 score unchanged for continuity, and put changes to what counts as correct into v2 (D-41). A citation must be in the observation ledger.
- GPU work goes through the Makefile and the `lam` CLI. Launching costs money: never run `make up` without the user's say-so; always end with `make down`.
- AWS scripts in `deploy/aws/` are dry runs unless `APPLY=1`; never apply without the user's say-so.

## Rules
- Keep SPEC.md in step with the code in the same change; a new non-trivial choice gets a `D-n` entry.
- Respect SPEC §3 invariants — above all: read-only (no mutating verbs, no Secrets), grounded diagnoses only, fail closed, no answer leakage into task text or the cluster card, redaction before storage.
- Runtime dependencies are pinned and locked (`uv.lock`): LangGraph, LangChain core and OpenAI integration only (D-33). Add nothing without a decision; pin exactly (images by digest, charts by version, actions by SHA).
- Never enable hosted tracing (LangSmith) or any telemetry that sends cluster data off-site (INV-13).
- Bind the model to `tools.json` verbatim; keep the prompt order and headers stable (INV-1, INV-14).
- Never hand-edit `fixtures/`; re-record with `lab/record.py`.
- Show the user a design (choice, reasons, blast radius) before a large change.
