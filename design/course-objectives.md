# Course objectives: how Ariadne answers the final-project brief

The brief is "Design the cluster and serve an app" (AI Inference Engineering & Systems Design). At office hours on
2026-09-27 the course said grading rewards instrumentation and explanation under stress more than app polish. The
presentation shows the repo, an architecture diagram, the dashboards, and a notebook answering the questions.

Status key: ✅ built and measured, 🟡 built with partial evidence, ⬜ not built. The measurements are in
`design/findings.md` (F-numbers) and the charts in `report/report.ipynb` (sections §1–§10).

## Parts of the brief

| Part | What the brief asks | How this project answers | Evidence | Status |
|---|---|---|---|---|
| 0 App | Track A or B; app-shaped traffic | Track B: Ariadne, a read-only cluster doctor. Investigations are interactive; audits and right-sizing are batch. Faults are tiered so the model's value over rules is measurable (D-31) | `doctor/`, `faults/`, golden set (26 tasks) | ✅ |
| 1 Capacity on paper | max sequences at max length and at app length; bytes per token; first limiter | `serving/fit.py` for every model × topology; vLLM's measured pool beside it. KV binds first: two full-length runs per H100 half | F5, F6; report §3; `design/model-matrix.md` | ✅ |
| 2 Cluster design | GPU, model, topology, concurrency, hop backend, overflow target, scaling pool | H100 halves (or A100 slices) under HAMi, Qwen3.8-27B-FP8 (86.5% v2), the in-flight cap sized to KV, an opt-in Mooncake KV hop, overflow decided but no backend (Superlinked blocked on billing), a KEDA-scaled vLLM pool, one to two workers (D-49) | `design/architecture.md`; D-40, D-42, D-43, D-49; F1, F22, F38 | ✅ (overflow backend ⬜) |
| 3 Guard, admit, stay vs leave | `inspect`, `should_shed`; 429/500/slice_oom stay, 503/529 may leave | The gateway's `decide` package: guard 400, tenant 429, KV / deadline / spread 503; only a 503 may leave and a restricted request never does (fuzz test) | `gateway/internal/decide`; F24, F32; report §6 | ✅ |
| 4 Place | `pick` + policy | `prefix_then_load` (bounded stickiness), with `least_loaded` and `p2c` for the A/B | F29; report §4 | ✅ |
| 5 Queue | who waits where; preemption; chunked prefill flags; abort on disconnect | Per-worker priority queue with deadlines; the in-flight cap sized to KV moves waiting from vLLM to the gateway; preemptions and chunked prefill measured; a client abort cancels the upstream call (`client_gone`) | F14, F15, F25, F32, F36; report §5, §7, §10 | ✅ |
| 6 Hop and warm | record a hop, or prove warm-up and re-quote TTFT | Warm-up: a worker takes traffic only after two probes replay the real first request (`orch_warmup_probe_seconds`). Hop: 8 KV hops completed the Mooncake protocol on hardware, transfer itself unconfirmed | D-42; F35 | ✅ warm-up · 🟡 hop |
| 7 Wire the app | smoke the engine first | `make bringup`, then a one-task golden run | Makefile; `design/lambda-test-plan.md` | ✅ |
| 8 Proof under app traffic | real app traffic, mixes | The doctor itself is the traffic: the golden set at concurrency 4–32 through the gateway, mixing interactive investigations and batch audits | F24–F36; report §5 | ✅ |

## Where each presentation question is answered

| Question | Answer and evidence |
|---|---|
| What is the app; which tokens are shared and which unique? | ~3.8k shared prefix (ruleset, tool schemas, cluster card); each step adds a few hundred unique tokens. 87.8% of prompt tokens served from cache (F8–F10; report §1) |
| What dies at guard vs admit vs place vs queue? | `orch_shed_total{reason}`: `kv_free` at admission up to the knee, `timeout_queue` once admission is sized to KV (F24, F32; report §6) |
| Where do I prevent work that will time out? | the queue's deadline check (interactive 10 s, batch 30 s) |
| Where do I protect KV? | the 0.80 KV line and the in-flight cap sized to the measured pool: preemptions down ~95% (F25, F36) |
| Where do I prioritise interactive traffic? | `X-Priority` orders the gateway queue; batch is shed first on spread |
| Where do I stop one tenant owning the GPU? | the per-tenant token quota (429, stays local) |
| Where do I hop, and what is not copied? | the opt-in KV hop copies only blocks the destination lacks; the shared prefix is already there from warm-up (D-42, F35) |
| Where do I evict; ghosts? | stickiness breaks when the bound worker restarts (boot counter), so a run never targets a cache that is gone |
| Engine scheduler vs my admit, place and queue? | vLLM orders execution within a batch (32 sequences, 8,192 tokens per step); the gateway decides who gets in and where (F14, F17) |
| What limited concurrency? | KV: about two full-length runs per H100 half; the knee is between 8 and 16 concurrent runs (F6, F24) |
| Production alerts | five rules, each tested to fire: KV saturated, queue wait high, vLLM preempting, inconclusive rate, restricted off-box (D-45; report §8) |
| Which pool scales? | The vLLM pool. KV per worker is the limiter, so KEDA adds workers, not slots: workers wanted = demand ÷ the cap of 4, plus a trigger on capacity sheds (D-49, D-51; rehearsed on kind, F38; on the H100 a new worker took 8 minutes to arrive, F39) |
| 10× traffic: what changes, which knobs are wrong? | report §9: more workers, a shared run table for a second gateway, an overflow backend, the KV hop; the 768-token cap never bound (F30) and chunked prefill rarely does (F15) |

## Office-hours telemetry rows

| Row | Source | Status |
|---|---|---|
| Time per layer (app → gateway → worker) | `X-Request-Id` across hops; `orch_request_duration_seconds{stage}`; per-step latency in the golden rows | ✅ |
| TTFT, p50/p95 across concurrency levels | vLLM dashboard and the sweep summaries (F24, F36) | ✅ |
| Rejected vs admitted vs shed, and when | gateway counters by reason; runner `refusal_reasons` (report §6) | ✅ |
| Pod A vs pod B placement | `orch_pick_total{pod}`, stickiness outcomes (F29) | ✅ |
| Errors by type (placement / overflow / shedding) | gateway counters by reason; `client_gone` separated from worker errors (D-44) | ✅ |
| Shared vs unique tokens, prefix hits | `orch_prompt_tokens_total{kind}`, `cached_share_of_prompt` (report §1) | ✅ |
| DCGM power, memory across prefill- vs decode-heavy phases | DCGM exporter on the vLLM dashboard; screenshots, since the node's Prometheus keeps nothing after `make down` | 🟡 (no saved time series) |
| Recommendations | report §9 | ✅ |

## Why a model and not rules: the tier story (D-31)

| Tier | What it tests | Dashboards and rule engines |
|---|---|---|
| easy | the pod's own status names the cause | find these without a model |
| multi-hop | the symptom is on a victim and the cause on another object (expired upstream cert, OOM-killed database, LimitRange, ResourceQuota, renamed dependency, sidecar eviction) | point at the victim |
| red herring | the obvious suspect is healthy (wrong CA bundle, probe starved of CPU, Service port, pod DNS) | point at the suspect |
| rightsizing | idle vs throttled vs not worth changing | give numbers, not judgement |

The golden results are reported per tier, so the presentation can show where rules stop and the model starts to matter:
Qwen3.8-27B scores 71% on multi-hop and 100% on red herrings, where the 8B scored 43% and 25% (F1).
