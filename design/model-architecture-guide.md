# Model architecture guide: how each candidate's attention, experts and quantization affect serving

This guide supports the model choice in D-40. It covers six topics, each applied to our candidates and our workload:
- dense models vs mixture of experts (MoE);
- how architecture helps or hurts prefix caching;
- grouped-query attention (GQA) and head counts;
- full attention vs hybrid linear, sliding-window and latent attention;
- weight quantization on the A100;
- which architecture suits which workload.

Our workload is an agentic tool loop with these properties:
- about 120 prompt tokens per useful output token;
- a 3.8k-token shared prefix;
- contexts that grow to 15–23k tokens;
- exact copying of reference strings from earlier tool results;
- an A100 40 GB, split into HAMi 20 GiB slices or used whole;
- vLLM v0.29.0.

Evidence labels:
- `[KB: id]` is course notes. Look a section up with `kb.py show <id>`.
- `[measured: file]` is our own runs in `metrics/`.
- `[hf]` is the model's `config.json` or model card.
- `[web: url, 2026-09-28]` was checked that day against the vLLM v0.29.0 source and docs.
- `[calc]` is arithmetic from those numbers.
- `[general]` is background knowledge that has not been verified here.

Section 9 lists where the course notes are wrong or imprecise.

---

## 0. Summary

| Question | Short answer for our workload | Evidence |
|---|---|---|
| Dense or MoE? | Use MoE when the model gets the whole card and the work is prefill-heavy or low-concurrency. Use dense when memory is sliced, or when decode concurrency is high and contexts are short | §2; measured 30B-A3B vs 8B |
| What does GQA change? | It sets KV bytes per token, which in turn sets concurrency, how much history stays cached, and decode bandwidth at long context. It barely changes prefill cost | §3 |
| Hybrid linear attention? | It needs 5–7× less KV per token. But prefix-cache hits only land on checkpoints 528 tokens or more apart, fewer KV connectors support it, and it may copy exact strings less reliably. Keep it out of the KV-hop demo. It may be worth trying later on the whole card | §4, §5 |
| Does the model affect prefix caching? | Yes, in four ways. KV per token sets how much history fits. Active parameters set how much compute a hit saves. Layer type (full or recurrent) sets where a hit can start. The chat template decides whether the rendered history stays byte-stable | §5 |
| Quantization on the A100? | Use 4-bit weights with 16-bit activations (W4A16), run by Marlin kernels. It saves weight memory and decode bandwidth, not prefill compute. FP8 weights run weight-only on the A100. The KV cache stays 16-bit unless you set FP8 KV, and FP8 KV moves attention off FlashAttention 2 | §6 |
| What we measured | The 30B-A3B on the whole card scored 67% on v2 (interval 54–78%). The 8B on one slice scored 52% (39–65%). The 30B needed 6 citation repairs against the 8B's 22, and never hit its context budget, against 4 times for the 8B | §8 |

---

## 1. The numbers per candidate

All per-token figures are per sequence, with 16-bit KV and the context length stated [hf, calc].

| Model | Layers (full attention) | Query heads : KV heads | Head dim | Query width | KV per token | KV at 20k context | Recurrent state per sequence | Total / active params | MLP GFLOP per token | Attention GFLOP per token at 20k | Attention share at 20k |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Qwen3-8B-AWQ | 36 (36) | 32 : 8 (4:1) | 128 | 4096 | 144 KiB | 2.75 GiB | none | 8.19B / 8.19B | 16.4 | 11.8 | 42% |
| Qwen3-14B-AWQ | 40 (40) | 40 : 8 (5:1) | 128 | 5120 | 160 KiB | 3.05 GiB | none | 14.8B / 14.8B | 29.5 | 16.4 | 36% |
| Qwen3-30B-A3B-2507, 4-bit | 48 (48) | 32 : 4 (8:1) | 128 | 4096 | 96 KiB | 1.83 GiB | none | 30.5B / 3.3B | 6.6 | 15.7 | 70% |
| Qwen3.5-9B | 32 (8), plus 24 Gated DeltaNet | 16 : 4 (4:1) | 256 | 4096 | 32 KiB | 0.61 GiB | about 49 MiB (fp32) | 9.65B / 9.65B | 19.3 | 2.6 | 12% |
| Qwen3.6-35B-A3B, 4-bit | 40 (10), plus 30 Gated DeltaNet | 16 : 2 (8:1) | 256 | 4096 | 20 KiB | 0.38 GiB | about 61 MiB | 36B / about 3B | 6.0 | 3.3 | 35% |
| Ministral-3-14B, BF16 | 40 (40), no sliding window | 32 : 8 (4:1) | 128 | 4096 | 160 KiB | 3.05 GiB | none | 13.9B / 13.9B | 27.9 | 13.1 | 32% |

