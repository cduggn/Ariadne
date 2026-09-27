"""The loop, with a scripted fake model: happy path, repair path, fail-closed path, prompt layout, headers."""
import json

from doctor import agent
from evals.build_golden import backend_for
from evals.checker import check


def _call(name, args, i=0):
    return {"choices": [{"message": {"tool_calls": [{"id": f"c{i}", "function": {"name": name, "arguments": json.dumps(args)}}]}}],
            "usage": {"prompt_tokens": 3000 + 300 * i, "completion_tokens": 60}}


def scripted(*calls):
    it = iter(enumerate(calls))
    seen = []

    def chat(messages, rid, headers):
        seen.append(([dict(m) for m in messages], headers))
        i, (name, args) = next(it)
        return _call(name, args, i)
    chat.seen = seen
    return chat


def test_reference_run_passes_the_checker(tasks, refs):
    t = tasks["dx-crashloop"]
    b = backend_for(t["snapshots"])
    ns = t["namespaces"][0]
    chat = scripted(("list_problem_pods", {"namespace": ns}),
                    ("get_events", {"namespace": ns, "object_name": "any", "limit": 10}),
                    ("pod_logs", {"namespace": ns, "pod": b.objects("pods", ns)[0]["metadata"]["name"], "previous": False, "tail": 40}),
                    ("submit_diagnosis", refs["dx-crashloop"]))
    run = agent.run_task(t, b, chat)
    assert run["stop"] == "submitted"
    assert check(t, run["diagnosis"], run["trace"], b)["pass"]


def test_prompt_layout_and_headers(tasks, refs):
    t1, t2 = tasks["dx-oom"], tasks["dx-audit-1"]
    c1 = scripted(("submit_diagnosis", refs["dx-oom"]))
    c2 = scripted(("submit_diagnosis", refs["dx-audit-1"]))
    agent.run_task(t1, backend_for(t1["snapshots"]), c1)
    agent.run_task(t2, backend_for(t2["snapshots"]), c2)
    (m1, h1), (m2, h2) = c1.seen[0], c2.seen[0]
    assert [m["role"] for m in m1] == ["system", "user", "user"]
    assert m1[0] == m2[0] and m1[1] == m2[1]                      # ruleset + cluster card identical → cacheable prefix
    assert m1[1]["content"].startswith("<cluster_card>") and m1[2] != m2[2]
    assert h1["X-Priority"] == "interactive" and h2["X-Priority"] == "batch" and h1["X-Data-Class"] == "restricted"


def test_repair_then_fail_closed(tasks, refs):
    t = tasks["dx-oom"]
    b = backend_for(t["snapshots"])
    bad = json.loads(json.dumps(refs["dx-oom"]))
    bad["findings"][0]["evidence"] = ["lg-invented-c1"]
    chat = scripted(("submit_diagnosis", bad), ("submit_diagnosis", bad), ("submit_diagnosis", bad))
    run = agent.run_task(t, b, chat)
    assert run["repairs"] == 2 and run["stop"] == "inconclusive"
    assert run["diagnosis"]["status"] == "inconclusive" and run["diagnosis"]["findings"] == []
    assert not check(t, run["diagnosis"], run["trace"], b)["pass"]


def test_duplicate_calls_refused_and_harness_off(tasks):
    t = tasks["dx-oom"]
    b = backend_for(t["snapshots"])
    args = {"namespace": "reports"}
    tool_msgs = []

    def repeat(messages, rid, headers):
        if messages[-1]["role"] == "tool":
            tool_msgs.append(json.loads(messages[-1]["content"]))
        return _call("list_problem_pods", args)
    run = agent.run_task({**t, "max_steps": 4}, b, repeat)
    assert run["stop"] == "step_cap"
    assert isinstance(tool_msgs[0].get("problem_pods"), list)            # first call executed
    assert "duplicate call" in tool_msgs[1]["error"]                     # identical repeat refused
    run = agent.run_task({**t, "max_steps": 3}, b, lambda m, r, h: _call("list_problem_pods", args), harness=False)
    assert all(not c["error"] for s in run["steps"] for c in s["calls"])
