"""Model profiles, topologies, the fit calculator and the matrix (D-40): what is served, where it fits, how it ranks."""
import json
import re

import httpx

from doctor import agent
from evals.build_golden import backend_for
from serving import fit, matrix, profiles, warmup
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
    assert any("--max-model-len" in e for e in errs) and any("linear.layers + sliding.layers" in e for e in errs)
    assert any("temprature" in e for e in errs) and any("revision" in e for e in errs)


def test_committed_manifest_is_the_rendered_default_and_cloud_init_agrees():
    name, topo = profiles.DEFAULT
    p, s = profiles.load_profile(name), profiles.load_serving()
    assert profiles.MANIFEST.read_text() == profiles.render_manifest(p, topo, s)
    boot = (ROOT / "deploy" / "cloud-init.yaml").read_text()
    assert re.search(r'^\s+VLLM_IMAGE="([^"]+)"', boot, re.M).group(1) == s["engine"]["image"]
    pins = {}
    for classes, body in re.findall(r"^\s+([a-z0-9|-]+)\) (MODEL=.*) ;;$", boot, re.M):
        for c in classes.split("|"):
            pins[c] = dict(re.findall(r'(\w+)="([^"]+)"', body))
    assert set(pins) == set(s["gpus"])                                   # every GPU class boots with a model
    for c, g in s["gpus"].items():
        boot_p, nxt = profiles.load_profile(g["boot"]["model"])["hf"], profiles.load_profile(g["boot"]["next_model"])["hf"]
        assert pins[c] == {"MODEL": boot_p["repo"], "MODEL_REVISION": boot_p["revision"],
                           "NEXT_MODEL": nxt["repo"], "NEXT_MODEL_REVISION": nxt["revision"]}, c
        assert s["topologies"][g["boot"]["topology"]]["gpu"] == c


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


def test_hop_adds_the_mooncake_connector_and_its_port_and_nothing_else():
    p, s = profiles.load_profile("qwen3-8b-awq"), profiles.load_serving()
    plain, hop = profiles.render_manifest(p, "sliced", s), profiles.render_manifest(p, "sliced", s, hop=True)
    conf = re.findall(r"^\s+- --kv-transfer-config=(.+)$", hop, re.M)
    assert [json.loads(c) for c in conf] == [s["kv_hop"]["kv_transfer_config"]]
    assert s["kv_hop"]["kv_transfer_config"]["kv_role"] == "kv_both"
    port = f"- {{name: mooncake, containerPort: {s['kv_hop']['bootstrap_port']}}}"
    assert port in hop
    policy = profiles.HOP_POLICY.format(port=s["kv_hop"]["bootstrap_port"])
    assert hop.endswith(policy)
    worker = hop[: -len(policy)]
    added = [line for line in worker.splitlines() if line not in plain.splitlines()]
    assert [line.strip() for line in added] == [f"- --kv-transfer-config={conf[0]}", port]
    assert "kind: NetworkPolicy" in policy and "matchLabels: {app: vllm}" in policy
    assert f"port: {s['kv_hop']['bootstrap_port']}" in policy and "port: 8000" in policy
    assert "MooncakeConnector" not in plain and "NetworkPolicy" not in plain


def test_gateway_env_sizes_admission_to_the_measured_kv_pool(capsys):
    # The 8B on an A100 slice (79,056 tokens measured) holds 9 typical runs; Qwen3.8 on an H100 half (51,092 measured)
    # holds 4, where the old constant of 16 let vLLM preempt at 32 concurrent runs (findings F24-F26).
    assert fit.main(["qwen3-8b-awq", "sliced", "--gateway-env"]) == 0
    assert capsys.readouterr().out.strip() == "GW_MAX_INFLIGHT=9 GW_HOP_KV_BYTES_PER_TOKEN=147456 GW_HOP_PREFILL_TOKENS_PER_S=3810"
    assert fit.main(["qwen3.8-27b-fp8", "h100-half", "--gateway-env"]) == 0
    assert capsys.readouterr().out.strip().startswith("GW_MAX_INFLIGHT=4 ")
    assert fit.main(["qwen3.8-27b-fp8", "h100-full", "--gateway-env"]) == 0           # a whole card stays at the ceiling
    assert capsys.readouterr().out.strip().startswith(f"GW_MAX_INFLIGHT={fit.GATEWAY_MAX_INFLIGHT} ")


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