How the columns are computed:
- KV per token is 2 (K and V) × full-attention layers × KV heads × head dim × 2 bytes. For the 8B that is 2 × 36 × 8 × 128 × 2 = 147,456 B, or 144 KiB. For the 30B it is 2 × 48 × 4 × 128 × 2 = 98,304 B, or 96 KiB. The formula family is in [KB: class3-engine-surgery:003].
- MLP compute per token is about 2 × active parameters.
- Attention compute per token at context L is about 4 × L × query heads × head dim × full-attention layers. That covers QKᵀ and AV. The QKV and output projections are left out because they count toward the parameters.
- Attention share is attention ÷ (attention + MLP) at 20k.
- For the recurrent state, the fp32 SSM state is V heads × V dim × K dim × 4 B, which is 2 MiB per layer. The conv state is (kernel − 1) × 8,192 channels × 2 B, which is 48 KiB per layer. Qwen3.5-9B has 24 such layers, so about 49 MiB per sequence [web: vLLM v0.29.0 qwen3_5 config; issue #40696] [calc]. That equals the KV of about 1,570 tokens of the model's own attention.
- The hybrid rows leave out the recurrent layers' own compute, which is small and grows linearly per token [general].

The table already shows three things:
- The 30B-A3B needs the least KV per token of the standard-attention models (96 KiB), because it has half the 8B's KV heads. A GiB of memory holds 1.5× as many of its tokens as of the 8B's.
- At 20k context, 70% of the 30B-A3B's per-token compute is attention. MoE only makes the MLP cheaper, so its saving shrinks as context grows. At 20k it still does about 22 GFLOP per token against the 8B's 28, because the MLP saving outweighs the extra attention.
- Four of the six models have a query width of 4,096, and the 14B has 5,120. Ministral and both A3B models project to a query width that differs from their hidden size, which is common [hf].

---

## 2. Dense models vs mixture of experts

### How MoE changes the costs

All experts must stay in GPU memory. The router's choice can't be predicted, so the whole parameter set has to be loaded [KB: class3b-engine-internals:017]. The 30B-A3B needs 16.9 GiB of weights to do the work of about 3.3B parameters. That is why it can't run on a 20 GiB slice: only about 1k tokens of KV pool would be left [calc; `make fit`].

Compute scales with active parameters, in prefill as well as decode, because each token passes through only its top 8 of 128 experts. An uncached 15k-token prefill costs about 187 TFLOP on the 30B-A3B and about 312 TFLOP on the 8B. Both totals include causal attention, which grows with the square of the length [calc]. The course note says the MoE speedup "applies mainly to decode, not prefill". Section 9 explains why that is imprecise.

Memory traffic depends on batch size. Each decode step reads the weights of every expert that any sequence in the batch selected. With 128 experts, 8 active per token, and uniform independent routing, the share of experts read per layer is 1 − (1 − 8/128)^B [calc]:

| Decode batch B | 1 | 4 | 8 | 16 | 32 |
|---|---|---|---|---|---|
| Experts read, 30B-A3B | 6% | 23% | 40% | 64% | 87% |
| Experts read, Qwen3.6 (256 experts, 8 active) | 3% | 12% | 22% | 40% | 64% |

Real routing is skewed, so fewer distinct experts get read than this. Treat the table as an upper bound.

Bytes read per decode step at 12k context, counting the weights read plus every sequence's KV [calc]:

| Batch | 8B (5.7 GiB weights) | 14B | 30B-A3B |
|---|---|---|---|
| 1 | 7.3 GiB | 11.1 | 3.7 |
| 4 | 12.3 | 16.6 | 9.6 |
| 16 | 32.1 | 38.6 | 29.0 |
| 32 | 58.5 | 67.9 | 50.1 |

For the 30B-A3B this assumes about 1.65 GiB of non-expert weights (attention, router and the BF16 LM head) and about 15.2 GiB of 4-bit experts. It reads fewer bytes than the 8B even at 32 sequences, because its smaller KV (8:1 GQA) offsets the extra expert reads. In long-context agent decode, KV reads outweigh weight reads above about four sequences, whatever the MLP looks like.

The kernels decide how much of this you actually get. On the A100 the fused MoE runs on Marlin W4A16 [web: vLLM v0.29.0 marlin.py, min capability 75]. For group-size-32 checkpoints like ours, vLLM may pick FlashInfer's MXINT4 MoE path instead. The startup log line "Using … backend for WNA16 MoE" says which [web: compressed_tensors_moe]. Expert kernels run less efficiently than dense GEMM [KB: class3-engine-surgery:003], so treat the theoretical 1.7× prefill advantage as an upper bound.

Quantization adds a specific risk for MoE. The cyankiwi checkpoint quantizes the experts and attention to symmetric INT4 in groups of 32. It was calibrated with an MSE observer, not the activation-aware search that classic AWQ uses. The 48 router gates (`mlp.gate`) and `lm_head` stay in BF16 [hf config]. A quantization error in a router changes which experts run, so keeping the gates in BF16 is the right protection.

### When to pick which

| Pick MoE when | Pick dense when |
|---|---|
| Memory is plentiful relative to the full weights (a whole card, or several GPUs) | Memory is sliced or tight: slices, MIG, many small replicas |
| The work is prefill-heavy (long prompts, agents, RAG), since compute scales with active parameters | Contexts are short and concurrency is high, since nearly every expert is read each step anyway |
| Decode concurrency is low (interactive use, one user), so each step reads few experts | You need several independent replicas per GPU, for routing, the KV hop or failure isolation |
| You want large-model quality at small-model compute | You want the simplest, most mature kernels and quantization recipes |

For us, the 30B-A3B fits the whole card best on paper. It has cheap prefill and small KV, and vLLM measured room for 7.5 full-length requests. It can't do the slicing, routing and KV-hop demo because it doesn't fit a slice. That is why D-40 ranks quality on the whole card and runs the serving demo on slices.

---

## 3. Grouped-query attention and head counts

The terms:
- Query heads are how many attention patterns each layer computes.
- KV heads are how many distinct key/value projections each layer has. Each KV head is shared by (query heads ÷ KV heads) query heads.
- Head dim is the vector width of each head.
- The variants are MHA (as many KV heads as query heads), MQA (one KV head), GQA (in between) and MLA (a compressed latent KV) [KB: class3-engine-surgery:003].

What each number changes:

| Knob | Changes | Does not change |
|---|---|---|
| Fewer KV heads | Less KV per token, linearly. That means more concurrency, more cached history and fewer KV bytes read per decode step | Prefill FLOPs, which depend on query heads |
| Query heads × head dim | Attention FLOPs per token, in prefill and decode | KV size |
| Larger head dim (256 in Qwen3.5/3.6) | More KV per KV head, and different kernel tile efficiency. FlashAttention 2 supports head dims up to 256 on the A100 [web: flash-attention README] | The number of heads |
| More full-attention layers | More KV per token, linearly | KV per layer |
| Grouping ratio (query ÷ KV heads) | Quality risk if it is too aggressive. MQA can degrade, while GQA stays close to MHA [KB: class3-engine-surgery:003] | KV size, if the KV-head count stays fixed |

Our candidates:
- The 8B, Qwen3.5-9B and Ministral group 4 query heads per KV head. The 14B groups 5, and the 30B-A3B and Qwen3.6 group 8.
- The course notes give "2–8 groups" as the practical range [KB: class3-engine-surgery:003]. All our candidates are GQA, and none is MQA.
- The ratio alone doesn't predict quality, because each model was trained with its grouping. Judge quality on the golden set, not on paper.

For long-context agents this matters more than parameter count. At 12k context one 8B sequence holds 1.65 GiB of KV. A decode step reads the 5.7 GiB of weights once for the whole batch, but reads each sequence's KV separately. From about four sequences up, KV traffic exceeds weight traffic (see the table in §2). The course states the general rule: "Attention type dominates KV cache cost far more than raw parameter count" [KB: class3-engine-surgery:003].

---

## 4. Full attention vs the alternatives

| Type | What it keeps per sequence | Strengths | Costs and risks | Our candidates |
|---|---|---|---|---|
| Full (softmax) attention with GQA | KV for every token in every layer | Recalls any earlier token exactly. Prefix caching works at 16-token block granularity. Every KV connector (hop, offload) supports it | KV grows linearly with context, and prefill attention compute grows with its square | 8B, 14B, 30B-A3B, Ministral (`sliding_window: null`, all 40 layers) [web: Ministral config] |
| Hybrid linear attention (Gated DeltaNet plus gated full attention, 3:1) | KV in 1 layer of every 4, plus a fixed-size recurrent state in each recurrent layer | 5–7× less KV per token than our standard models (32 or 20 KiB). The state doesn't grow, so long context is cheap | Listed below the table | Qwen3.5-9B, Qwen3.6-35B-A3B |
| Sliding window, often interleaved local and global layers | KV for the last W tokens in the local layers | Caps KV growth. Prefix caching works, since only the window is needed | Local layers can't see past W, so quality depends on the global layers | None of ours. Gemma-style models use it [general] |
| MLA (latent KV) | A low-rank latent per token, projected back up when used | 5–13× less KV than MHA, at MHA-level quality [KB: sysdesign-guide:025] | Narrower engine and kernel support. The latent is cached and moved as one unit | None of ours. The DeepSeek-V2/V3 family uses it |

The hybrid models carry five specific costs:
1. Prefix caching works only in "align" mode. Hits land on 528-token blocks, and only where a checkpoint was kept (§5).
2. Moving KV between workers needs a connector that supports the hybrid cache manager (§5).
3. The recurrent layers compress history, which may make exact copying less reliable [general: linear attention is weak at associative recall; hybrids add full-attention layers to compensate].
4. Thinking is on by default, and the chat template rewrites history (§5).
5. The weights include a vision encoder. `--language-model-only` skips loading it.

The third cost matters most for us. Our grounding check requires the model to copy a string such as `lg-orders-api-54b7775f96-6wmnn-orders-api-c2` from a tool result thousands of tokens back. In a 3:1 hybrid, only the 8 (or 10) full-attention layers can do that exact recall. The hybrids were designed with this in mind, and published results are good. Still, this is the one point where the architecture affects whether our answers are correct, not only what they cost. We can measure it by comparing `evidence-exists` repairs per task. So far the 8B needed 22 repairs across 52 tasks and the 30B-A3B needed 6 [measured: golden-*-20260928-*.jsonl].

---

## 5. How the model affects prefix caching

Prefix caching reuses the KV of an identical token prefix. vLLM hashes it in 16-token blocks [KB: sysdesign-guide:025]. It covered 95–97% of our prompt tokens: `cached_share_of_prompt` was 0.949 on the 8B and 0.969 on the 30B-A3B [measured]. The model changes it in seven ways:

| Factor | Effect | Numbers for us |
|---|---|---|
| KV per token (KV heads × head dim × full-attention layers) | Smaller helps. More history fits in the pool before eviction, so hits survive higher concurrency | A GiB holds 10.9k tokens for the 30B-A3B, 7.3k for the 8B and 6.6k for the 14B. Measured pools: 79,056 tokens on the 8B slice, 185,136 on the 30B whole card |
| Active parameters | More active parameters make each hit worth more. A cached token skips 2 × active parameters of compute, plus its attention | A cached token saves 29.5 GFLOP on the 14B, 16.4 on the 8B and 6.6 on the 30B-A3B. Caching pays off most for big dense models, and MoE makes each miss cheaper |
| Layer type | Full attention helps, because any 16-token boundary can be a hit. Recurrent layers hurt, because the state can't be rewound to an arbitrary token | vLLM v0.29 requires `--mamba-cache-mode align` for Qwen3.5, which "does not support 'all'". It enlarges the attention block to 528 tokens for Qwen3.5 (an estimated 1,056 for Qwen3.6), and a hit can only start on that grid. Upstream measured about 0% hits on a 479-token prompt and 95% on a 552-token one. By default v0.29 also keeps only "semantic" checkpoints (`--prefix-cache-retention-interval` 0) [web: v0.29.0 qwen3_5.py, models/config.py, release notes; issue #40696] |
| Checkpoint memory | Hurts hybrids. Each kept checkpoint is a full copy of the state | About 49 MiB per checkpoint for Qwen3.5-9B [calc from config; no official figure]. Keeping many checkpoints eats the KV savings |
| Chat template | A byte-stable rendering of history helps | The Qwen3 and Qwen3.5 templates render past reasoning only for turns after the last user message. When a new user message arrives, earlier turns are re-rendered without their `<think>` blocks and the prefix changes. Our tasks have one user message each, and tool results don't count as user messages, so the loop stays stable. That is why we see 95%. A chat UI with many user turns would lose hits at each new message. The Qwen3-30B-A3B-Instruct-2507 template has no reasoning handling, so its history is always stable [web: chat templates] |
| Tokenizer | No direct effect, but it matters when comparing models | The same prompt tokenizes differently per model: Qwen3 has a 151k vocabulary, Qwen3.5 248k and Mistral's Tekken 131k. The 3,787-token prefix is a Qwen3 measurement, so re-measure it for each model |
| KV connectors (hop, offload) | Full attention works with all of them. Hybrids work only with some | In v0.29.0, a connector without `SupportsHMA` turns off the hybrid KV cache manager, and a hybrid model then fails at startup. Mooncake, NIXL and Offloading implement it, and Mooncake says it is "tested with GDN". The in-tree LMCache connectors don't [web: v0.29.0 config/vllm.py and connector sources] |

For a multi-turn, prefix-heavy agent, full attention with small KV per token, as in the 30B-A3B, is the best combination. Hybrids give up prefix-cache granularity and simple KV transfer to save memory we don't need on a whole A100.

---

## 6. Weight quantization on the A100 (sm80)

| Format | What it quantizes | How vLLM v0.29 runs it on the A100 | Saves | Costs |
|---|---|---|---|---|
| AWQ, GPTQ or compressed-tensors W4A16 (4-bit weights, 16-bit activations) | Linear-layer weights, in groups. The 8B uses groups of 128 with a zero point. The 30B-A3B cyankiwi checkpoint uses groups of 32, symmetric, with an MSE observer | Marlin kernels dequantize inside the GEMM (min capability 7.5). Machete needs Hopper (min 9.0) [web: v0.29.0 marlin.py, machete.py] | About 3.5× less weight memory than BF16: 0.5 B per parameter plus scales, which add about 0.0625 B per parameter at group 32. Also decode bandwidth at small batch | No saving in compute-bound prefill, where the math is still FP16, plus a small dequantization overhead. Some accuracy risk, higher for community calibrations |
| FP8 weights (W8A8 checkpoints) | Weights, and activations on Hopper | Weight-only (W8A16) through Marlin. FP8 compute needs capability 8.9 or higher [web: v0.29.0 quantization docs] | 2× less weight memory than BF16 | No compute speedup on the A100. Ministral's default repo is FP8, which is why we use its BF16 repo |
| FP8 KV cache (`--kv-cache-dtype fp8`) | KV storage | FlashAttention 2 doesn't accept FP8 KV, so vLLM switches to FlashInfer, or Triton if FlashInfer is absent. Without calibration the scales are 1.0 [web: v0.29.0 attention backends, quantized_kvcache] | About 2× more KV capacity: roughly 158k tokens instead of 79k on the 8B slice [calc] | The different attention backend changes latency. Uncalibrated scales risk quality. It halves attention KV only, not hybrid state |
| Activation quantization (INT8 W8A8) | Weights and activations | Supported, but not something we use | Compute runs on Ampere's INT8 tensor cores | Risk of tool-call format errors [KB: sysdesign-guide:024] |

Two points follow for us:
- Quantizing weights doesn't quantize the KV cache. Every KV figure in this guide assumes 16-bit KV.
- W4A16 speeds up decode, not prefill. Our workload is about 120:1 prompt-heavy, with 95% of prompt tokens cached, so weight quantization mostly saves memory. That saving is what lets the 8B fit a slice with 79k tokens of KV. It gains us very little speed.

---

## 7. Which architecture suits which workload

| Workload profile | What limits it | Architecture that suits it | Why |
|---|---|---|---|
| Short prompts, short answers, few users (interactive chat) | Decode bandwidth at batch 1–4 | MoE (few experts read per step), or a small quantized dense model | Bytes read per token dominate. See the batch table in §2 |
| Short prompts, many concurrent users | Decode compute and weight reads per step | A small dense model with W4A16 | At batch 16 or more an MoE reads most of its experts anyway, while a dense model's weights are read once for the whole batch |
| Long prompts, few users (document QA) | Prefill compute and TTFT | MoE, since prefill compute scales with active parameters; GQA | Prefill FLOPs scale with active parameters plus attention, and KV isn't the limit |
| Long prompts, many users | KV capacity, then prefill | Small KV per token (aggressive GQA, hybrid or MLA), FP8 KV | Concurrency equals the pool divided by (KV per token × length) |
| RAG (different retrieved context per query) | Prefill of uncached documents, and KV | Small KV per token, MoE for prefill; hybrids are attractive here | Only the system prompt caches, and reordering documents breaks reuse [KB: sysdesign-guide:036]. Quoting documents exactly favours full attention |
| Agents and tool calling (our case) | Prefill of each growing turn, mostly cached; KV for long histories; exact copying; tool-parser support | Full attention with GQA, small KV per token and prefix caching. MoE if the model gets the whole card. A non-thinking model, or a template that keeps history stable | With 95% of the prompt cached, the uncached tail is cheap. Small KV keeps more runs' histories in memory. Exact refs need full attention. D-38 showed that the tool parser and decoding mode matter as much as the model |
| Long generation (reasoning, code) | Number of decode steps × context | MoE at low batch, small KV per token, speculative decoding | Every output token is a decode step |

---

## 8. What we measured, and what it means for our candidates

Measured 2026-09-28, 26 tasks run twice each, one request at a time:

| Result | Qwen3-8B-AWQ, one slice | Qwen3-30B-A3B-2507 4-bit, whole card | Interpretation |
|---|---|---|---|
| v2 pass, with 95% interval | 52% (39–65) | 67% (54–78) | The intervals overlap, so the gap isn't established yet. Run at least 3 repeats |
| v1 pass | 44% | 65% | The v1 to v2 gap is larger for the 8B, so more of its answers were right but labelled differently or found by a different tool path |
| v2 pass by tier: easy, multi-hop, red herring, right-sizing | 68%, 43%, 25%, 0% | 75%, 50%, 75%, 50% | The biggest gain is on red herrings, where the model has to reject the obvious suspect |
| Root found; mechanism right | 62%; 58% | 77%; 77% | The 30B names the right root more often, and explains the mechanism correctly each time it does |
| Submitted a valid diagnosis | 83% | 96% | The 8B's other runs ended at the context budget, the step cap or a failed citation check |
| Citation repairs, all tasks | 22 | 6 | Fewer failed citations, which fits the exact-copy argument in §4 |
| Runs stopped at the context budget | 4 | 0 | Fewer, more targeted tool calls: 9.7 steps against 11.0, and a longest prompt of 14.8k tokens against 22.6k |
| Share of prompt tokens cached | 94.9% | 96.9% | Full attention and a byte-stable template |
| Step latency, p50 and p95 | 0.44 s and 6.5 s | 0.44 s and 2.5 s | Not comparable: the 8B had 50% of the GPU's compute and the 30B had 100% |
| KV pool, measured against paper | 79,056 tokens (0.8% below) | 185,136 tokens (6.3% below) | The calculator underestimates MoE overhead by about 1.1 GiB (fused-MoE workspace, CUDA graphs). D-40 says to recalibrate past 5% |
| Correct diagnoses per GPU-hour | 267 | 326 | Scaled by the GPU share each used, at one request at a time only |

The 30B-A3B leads on every sub-score, and the direction matches what its architecture predicts: cheap prefill, small KV, exact recall and a template that keeps history stable. But these are two repeats at one request at a time, on different topologies. Two things are needed before calling a winner:
1. Run both with REPEAT=3 to tighten the intervals.
2. Add the 14B on a slice, to separate the effect of more parameters from the effect of MoE.

The 14B run is what settles the argument. If the dense 14B matches the 30B-A3B, model size explains the gain. If it doesn't, the case for MoE and fewer KV heads holds.

---

## 9. Where the course notes are wrong or imprecise

| Note | Claim | Correction |
|---|---|---|
| [KB: class1-physics:005] | "Llama 3 8B @ FP16 caches ~0.5 MB per token … one 8k-token conversation ≈ 4 GB" | 32 layers × 2 × 8 KV heads × 128 × 2 B = 131,072 B, or 128 KiB per token. The note's own arithmetic says 131 KB. 8k tokens come to about 1 GiB, not 4 GB |
| [KB: class3-engine-surgery:003] | The MoE speedup "applies mainly to decode, not prefill" | Each token uses only its top-k experts in prefill too, so prefill FLOPs scale with active parameters. What prefill doesn't save is memory, since all weights stay resident. It is decode's bandwidth saving that shrinks as the batch grows (§2) |
| [KB: sysdesign-guide:024] | "AWQ INT4 ~2–3× latency", "FP8 H100+ required" | W4A16 speeds up memory-bound decode, not compute-bound prefill, and the gain depends on batch size. FP8 weights do run on the A100, as W8A16 through Marlin, saving memory without FP8 math. The note gives no source for its percentages |
| [KB: class3b-engine-internals:017] | MoE KV "is unchanged from an equivalent dense model" | True in principle, since KV depends on attention, not experts. But MoE models are often built with fewer KV heads. The 30B-A3B has 4 against the 8B's 8, so in practice its KV per token is smaller |

---

## 10. Checks on our stack

Each check is one command or one column:
1. Cached share per model: `cached_share_of_prompt` in each golden summary, already shown in the matrix.
2. Citation repairs per task: `repairs` in the summary. This is the exact-copy measure from §4.
3. MoE kernel: the startup log line "Using … backend for WNA16 MoE" names Marlin or FlashInfer MXINT4.
4. Hybrid caching, if we serve a hybrid: the startup log should show an attention block size of 528 and mamba cache mode `align`. Then compare `vllm:prefix_cache_hits_total / queries_total` with the 30B's.
5. FP8 KV experiment: add `--kv-cache-dtype fp8`, confirm the backend switch in the log, then compare the pool (`make kv`), TTFT and v2 pass against the same model without it.
6. Fair speed comparison: run the 8B once with `TOPO=full`. Pass rates compare across topologies, but latencies don't.

Sources, checked 2026-09-28:
- vLLM v0.29.0 docs: engine args, attention backends, quantized KV cache, quantization, FP8 (llm-compressor).
- vLLM v0.29.0 source: `qwen3_5.py`, `models/config.py`, `config/vllm.py`, the KV connector sources, `marlin.py`, `machete.py`.
- vLLM issue #40696 and the v0.29.0 release notes.
- The LMCache hybrid-models guide.
- The FlashAttention README.
- Hugging Face model cards, configs and chat templates for the six candidates.

Key URLs:
- <https://docs.vllm.ai/en/v0.29.0/design/attention_backends/>
- <https://docs.vllm.ai/en/v0.29.0/features/quantization/quantized_kvcache/>
- <https://docs.vllm.ai/en/latest/features/automatic_prefix_caching/>
- <https://github.com/vllm-project/vllm/issues/40696>
- <https://docs.lmcache.ai/mp/hybrid_models.html>
