"""The LangGraph agent end to end over the real LangChain client, against a scripted OpenAI-compatible
server (httpx MockTransport). Proves the wire format too: tools.json verbatim, prompt order, headers."""
import json

from doctor import agent
from evals.build_golden import backend_for
from evals.checker import check
from tests.mockllm import Server, llm_for


def test_reference_run_passes_the_checker(tasks, refs):
    t = tasks["dx-crashloop"]
    b = backend_for(t["snapshots"])
    pod = b.objects("pods", "orders")[0]["metadata"]["name"]
    s = Server(("list_problem_pods", {"namespace": "orders"}),
               ("get_events", {"namespace": "orders", "object_name": "any", "limit": 10}),
               ("pod_logs", {"namespace": "orders", "pod": pod, "container": "", "previous": False, "tail": 40}),
               ("submit_diagnosis", refs["dx-crashloop"]))
    run = agent.run_task(t, b, llm_for(s))
    assert run["stop"] == "submitted" and run["trace"] == ["list_problem_pods", "get_events", "pod_logs", "submit_diagnosis"]
    assert check(t, run["diagnosis"], run["trace"], b)["pass"]
    assert run["steps"][0]["cached_tokens"] == 3800 and run["steps"][0]["prompt_tokens"] == 4000


def test_wire_format_tools_prompt_order_and_headers(tasks, refs):
    s1, s2 = Server(("submit_diagnosis", refs["dx-oom"])), Server(("submit_diagnosis", refs["dx-audit-3"]))
    agent.run_task(tasks["dx-oom"], backend_for(tasks["dx-oom"]["snapshots"]), llm_for(s1))
    agent.run_task(tasks["dx-audit-3"], backend_for(tasks["dx-audit-3"]["snapshots"]), llm_for(s2))
    b1, h1 = s1.requests[0]["body"], s1.requests[0]["headers"]
    b2, h2 = s2.requests[0]["body"], s2.requests[0]["headers"]
    assert b1["tools"] == agent.TOOLS                                            # tools.json verbatim
    assert b1["tool_choice"] == "required" and b1["temperature"] == 0 and b1["chat_template_kwargs"] == {"enable_thinking": False}
    roles = [m["role"] for m in b1["messages"]]
    assert roles == ["system", "user", "user"] and b1["messages"][0]["content"] == agent.RULESET
    assert b1["messages"][:2] == b2["messages"][:2]                              # ruleset + card identical → cacheable prefix
    assert b1["messages"][1]["content"].startswith("<cluster_card>") and b1["messages"][2] != b2["messages"][2]
    assert h1["x-priority"] == "interactive" and h2["x-priority"] == "batch" and h1["x-data-class"] == "restricted"
    assert h1["x-request-id"].startswith("dx-oom-") and h1["x-request-id"].endswith("-s1") and h1["x-app"] == "cluster-doctor"


def test_repair_then_fail_closed(tasks, refs):
    t = tasks["dx-oom"]
    b = backend_for(t["snapshots"])
    bad = json.loads(json.dumps(refs["dx-oom"]))
    bad["findings"][0]["evidence"] = ["lg-invented-x-c1"]
    s = Server(("submit_diagnosis", bad))
    run = agent.run_task(t, b, llm_for(s))
    assert run["repairs"] == 2 and run["stop"] == "inconclusive" and len(s.requests) == 3
    assert run["diagnosis"]["status"] == "inconclusive" and run["diagnosis"]["findings"] == []
    assert "evidence-exists" in json.dumps(s.requests[1]["body"]["messages"][-1])     # the model is told what was wrong
    assert not check(t, run["diagnosis"], run["trace"], b)["pass"]


def test_duplicates_budget_step_cap_and_bad_json(tasks):
    t = {**tasks["dx-oom"], "max_steps": 4}
    b = backend_for(t["snapshots"])
    s = Server(("list_problem_pods", {"namespace": "reports"}))
    run = agent.run_task(t, b, llm_for(s))
    tool_msgs = [m for m in s.requests[-1]["body"]["messages"] if m["role"] == "tool"]
    assert run["stop"] == "step_cap" and len(s.requests) == 4
    assert "problem_pods" in tool_msgs[0]["content"] and "duplicate call" in tool_msgs[1]["content"]
    assert "model calls left" in json.dumps(s.requests[-1]["body"]["messages"])          # submit-now nudge
    run = agent.run_task(t, b, llm_for(Server(("describe", "{not json"))))
    assert run["stop"] == "step_cap" and any("not valid JSON" in (c["error"] or "") for st in run["steps"] for c in st["calls"])


def test_gateway_refusal_ends_with_named_stop(tasks):
    t = tasks["dx-oom"]
    run = agent.run_task(t, backend_for(t["snapshots"]), llm_for(Server(status=503)))
    assert run["stop"] == "http_503" and run["diagnosis"] is None and run["steps"][0]["http_status"] == 503
    run = agent.run_task(t, backend_for(t["snapshots"]), llm_for(Server(status=429)))
    assert run["stop"] == "http_429"


def test_multi_hop_reference_run_and_harness_off(tasks, refs):
    t = tasks["dx-cascade-db"]
    b = backend_for(t["snapshots"])
    api = next(p["metadata"]["name"] for p in b.objects("pods", "inventory") if p["metadata"]["name"].startswith("stock-api"))
    s = Server(("list_problem_pods", {"namespace": "inventory"}),
               ("pod_logs", {"namespace": "inventory", "pod": api, "container": "", "previous": False, "tail": 40}),
               ("describe", {"kind": "deployment", "namespace": "inventory", "name": "stock-db"}),
               ("submit_diagnosis", refs["dx-cascade-db"]))
    run = agent.run_task(t, b, llm_for(s))
    assert run["stop"] == "submitted" and check(t, run["diagnosis"], run["trace"], b)["pass"]
    off = agent.run_task({**t, "max_steps": 3}, b, llm_for(Server(("list_problem_pods", {"namespace": "inventory"}))), harness=False)
    assert all(not c["error"] for st in off["steps"] for c in st["calls"])


def test_hosted_tracing_is_forced_off():
    import os
    assert all(os.environ[v] == "false" for v in ("LANGSMITH_TRACING", "LANGCHAIN_TRACING_V2", "LANGCHAIN_TRACING"))
