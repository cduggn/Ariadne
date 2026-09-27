# Course objectives — how the cluster doctor answers the final-project brief

Brief: "Design the cluster and serve an app" (AI Inference Engineering & Systems Design). Office-hours
guidance (2026-09-27): graded on **instrumentation and explanation under stress**, not app polish;
present the repo, an architecture diagram, the dashboards, and a notebook answering the questions.

Status: ✅ built and tested offline · 🟡 built, needs the GPU or the gateway to produce evidence · ⬜ to build.

## Parts of the brief

| Part | What the brief asks | How this project answers | Evidence (where) | Status |
|---|---|---|---|---|
| 0 App | Track A or B; app-shaped traffic | Track B: read-only cluster doctor; investigations (interactive) and audits (batch) | `doctor/`, `faults/`, golden set | ✅ |
| 1 Capacity on paper | max seqs at max_len and at app length; bytes/token; first limiter | 144 KiB/token; ≈ 26–40 investigations or ≈ 9 audits per slice; KV binds first once audits mix in | `design/capacity-qwen3-8b.md`, D-29 | ✅ paper · 🟡 `make kv` |
| 2 Cluster design | GPU, model, topology, concurrency, hop backend, overflow target, scaling pool | A100 40 GB, 2 HAMi slices, Qwen3-8B-AWQ, `--max-num-seqs 32`; overflow never for restricted data; hop optional (office hours) | `design/architecture.md`, D-2/D-16 (inherited), D-25 | 🟡 overflow target + scaling pool in gateway design |
| 3 Guard, admit, stay vs leave | `inspect`, `should_shed`; 429/500/slice_oom stay, 503/529 may leave | Gateway (Go). Doctor side: headers carry tenant, priority, restricted data class | D-25; gateway repo | ⬜ gateway |
| 4 Place | `pick` + policy | pack:cluster affinity + per-run stickiness, bounded; P2C | gateway repo | ⬜ gateway |
| 5 Queue (notebook with scrapes) | who waits where; preemption; chunked prefill flags; abort on disconnect | Gateway queue with deadlines; vLLM flags inherited | notebook | ⬜ |
| 6 Hop and warm | record a hop or prove warm-up and re-quote TTFT | Warm-up proof on vllm-1 (hop store optional per office hours) | notebook | ⬜ |
| 7 Wire the app | smoke the engine first | `make up/deploy`, `make golden ONLY=dx-crashloop` | Makefile | 🟡 |
| 8 Proof under app traffic | real app traffic, mixes | `evals.run_golden --concurrency N --repeat M`: interactive + batch mix | metrics/, plots | 🟡 |

## The presentation questions (brief) → where the answer comes from

| Question | Answer source |
|---|---|
| What is the app; which tokens are shared vs unique? | Ruleset + tool schemas (2,488) and cluster card shared; tool results (logs/events) unique — `design/capacity-qwen3-8b.md` |
| What dies at guard vs admit vs place vs queue? | gateway `orch_*` counters by reason, per run |
| Where do I prevent work that will time out? | gateway queue deadline filter (`timeout_queue`) |
| Where do I protect KV? | gateway KV line (0.80 on `vllm:kv_cache_usage_perc`) + the capacity arithmetic above |
| Where do I prioritise interactive traffic? | `X-Priority` → gateway queue ordering; audits shed first |
| Where do I stop one tenant owning the GPU? | per-tenant token quota (429, stays local) |
| Where do I hop / what is not copied? | warm-up proof (hop optional) |
| Where do I evict / ghosts? | stickiness bound + vLLM prefix-cache counters |
| Engine scheduler vs my admit/place/queue? | vLLM orders execution; gateway orders admission |
| What limited concurrency? | measured: KV first with audits in the mix (D-29) |
| Four production alerts | KV > 0.8 sustained; queue wait p95 > SLO; inconclusive rate > x %; restricted request routed off-box (must be 0) |
| Which pool scales? | decode slots vs uncached prefill tokens, from the sweep |
| 10× traffic — what changes, which knobs are wrong? | notebook recommendations section |

## Office-hours telemetry rows

| Row | Source | Status |
|---|---|---|
| Time per layer (app → gateway → worker) | `X-Request-Id` across hops; gateway per-stage timings; runner step latency | 🟡 |
| TTFT, p50/p95 across concurrency levels | concurrency sweep through the gateway | ⬜ |
| Rejected vs admitted vs shed, and when | gateway counters over time; runner `http_refusals` | ⬜ |
| Pod A vs pod B placement | gateway pick counters | ⬜ |
| Errors by type (placement / overflow / shedding) | gateway counters by reason | ⬜ |
| Shared vs unique tokens, prefix hits | measured split (D-29) + `vllm:prefix_cache_*` + runner `cached_share_of_prompt` | 🟡 |
| DCGM power, memory across prefill- vs decode-heavy phases | DCGM exporter (bootstrap) + audits (prefill-heavy) vs investigations | 🟡 |
| Recommendations | notebook closing section | ⬜ |
