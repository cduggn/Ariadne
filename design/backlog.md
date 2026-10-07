# Backlog: after the course requirements are met

Ideas and fixes found while running the project. None of these block the submission. Each one says why it is here and
roughly what it costs. Remove an item when it lands, and record it in `decisions.md` if it changes a decision.

## Agent quality (likely to lift the pass rate)
- [ ] **Make `crashloop` and `job-failed` repairable (SPEC open issue 6).** Both manifests `echo` an error and `exit`,
  so the fix the golden set names would not repair them. Diagnosis is unaffected.
- [ ] **`describe` accepts any case for `kind`.** The model sends `Deployment`, but the tool accepts only `deployment`.
  The 8B baseline on 2026-09-28 had 101 such rejections across 52 runs, and the 30B-A3B had 18. Normalise to
  lowercase in the tool. About 15 min, plus a test.
- [ ] **Too many evidence items no longer fail a correct diagnosis.** The schema allows at most 8 evidence refs. The
  8B cited 9–11 and failed closed after two repairs (24 times in the 8B baseline; the 2026-10-04 smoke run found the
  right root cause and still scored 0). On the last repair, keep the first 8 refs instead. About 30 min. It changes
  the scores, so re-run the golden set afterwards.

## Gateway
- [ ] **Tune the KV-sized cap (D-43, findings F36).** Cap 4 kept vLLM healthy but finished fewer runs than cap 16 at 16
  and 32 concurrent runs, because queued requests hit the queue deadline. Try a cap of 6–8 on an H100 half (or size
  `gateway_env` for a small preemption budget instead of none), or keep 4 with a longer interactive queue deadline.
  Experiment on one node: `make sweep WORKERS=2 LEVELS="16 32" REPEAT=1` per setting, comparing pass rate,
  preemptions, cached share and step p95 with F36's table. About 15 min of GPU per setting.
- [ ] **Overflow to Superlinked's hosted API** (stay or leave, end to end). The course's
  `class-code/class10/router/overflow.py` posts OpenAI chat completions to `https://api.superlinked.com/v1` with a
  Bearer key, leaves only on 503/529, and caps requests with `OVERFLOW_MAX_REQS`. Our `MayLeave` already makes the
  same decision; the backend is null. First check whether the endpoint accepts `tools` and which models it serves (the
  course uses Qwen3.5-4B). Then build `forwardOverflow` with model rewrite, a key from a Kubernetes Secret the user
  creates, a run that has left stays remote, `orch_overflow_total{result=sent|failed|capped}`, Go tests against a fake
  remote, and a saturation demo. About half a day. Blocked until Superlinked fixes billing (findings F22).
- [ ] **Confirm the KV hop moves the KV** (`design/gateway.md` "KV hop"). On 2026-10-07, 8 hops completed the
  protocol on an H100 with none failed (findings F35), but nothing showed the destination pulling the KV instead of
  recomputing it. Next session, force moves with the tunnel up and compare the hopped step's `cached_tokens` with its
  `prompt_tokens`, and read the `hop` latency stage. Then replace the guessed `GW_HOP_TRANSFER_BYTES_PER_S` and the
  paper prefill rate with measured ones.
- [ ] **`make gateway` restarts the pod twice when the policy changes** (`set env` and `rollout restart`), and it
  uploads stale files left in `.cache/gateway/`. Restart only when nothing else changed, and stage the upload in a
  clean folder.

## Serving and infrastructure
- [ ] **A hybrid-model correction in `serving/fit.py` (SPEC open issue 8, F5).** The paper estimate was 20–34%
  optimistic for Qwen3.8. Until it is corrected, size a hybrid model from vLLM's measured pool, as `make preflight` does.
- [ ] **vLLM v0.30.0.** The `-cu129` image crashes on start (vllm-project/vllm#59157: torch cu130 with a cu129
  torchvision). Upgrade once a fixed image is out, or try the cu130 image after checking the node driver.
- [ ] **OpenCost prices every node at $1.99/hr** (the A100). Set the price per GPU class at boot.
- [ ] **First GH200 boot.** The H100 matched `serving.json` (findings F18). On a GH200, check the card's memory size
  (`nvidia-smi`) against `serving.json`, check that HAMi registers the GPU, and check the arm64 OS image (`lam images`).
- [ ] **Qwen3.8 NaN after a prefix-cache hit** (vllm-project/vllm#55766, open on 2026-10-03). Watch the smoke run for
  runs of "!" tokens.

## Running locally on Apple Silicon
- [ ] **vllm-metal against vllm-mlx, a 30-minute check.** Two identical 3.8k-prefix requests (the second should be
  ≥10× faster), the `/metrics` names, 8 concurrent requests, and `cached_tokens` in usage. vllm-metal is the official
  port and may need no changes. vllm-mlx turns off the prefix cache for hybrid models (waybarrios/vllm-mlx#730), has no
  `cached_tokens`, and names its metrics `vllm_mlx_*`.
- [ ] If vllm-mlx: map its metric names in `gateway/internal/fleet/scrape.go`, and treat a missing `cached_tokens` as
  unknown rather than a miss.

## Production notes (presentation slide, not code)
- Consolidation: move an idle or underused workload to a cheaper card (Karpenter-style), with a hold-down period.
- A shared KV store (Mooncake) keeps the cache warm across a move; it only pays off with large reusable contexts.
- Weights from a persistent volume or object storage instead of a hostPath cache. The gateway already moved from a
  hostPath binary to a registry image.
- Two-GPU topologies: data parallel behind the gateway fits this workload. Tensor parallel only for a model that
  doesn't fit one card. A prefill/decode split doesn't fit a 120:1 workload.
