# Agent guide — cluster-doctor

Read [`SPEC.md`](SPEC.md) first — it is the source of truth. Why each choice was made:
[`design/decisions.md`](design/decisions.md). Numbers: [`design/capacity-qwen3-8b.md`](design/capacity-qwen3-8b.md).

## Commands
- `make lint test` — must pass before any commit. `make golden-build` after changing tools, schemas, faults, fixtures or the checker.
- `make lab-up lab-record` re-records fixtures on the local kind cluster only.
- GPU work goes through the Makefile and the `lam` CLI. Launching costs money: never run `make up` without the user's say-so; always end with `make down`.
- AWS scripts in `deploy/aws/` are dry runs unless `APPLY=1`; never apply without the user's say-so.

## Rules
- Keep SPEC.md in step with the code in the same change; a new non-trivial choice gets a `D-n` entry.
- Respect SPEC §3 invariants — above all: read-only (no mutating verbs, no Secrets), grounded diagnoses only, fail closed, no answer leakage into task text or the cluster card, redaction before storage.
- Runtime is the Python standard library only; pin anything new exactly (images by digest, charts by version, actions by SHA).
- Never hand-edit `fixtures/`; re-record with `lab/record.py`.
- Show the user a design (choice, reasons, blast radius) before a large change.
