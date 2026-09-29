"""Model profiles, topologies, the fit calculator and the matrix (D-40): what is served, where it fits, how it ranks."""
import json
import re

import httpx

from doctor import agent
from serving import fit, matrix, profiles
from tests.mockllm import Server

ROOT = profiles.ROOT


def test_every_profile_is_valid_and_the_plan_covers_both_topologies():
    ps = profiles.profiles()
    assert len({p["name"] for p in ps}) == len(ps) >= 6 and ps[0]["name"] == "qwen3-8b-awq"
    for p in ps:
        assert profiles.validate_profile(p, p["name"]) == [], p["name"]
    rows = {(r["model"], r["topology"]): r for r in fit.all_rows()}
    assert all(rows[(p["name"], "full")]["fits"] for p in ps)                      # everything benchmarks on the whole card
    assert rows[("qwen3-8b-awq", "sliced")]["fits"] and not rows[("qwen3-8b-awq", "sliced")]["tight"]


def test_profile_validation_catches_mistakes():
    p = profiles.load_profile("qwen3-8b-awq")
    bad = json.loads(json.dumps(p))
    bad["serve"]["args"].append("--max-model-len=40960")           # engine settings belong to serving.json
    bad["arch"]["attn_layers"] = 30                                # hybrid without its recurrent layers
    bad["client"]["temprature"] = 0.2
    bad["hf"]["revision"] = "main"                                 # unpinned
    errs = profiles.validate_profile(bad, "qwen3-8b-awq")
    assert any("--max-model-len" in e for e in errs) and any("arch.linear" in e for e in errs)
    assert any("temprature" in e for e in errs) and any("revision" in e for e in errs)


def test_committed_manifest_is_the_rendered_default_and_cloud_init_agrees():
    name, topo = profiles.DEFAULT
    p, s = profiles.load_profile(name), profiles.load_serving()
    assert profiles.MANIFEST.read_text() == profiles.render_manifest(p, topo, s)
    boot = (ROOT / "deploy" / "cloud-init.yaml").read_text()
    pins = dict(re.findall(r'^\s+(VLLM_IMAGE|MODEL|MODEL_REVISION)="([^"]+)"', boot, re.M))
    assert pins == {"VLLM_IMAGE": s["engine"]["image"], "MODEL": p["hf"]["repo"], "MODEL_REVISION": p["hf"]["revision"]}


def test_full_topology_gets_the_whole_card_and_model_specific_args():
    m = profiles.render_manifest(profiles.load_profile("qwen3.5-9b"), "full")
    assert 'nvidia.com/gpumem: "40960"' in m and 'nvidia.com/gpucores: "100"' in m
    assert "--tool-call-parser=qwen3_coder" in m and "--language-model-only" in m and "--max-model-len=24576" in m
    assert "doctor.serving/model: qwen3.5-9b" in m and "hermes" not in m
    sliced = profiles.render_manifest(profiles.load_profile("qwen3-8b-awq"), "sliced")
    assert 'nvidia.com/gpumem: "20480"' in sliced and 'nvidia.com/gpucores: "50"' in sliced
    pre = profiles.render_prefetch(profiles.load_profile("ministral-3-14b"))
    assert '"consolidated.safetensors"' in pre and "model-3cea74c1ebaf5ce5f5a2553de470e2ceab825142" in pre
    assert "allow_patterns=None" in profiles.render_prefetch(profiles.load_profile("qwen3-8b-awq"))


