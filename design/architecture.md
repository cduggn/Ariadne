# Architecture

```
                         ┌──────────────── cluster-doctor (this repo, laptop or in-cluster) ────────────────┐
 user / alert  ────────► │ agent loop (doctor/agent.py)                                                      │
 (investigate:           │   prompt = triage ruleset + tool schemas │ cluster card │ task │ tool turns…      │
  interactive;           │   tools (doctor/tools.py) ──► Backend ──┬── SnapshotBackend  fixtures/ (tests, golden)
  audit: batch)          │   validate (grounding) → fail closed    └── KubectlBackend   kubectl -o json (read-only)
                         └───────────────┬───────────────────────────────┬──────────────────────────────────┘
                                         │ OpenAI-compatible HTTP        │ read-only data sources
                                         │ X-Request-Id · X-Tenant ·     │  Kubernetes API (RBAC: get/list/watch, no Secrets)
                                         │ X-App · X-Priority ·          │  Prometheus presets (vLLM, DCGM)
                                         │ X-Data-Class: restricted      │  OpenCost (same-day cluster cost)
                                         ▼                               │  S3 lab bucket · Cost Explorer via ccexplorer
             ┌──────────── gateway (Go, separate repo: cduggn/inference-gateway) ─────────────┐
             │ inspect (guard 400) → should_shed (429 tenant / 503 KV·deadline) → pick          │
             │ (pack:cluster affinity, bounded stickiness, P2C) → per-worker queue (deadlines)  │
             │ overflow: 503/529 may leave — never for X-Data-Class: restricted                 │
             │ /metrics: orch_* (class-9 dashboard names), per-stage timings                    │
             └───────────────┬───────────────────────────────────────┬────────────────────────┘
                             ▼                                       ▼
   ┌──────────────── Lambda gpu_1x_a100_sxm4 · k3s · HAMi (2 × 20 GiB slices) ────────────────────┐
   │ vllm-0 (Qwen3-8B-AWQ)            vllm-1 (same flags, warm-up proof before READY)              │
   │ Prometheus · Grafana · DCGM exporter · OpenCost                         [hop store: optional] │
   └───────────────────────────────────────────────────────────────────────────────────────────────┘
```

**Two traffic classes, one GPU.** Investigations are interactive (a person is waiting, p95 matters);
audits are batch (several namespaces, long contexts, can wait or be shed first). Both carry a
restricted data class: cluster data never leaves self-hosted inference — the product's thesis and the
gateway's overflow rule.

**Where the course's four scarce resources show up** (decode slots, KV blocks, hop bandwidth, warm-up):
see `design/capacity-qwen3-8b.md` for the KV arithmetic and `design/course-objectives.md` for evidence.
