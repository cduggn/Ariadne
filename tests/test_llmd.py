"""The llm-d router render (design/llm-d.md): chart values, EPP config and the worker manifest, from the same sources."""
import json
import re

import pytest

from serving import fit, llmd, profiles

ROOT = profiles.ROOT
LLMD = ROOT / "deploy" / "llm-d"


def parsed(text: str) -> dict:
    return json.loads("".join(line for line in text.splitlines(keepends=True) if not line.startswith("#")))


def epp(name: str, topo: str) -> dict:
    return parsed(llmd.render_values(profiles.load_profile(name), topo))["router"]["epp"]["pluginsConfig"]


def plugin(config: dict, kind: str) -> dict:
    return next(x for x in config["plugins"] if x["type"] == kind).get("parameters", {})


def test_committed_llm_d_files_are_the_rendered_default():
    name, topo = profiles.DEFAULT
    p = profiles.load_profile(name)
    assert (LLMD / "values.yaml").read_text() == llmd.render_values(p, topo)
    assert (LLMD / "vllm.yaml").read_text() == profiles.render_manifest(p, topo, llmd=True)
    assert profiles.MANIFEST.read_text() == profiles.render_manifest(p, topo)       # llm-d mode stays opt-in


def test_hop_and_llmd_are_exclusive():
    with pytest.raises(ValueError, match="exclusive"):
        profiles.render_manifest(profiles.load_profile("qwen3-8b-awq"), "sliced", hop=True, llmd=True)


def test_full_attention_routes_precisely_and_publishes_64_token_kv_events():
    m = profiles.render_manifest(profiles.load_profile("qwen3-8b-awq"), "sliced", llmd=True)
    events = json.loads(re.search(r"^\s+- --kv-events-config=(.+)$", m, re.M).group(1))
    assert events["topic"] == "kv@$(POD_IP):$(POD_PORT)@Qwen/Qwen3-8B-AWQ" and events["endpoint"] == "$(KV_EVENTS_ENDPOINT)"
    assert "- --block-size=64" in m and '{name: KV_EVENTS_ENDPOINT, value: "tcp://*:5556"}' in m
    assert "{name: kv-replay, containerPort: 5559}" in m and "fieldPath: status.podIP" in m
    c = epp("qwen3-8b-awq", "sliced")
    assert plugin(c, "token-producer")["modelName"] == "Qwen/Qwen3-8B-AWQ"
    assert plugin(c, "precise-prefix-cache-producer")["tokenProcessorConfig"] == {"blockSizeTokens": 64, "hashAlgorithm": "xxh64"}
    assert plugin(c, "precise-prefix-cache-producer")["kvEventsConfig"]["podDiscoveryConfig"] == {"socketPort": 5556, "replaySocketPort": 5559}
    assert c["schedulingProfiles"][0]["plugins"] == [{"pluginRef": "prefix-cache-affinity-filter"}, {"pluginRef": "token-load-scorer"}]


def test_hybrid_routes_approximately_and_leaves_the_engine_block_alone():
    m = profiles.render_manifest(profiles.load_profile("qwen3.5-9b"), "full", llmd=True)
    assert "OffloadingConnector" in m and "--block-size" not in m and "kv-events" not in m and "POD_IP" not in m
    c = epp("qwen3.5-9b", "full")
    assert c["schedulingProfiles"][0]["plugins"] == [
        {"pluginRef": "queue-scorer", "weight": 2}, {"pluginRef": "kv-cache-utilization-scorer", "weight": 2},
        {"pluginRef": "prefix-cache-scorer", "weight": 3}, {"pluginRef": "no-hit-lru-scorer", "weight": 2}]
    assert "dataLayer" not in c and not any(x["type"] == "token-producer" for x in c["plugins"])


