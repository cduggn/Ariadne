# First Lambda session — test plan (D-34)

Goal: turn the paper and tokenizer numbers into measured ones, get the first model results per tier,
and produce the telemetry the rubric asks for. About 1.5 h of A100 time (~$3). The gateway is not in
this run: the laptop talks to vLLM through the SSH tunnel.

| # | Command | What it proves (claim → where it is written) |
|---|---|---|
| 0 | `make preflight` | lint, 34 tests, golden references pass and are committed, lam has an API key |
| 1 | `make up` | node bootstraps: k3s, HAMi, Prometheus, Grafana, DCGM, OpenCost, Qwen3-8B weights (`ready.json`) |
| 2 | `make deploy` then `make kv` | **KV pool per worker**: `Available KV cache memory` and `GPU KV cache size` vs paper 10.95 GiB / 79,700 tokens (capacity file) |
| 3 | `make dashboards`, `make grafana` (terminal 3), `make tunnel` (terminal 2) | dashboards live: KV usage, running/waiting, TTFT/ITL, prefix hits, GPU power |
| 4 | `make golden TAG=baseline ONLY=dx-crashloop` | smoke: the LangGraph agent + hermes parser + our tools end to end |
| 5 | `make golden TAG=baseline` | **pass rate per tier** (easy / multi-hop / red-herring / right-sizing); prompt tokens per step vs measured prefix 3,787 and unique sizes; **cached share** |
| 6 | `make sweep REPEAT=2` | TTFT and step latency p50/p95 at concurrency 1/4/8/16/32; `vllm:kv_cache_usage_perc`, waiting, preemptions per level (`metrics/vllm-sweep-*.prom`) — **does KV bind before 32?** |
| 7 | Grafana during step 6 | DCGM power: audits (prefill-heavy, long contexts) vs investigations (decode-heavy) |
| 8 | `make scale N=2` + `make golden TAG=w2` | second HAMi slice serves; KV per worker the same |
| 9 | optional: `make kubeconfig`, `make k8s-tunnel`, `make record-live ONLY=gpu-unavailable` | first live-only fixture from the real GPU node |
| 10 | `make down` | billing stops |

Record results in: `metrics/` (committed), capacity file (measured rows), decisions (D-29/D-34 results).
What would change the design: KV pool far from 79.7k; cached share well below 0.9; pass rate on
multi-hop/red-herring near zero (then the multi-agent reviewer, D-35's sibling, moves up the list).
