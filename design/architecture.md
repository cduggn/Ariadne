# Architecture

```mermaid
flowchart LR
    subgraph laptop["Laptop or CI"]
        doctor["<b>cluster doctor</b><br/>agent: LangGraph over read-only tools<br/>watch · investigate · audit · rightsize"]
        evals["golden set<br/>26 recorded faults, v1 + v2 scores"]
    end

    subgraph node["Lambda GPU node · k3s · HAMi"]
        gw["<b>gateway</b> (Go)<br/>guard → admit → place → queue<br/>warm-up · stay or leave · orch_* metrics"]
        subgraph gpu["one GPU, HAMi slices"]
            v0["vllm-0"]
            v1["vllm-1"]
        end
        obs["Prometheus · Grafana · DCGM<br/>OpenCost · 5 alert rules"]
    end

    k8s[("Kubernetes API<br/>read-only, no Secrets")]

    doctor -- "chat completions + headers<br/>(run id, tenant, priority, data class)" --> gw
    evals -. "same client" .-> gw
    gw -- "sticky placement<br/>(prefix_then_load)" --> v0
    gw --> v1
    v0 -. "KV hop, opt-in<br/>(Mooncake)" .- v1
    doctor -- "get · list · logs" --> k8s
    obs -. scrapes .-> gw
    obs -. scrapes .-> v0
    obs -. scrapes .-> v1
```

**One request's path.** The doctor makes 5–16 chained calls per run, each carrying `X-Request-Id`
(task-run-step), `X-Tenant`, `X-Priority` (interactive or batch) and `X-Data-Class: restricted`. The gateway
(`gateway/`, D-42):
1. **guards:** malformed bodies, streaming and client-supplied `kv_transfer_params` get a 400;
2. **admits:** a tenant over its token quota gets a 429, and a worker below 20% free KV, or a request that would miss
   its queue deadline, gets a 503;
3. **places:** a run stays on the worker that holds its history unless that worker is much busier (`prefix_then_load`);
4. **queues:** per worker, in priority order, at most `GW_MAX_INFLIGHT` in flight, sized to the worker's measured KV
   pool (D-43).

A 503 may overflow off the box for non-restricted data only, and the overflow backend is null today.

**Two traffic classes, one GPU.** Investigations are interactive (a person is waiting, p95 matters); audits are batch
(several namespaces, long contexts, can wait or be shed first). Both carry the restricted data class: cluster data never
leaves self-hosted inference, which is the product's thesis and the gateway's overflow rule.

**Hardware.** `make up` takes the first of H100 PCIe, H100 SXM5, GH200 and A100 with capacity. One cloud-init detects
the card:

| GPU | Workers | Model |
|---|---|---|
| A100 40 GB | two 20 GiB slices | Qwen3-8B-AWQ |
| H100 80 GB | two 39 GiB halves | Qwen3.8-27B-FP8 (KV hop on) |
| GH200 96 GB | whole card | Qwen3.8-27B-FP8 |

**Where the course's four scarce resources show up:** decode slots, KV blocks, hop bandwidth and warm-up. See
`design/findings.md` for the measurements, `report/report.ipynb` for the charts, and `design/course-objectives.md` for
how each brief question is answered.