def test_offload_tier_is_sized_in_the_connector_shm_and_memory_request():
    m = profiles.render_manifest(profiles.load_profile("qwen3-8b-awq"), "sliced", llmd=True)
    conf = json.loads(re.search(r"^\s+- --kv-transfer-config=(.+)$", m, re.M).group(1))
    assert conf == {"kv_connector": "OffloadingConnector", "kv_role": "kv_both", "kv_connector_extra_config": {"cpu_bytes_to_use": 34359738368}}
    assert "emptyDir: {medium: Memory, sizeLimit: 34Gi}" in m and 'requests: {memory: "34Gi"}' in m


def test_epp_admission_and_affinity_use_the_fit_numbers_the_gateway_gets(capsys):
    c = epp("qwen3-8b-awq", "sliced")
    assert plugin(c, "concurrency-detector") == {"maxConcurrency": 9, "concurrencyMode": "requests", "headroom": 0.0}
    assert plugin(c, "prefix-cache-affinity-filter")["peakPrefillThroughput"] == 3810
    assert fit.main(["qwen3-8b-awq", "sliced", "--gateway-env"]) == 0
    gw = dict(kv.split("=") for kv in capsys.readouterr().out.split())
    assert (gw["GW_MAX_INFLIGHT"], gw["GW_HOP_PREFILL_TOKENS_PER_S"]) == ("9", "3810")
    assert plugin(epp("qwen3.8-27b-fp8", "h100-half"), "concurrency-detector")["maxConcurrency"] == 4


def test_flow_control_bands_wait_no_longer_than_the_gateway_queue():
    v = parsed(llmd.render_values(profiles.load_profile("qwen3-8b-awq"), "sliced"))["router"]
    assert v["inferenceObjectives"] == [{"name": "doctor-interactive", "priority": 100}, {"name": "doctor-batch", "priority": 0}]
    fc = v["epp"]["pluginsConfig"]["flowControl"]
    assert [(b["priority"], b["defaultRequestTTL"]) for b in fc["priorityBands"]] == [(100, "10s"), (0, "30s")]
    budgets = re.search(r"DefaultBudgets = Budgets\{Interactive: (\d+) \* time.Second, Batch: (\d+) \* time.Second\}",
                        (ROOT / "gateway" / "internal" / "decide" / "request.go").read_text())
    assert [f"{n}s" for n in budgets.groups()] == [b["defaultRequestTTL"] for b in fc["priorityBands"]]
    assert v["inferencePool"]["failureMode"] == "FailOpen" and v["proxy"]["mode"] == "service" and v["epp"]["replicas"] == 1


def test_capacity_shed_signal_counts_every_router_refusal():
    rules = (ROOT / "deploy" / "observability" / "alerts.yaml").read_text()
    shed = set(re.search(r'gateway_capacity_sheds_per_minute\n\s+expr: .*reason=~"([^"]+)"', rules).group(1).split("|"))
    router = {"router_saturated", "router_ttl_expired", "router_evicted", "router_no_endpoints", "router_shutting_down", "router_unknown"}
    assert router <= shed and "tenant_tokens" not in shed


def test_llmd_workers_take_the_api_from_the_router_and_kv_events_only_from_the_epp():
    precise = profiles.render_manifest(profiles.load_profile("qwen3-8b-awq"), "sliced", llmd=True)
    assert precise.endswith("""  ingress:
    - from:
        - podSelector:
            matchLabels: {llm-d-router-proxy: doctor-router-proxy}
        - podSelector:
            matchLabels: {llm-d-router-gateway: doctor-router-epp}
        - podSelector:
            matchLabels: {app: gateway}
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: monitoring}
      ports:
        - {port: 8000, protocol: TCP}
    - from:
        - podSelector:
            matchLabels: {llm-d-router-gateway: doctor-router-epp}
      ports:
        - {port: 5556, protocol: TCP}
        - {port: 5559, protocol: TCP}
""")
    approximate = profiles.render_manifest(profiles.load_profile("qwen3.5-9b"), "sliced", llmd=True)
    assert approximate.endswith("""      ports:
        - {port: 8000, protocol: TCP}
""") and "name: vllm-llm-d" in approximate
