"""Will this model fit this topology, and how much KV is left? Paper numbers from the profile, measured ones from `make kv`.

    python -m serving.fit qwen3-30b-a3b-2507-awq full        # one pair, with the derivation
    python -m serving.fit all                                 # every profile × topology, one table
    python -m serving.fit qwen3-8b-awq sliced --gate          # exit 1 if the pair fails the gate (make deploy runs this)

Per worker (topology → HAMi memory and SM share):
    budget      = slice MiB × gpu_memory_utilization
    KV pool     = budget − weights − activation peak − CUDA context
    KV / token  = 2 (K,V) × attention layers × KV heads × head dim × bytes            (standard attention)
    state / seq = recurrent layers × (V heads·V dim·K dim × 4 B (fp32) + (conv kernel − 1)·(2·K heads·K dim + V heads·V dim) × bytes)
                  (hybrid models only: a fixed-size state per sequence instead of KV for those layers; an estimate)
    sequences   = KV pool ÷ (KV/token × length + state/seq)
    gate        = at least `gate.min_full_length_sequences` requests of max_model_len fit (vLLM will not start otherwise);
                  under `comfortable_full_length_sequences` the pair is marked tight
Decode and prefill floors use the ACTIVE parameters (mixture of experts) and the topology's SM share. Every number
except the architecture is an estimate until `make kv` records vLLM's own "GPU KV cache size" for the pair.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

from serving.profiles import ROOT, load_profile, load_serving, profiles, topology

GIB, MIB = 1024 ** 3, 1024 ** 2
KV_LOG = re.compile(r"GPU KV cache size:\s*([\d,]+)\s*tokens")


def kv_per_token(arch: dict, kv_bytes: int) -> int:
    return 2 * arch["attn_layers"] * arch["kv_heads"] * arch["head_dim"] * kv_bytes


def state_per_seq(arch: dict, kv_bytes: int) -> int:
    """Gated DeltaNet state per sequence: an fp32 SSM state (V heads × V dim × K dim) plus a conv state of
    (kernel − 1) × channels in the KV dtype, per recurrent layer. Qwen3.5-9B: 2.05 MiB/layer, ~49 MiB over 24 layers."""
    lin = arch.get("linear")
    if not lin:
        return 0
    v = lin["value_heads"] * lin["value_head_dim"]
    ssm = v * lin["key_head_dim"] * 4
    conv = (lin["conv_kernel"] - 1) * (2 * lin["key_heads"] * lin["key_head_dim"] + v) * kv_bytes
    return lin["layers"] * (ssm + conv)


def measured_kv(name: str, topo: str, metrics: Path | None = None) -> int | None:
    """vLLM's own pool size for this pair, from the newest metrics/kv-<model>-<topo>-*.log that has one."""
    for f in sorted((metrics or ROOT / "metrics").glob(f"kv-{name}-{topo}-*.log"), reverse=True):
        m = KV_LOG.search(f.read_text())
        if m:
            return int(m.group(1).replace(",", ""))
    return None


def fit(p: dict, topo: str, s: dict | None = None, *, metrics: Path | None = None) -> dict:
    s = s or load_serving()
    t, e, est, a = topology(s, topo), s["engine"], s["estimates"], p["arch"]
    kvb = e["kv_cache_bytes"]
    share = t["gpucores"] / 100
    budget = t["gpumem_mib"] * MIB * e["gpu_memory_utilization"]
    weights = a["weights_gib"] * GIB
    act = e["max_num_batched_tokens"] * 2 * (4 * a["hidden"] + 2 * a["intermediate"])
    ctx = est["cuda_context_gib"] * GIB
    pool = budget - weights - act - ctx
    kvt, state = kv_per_token(a, kvb), state_per_seq(a, kvb)
    measured = measured_kv(p["name"], topo, metrics)
    if measured:
        pool_used = measured * kvt
    else:
        pool_used = pool
    n_max, app, prefix = e["max_model_len"], est["app_len"], est["shared_prefix"]

    def seqs(n: int) -> float:
        return max(0.0, pool_used / (kvt * n + state)) if pool_used > 0 else 0.0

    shared = ((pool_used - prefix * kvt) / (kvt * (app - prefix) + state)) if pool_used > prefix * kvt else 0.0
    active = a["active_params_b"] / a["params_b"]
    bw, flops = s["gpu"]["bandwidth_gbs"] * 1e9 * share, s["gpu"]["tflops_bf16"] * 1e12 * est["mfu"] * share
    gate_n, comfy = s["gate"]["min_full_length_sequences"], s["gate"]["comfortable_full_length_sequences"]
    return {
        "model": p["name"], "topology": topo, "plan": p["plan"], "repo": p["hf"]["repo"], "hybrid": bool(a.get("linear")),
        "moe": a["active_params_b"] < a["params_b"], "slice_gib": t["gpumem_mib"] / 1024, "sm_share": share,
        "budget_gib": budget / GIB, "weights_gib": a["weights_gib"], "activation_gib": act / GIB, "cuda_context_gib": ctx / GIB,
        "pool_gib": pool / GIB, "kv_per_token_kib": kvt / 1024, "state_per_seq_mib": state / MIB,
        "tokens_paper": max(0, int(pool / kvt)), "tokens_measured": measured,
        "measured_vs_paper": (measured / (pool / kvt) - 1) if measured and pool > 0 else None,
        "seqs_at_max_len": seqs(n_max), "seqs_at_app_len": seqs(app), "seqs_at_app_len_shared_prefix": max(0.0, shared),
        "max_model_len": n_max, "app_len": app, "shared_prefix": prefix,
        "decode_floor_tok_s": bw / (weights * active + kvt * app),
        "prefill_s_app_len_uncached": 2 * a["active_params_b"] * 1e9 * app / flops,
        "gate_sequences": gate_n, "comfortable_sequences": comfy, "fits": seqs(n_max) >= gate_n, "tight": seqs(n_max) < comfy,
        "max_replicas": t["max_replicas"],
    }