def test_warmup_body_is_the_doctors_first_request_with_one_output_token():
    task = json.loads(warmup.TASKS.read_text().splitlines()[0])
    s = Server(("submit_diagnosis", {}))
    served, client = agent.load_profile("qwen3-8b-awq")
    llm = agent.build_llm("http://gw.test/v1", served, client=client, http_client=httpx.Client(transport=httpx.MockTransport(s)))
    agent.run_task(task, backend_for(task["snapshots"]), llm)
    live = s.requests[0]["body"]
    warm = json.loads(warmup.warm_body("qwen3-8b-awq"))
    assert warm["max_completion_tokens"] == 1 and live["max_completion_tokens"] == 768
    assert {**warm, "max_completion_tokens": 768} == live


def test_result_files_keep_a_dotted_tag_whole(tmp_path):
    from evals.run_golden import result_paths
    rows, summary = result_paths(tmp_path, "sweep-qwen3.8-27b-fp8-c16", "20261007-151105")
    assert rows.name == "golden-sweep-qwen3.8-27b-fp8-c16-20261007-151105.jsonl"
    assert summary.name == "golden-sweep-qwen3.8-27b-fp8-c16-20261007-151105.summary.json"


def test_cloud_init_embeds_the_alert_rules_verbatim():
    import textwrap
    rules = (ROOT / "deploy" / "observability" / "alerts.yaml").read_text()
    groups = rules[rules.index("groups:"):]
    boot = (ROOT / "deploy" / "cloud-init.yaml").read_text()
    assert "        alerting_rules.yml:\n" + textwrap.indent(groups, " " * 10).rstrip() + "\n" in boot


def test_autoscaling_reads_the_tested_signals_and_scales_only_on_capacity():
    """D-49: the ScaledObject queries recording rules that alerts.yaml defines (and promtool tests), the shed signal
    names every capacity reason the gateway emits and not the tenant quota, and cloud-init installs a pinned KEDA."""
    so = (ROOT / "deploy" / "autoscale" / "keda-vllm.yaml").read_text()
    rules = (ROOT / "deploy" / "observability" / "alerts.yaml").read_text()
    recorded = set(re.findall(r"record: (\S+)", rules))
    queried = set(re.findall(r"doctor:[a-z_]+", so))
    assert queried and queried <= recorded, queried - recorded
    assert "__MAX_REPLICAS__" in so and "__WORKER_CAP__" in so
    assert "kind: StatefulSet, name: vllm" in so and "minReplicaCount: 1" in so

    gateway_reasons = set(re.findall(r'Reason\w* *= *"([a-z0-9_]+)"', "".join(
        f.read_text() for f in (ROOT / "gateway" / "internal" / "decide").glob("*.go") if not f.name.endswith("_test.go"))))
    shed_rule = re.search(r'gateway_capacity_sheds_per_minute\n\s+expr: .*reason=~"([^"]+)"', rules).group(1)
    assert set(shed_rule.split("|")) == gateway_reasons - {"tenant_tokens"}, (shed_rule, gateway_reasons)

    boot = (ROOT / "deploy" / "cloud-init.yaml").read_text()
    assert re.search(r'KEDA_CHART_VERSION="\d+\.\d+\.\d+"', boot) and "kedacore/keda --version \"$KEDA_CHART_VERSION\"" in boot


def test_cluster_dashboard_reads_only_exported_metrics_and_recorded_signals():
    """The cluster dashboard (D-49) reads gateway metrics the gateway exports and scaling signals alerts.yaml records."""
    dash = (ROOT / "deploy" / "observability" / "dashboards" / "cluster.json").read_text()
    exported = set(re.findall(r'^\s+"(orch_[a-z_]+)",$', (ROOT / "gateway" / "internal" / "metrics" / "metrics.go").read_text(), re.M))
    used = {re.sub(r"_(bucket|sum|count)$", "", m) for m in re.findall(r"orch_[a-z_]+", dash)}
    assert used and used <= exported, used - exported
    recorded = set(re.findall(r"record: (\S+)", (ROOT / "deploy" / "observability" / "alerts.yaml").read_text()))
    assert set(re.findall(r"doctor:[a-z_]+", dash)) == recorded
    assert json.loads(dash)["uid"] == "cd-cluster"