def test_fit_matches_the_measured_8b_slice_and_gates_what_cannot_start():
    r = fit.fit(profiles.load_profile("qwen3-8b-awq"), "sliced")
    assert r["tokens_measured"] == 79_056 and abs(r["measured_vs_paper"]) < 0.015       # paper within 1.5 % of vLLM
    assert r["kv_per_token_kib"] == 144 and 3.0 < r["seqs_at_max_len"] < 3.4           # vLLM said 3.22
    moe_sliced = fit.fit(profiles.load_profile("qwen3-30b-a3b-2507-awq"), "sliced")
    moe_full = fit.fit(profiles.load_profile("qwen3-30b-a3b-2507-awq"), "full")
    assert not moe_sliced["fits"] and moe_full["fits"] and moe_full["seqs_at_max_len"] > 7
    assert moe_full["prefill_s_app_len_uncached"] < fit.fit(profiles.load_profile("qwen3-8b-awq"), "full")["prefill_s_app_len_uncached"]
    assert fit.fit(profiles.load_profile("qwen3-14b-awq"), "sliced")["tight"]
    hybrid = fit.fit(profiles.load_profile("qwen3.5-9b"), "full")
    assert hybrid["hybrid"] and hybrid["kv_per_token_kib"] == 32 and hybrid["state_per_seq_mib"] > 10
    assert fit.main(["qwen3-30b-a3b-2507-awq", "sliced", "--gate"]) == 1 and fit.main(["qwen3-30b-a3b-2507-awq", "full", "--gate"]) == 0


def test_profile_sampling_reaches_the_wire():
    def first_body(ref):
        s = Server(("submit_diagnosis", {}))
        served, client = agent.load_profile(ref)
        llm = agent.build_llm("http://gw.test/v1", served, client=client, http_client=httpx.Client(transport=httpx.MockTransport(s)))
        llm.invoke("hi")
        return s.requests[0]["body"]
    mistral = first_body("ministral-3-14b")
    assert mistral["model"] == "mistralai/Ministral-3-14B-Instruct-2512" and mistral["temperature"] == 0.15
    assert "top_k" not in mistral and "chat_template_kwargs" not in mistral
    coder = first_body("qwen3-coder-30b-a3b-awq")
    assert coder["repetition_penalty"] == 1.05 and "chat_template_kwargs" not in coder
    assert first_body(str(ROOT / "deploy/models/qwen3-8b-awq.json"))["chat_template_kwargs"] == {"enable_thinking": False}
    assert agent.resolve_model(None, "qwen3-14b-awq")[0] == "Qwen/Qwen3-14B-AWQ"
    assert agent.resolve_model("gateway-alias", "qwen3-14b-awq")[0] == "gateway-alias"


def test_matrix_ranks_full_runs_and_lists_what_was_not_run(tmp_path):
    for f in (ROOT / "metrics").glob("kv-*.log"):
        (tmp_path / f.name).write_text(f.read_text())
    def summary(name, profile, topo, workers, share, passed, n=52, wall=1800, ci=(0.36, 0.64)):
        (tmp_path / f"golden-{name}.summary.json").write_text(json.dumps({
            "tag": name, "profile": profile, "topology": topo, "workers": workers, "gpu_share": share, "tasks": n,
            "unique_tasks": 26, "repeat": 2, "only": None, "passed": passed - 4, "passed_v2": passed, "pass_rate": (passed - 4) / n,
            "pass_rate_v2": passed / n, "pass_rate_v2_ci95": list(ci), "pass_rate_by_tier_v2": {"easy": 0.8}, "parts_v2": {"root": 0.7},
            "stop_reasons": {"submitted": n}, "wall_s": wall, "concurrency": 4, "timestamp": f"20261001-00000{workers}"}))
    summary("a", "qwen3-8b-awq", "sliced", 2, 1.0, 26)
    summary("b", "qwen3-30b-a3b-2507-awq", "full", 1, 1.0, 40, ci=(0.66, 0.86))
    (tmp_path / "golden-smoke.summary.json").write_text(json.dumps({"tag": "smoke", "model": "Qwen/Qwen3-8B-AWQ", "tasks": 2,
                                                                     "repeat": 2, "passed": 0, "timestamp": "20260927-000000"}))
    text = matrix.render(tmp_path)
    ranking = text.split("## 3.")[1].split("## 4.")[0]
    assert ranking.index("qwen3-30b-a3b-2507-awq") < ranking.index("qwen3-8b-awq") and "Clearly ahead of the next model" in ranking
    assert "80 correct diagnoses per GPU-hour" in ranking and "52 correct diagnoses per GPU-hour" in ranking     # 40 / 0.5 h / 1.0
    assert "Not run: **qwen3.6-35b-a3b-awq**" in ranking and "79,056 (-0.8%)" in text
    assert "partial: subset; profile/topology assumed (pre D-40)" in text