def verdict(r: dict) -> str:
    if r["pool_gib"] <= 0:
        return "does not fit: weights + overhead exceed the budget"
    if not r["fits"]:
        return f"below the gate: {r['seqs_at_max_len']:.1f} < {r['gate_sequences']} requests of {r['max_model_len']:,} tokens"
    if r["tight"]:
        return f"fits, tight: {r['seqs_at_max_len']:.1f} requests of {r['max_model_len']:,} tokens (< {r['comfortable_sequences']})"
    return "fits"


def explain(r: dict) -> str:
    lines = [f"{r['model']} on {r['topology']}  ({r['repo']})",
             f"  budget      {r['slice_gib']:.0f} GiB slice × util = {r['budget_gib']:.1f} GiB   (SM share {r['sm_share']:.0%})",
             f"  − weights   {r['weights_gib']:.2f} GiB   − activation peak {r['activation_gib']:.2f} GiB   − CUDA context {r['cuda_context_gib']:.2f} GiB",
             f"  = KV pool   {r['pool_gib']:.2f} GiB  → {r['tokens_paper']:,} tokens at {r['kv_per_token_kib']:.0f} KiB/token"
             + (f"   + {r['state_per_seq_mib']:.1f} MiB recurrent state per sequence (estimate)" if r["hybrid"] else ""),
             (f"  measured    {r['tokens_measured']:,} tokens ({r['measured_vs_paper']:+.1%} vs paper); sequences below use it"
              if r["tokens_measured"] else "  measured    none yet; run `make kv` on this pair"),
             f"  sequences   {r['seqs_at_max_len']:.1f} at {r['max_model_len']:,} · {r['seqs_at_app_len']:.1f} at {r['app_len']:,} · "
             f"{r['seqs_at_app_len_shared_prefix']:.1f} at {r['app_len']:,} with the {r['shared_prefix']:,}-token prefix cached once",
             f"  floors      decode ≤ {r['decode_floor_tok_s']:.0f} tok/s per stream · uncached prefill of {r['app_len']:,} tokens ≈ "
             f"{r['prefill_s_app_len_uncached']:.2f} s" + ("   (mixture of experts: active parameters)" if r["moe"] else ""),
             f"  verdict     {verdict(r)}"]
    return "\n".join(lines)


def table(rows: list[dict]) -> str:
    head = ("| Model | Plan | Topology | Weights GiB | KV/token KiB | State/seq MiB | KV pool GiB | Tokens (paper) | Tokens (measured) "
            "| Seqs @24k | Seqs @12k, prefix shared | Prefill 12k s | Verdict |")
    out = [head, "|" + "---|" * (head.count("|") - 1)]
    for r in rows:
        meas = f"{r['tokens_measured']:,} ({r['measured_vs_paper']:+.1%})" if r["tokens_measured"] else "n/a"
        out.append(f"| {r['model']} | {r['plan']} | {r['topology']} | {r['weights_gib']:.1f} | {r['kv_per_token_kib']:.0f} | "
                   f"{r['state_per_seq_mib']:.1f} | {r['pool_gib']:.1f} | {r['tokens_paper']:,} | {meas} | {r['seqs_at_max_len']:.1f} | "
                   f"{r['seqs_at_app_len_shared_prefix']:.1f} | {r['prefill_s_app_len_uncached']:.2f} | {verdict(r)} |")
    return "\n".join(out)


def all_rows(metrics: Path | None = None) -> list[dict]:
    s = load_serving()
    return [fit(p, t, s, metrics=metrics) for p in profiles() for t in s["topologies"]]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("model", help="a profile name, or all")
    ap.add_argument("topology", nargs="?", default="all", help="sliced, full, or all")
    ap.add_argument("--gate", action="store_true", help="exit 1 unless the pair passes the fit gate")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args(argv)
    s = load_serving()
    ps = profiles() if a.model == "all" else [load_profile(a.model)]
    topos = list(s["topologies"]) if a.topology == "all" else [a.topology]
    rows = [fit(p, t, s) for p in ps for t in topos]
    if a.json:
        print(json.dumps(rows, indent=1))
    elif len(rows) == 1:
        print(explain(rows[0]))
    else:
        print(table(rows))
    if a.gate and not all(r["fits"] for r in rows):
        print("fit gate failed: " + "; ".join(f"{r['model']} on {r['topology']}: {verdict(r)}" for r in rows if not r["fits"]), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
