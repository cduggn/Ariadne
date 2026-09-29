"""The v2 score and the observation ledger (D-41), against the cases the 2026-09-28 review reproduced: a wrong explanation
that passed, a defensible reading that failed, a citation the model never saw, duplicates, malformed findings, and
uncertainty pushed toward "healthy"."""
import copy
import json

from doctor import agent
from doctor.validate import new_ledger, observe, validate
from evals.build_golden import backend_for, replay
from evals.checker import check, check_v2
from tests.mockllm import TRAJECTORIES, Server, llm_for, reference_script


def _v1(tasks, tid, d):
    t = tasks[tid]
    return check(t, d, t["expect"]["must_call"], backend_for(t["snapshots"]))


def _v2(tasks, tid, d, extra_calls=()):
    t = tasks[tid]
    b = backend_for(t["snapshots"])
    ledger, records = replay(b, TRAJECTORIES[tid] + list(extra_calls))
    return check_v2(t, d, b, observed=ledger, calls=records)


def test_reversed_ports_pass_v1_but_fail_the_mechanism_in_v2(tasks, refs):
    """The saved fix-check answer: a Service readiness probe and the ports reversed. v1 passed it."""
    d = copy.deepcopy(refs["dx-port-mismatch"])
    d["findings"][0].update(
        root_cause="Calls to pricing-api fail with connection refused because the service's readiness probe is configured to "
                   "check the wrong port (80) which is not exposed by the container (8080).",
        fix="Update the service's readiness probe to use the correct container port (8080) instead of 80.")
    assert _v1(tasks, "dx-port-mismatch", d)["pass"]
    r = _v2(tasks, "dx-port-mismatch", d)
    mech = next(f for f in r["failed"] if f.startswith("mechanism:pricing-api"))
    assert not r["pass"] and "missing names the targetPort" in mech and "Services have no probes" in mech and "listens on 8080" in mech
    assert r["parts"]["root"] and r["parts"]["category"] and not r["parts"]["mechanism"]
    assert _v2(tasks, "dx-port-mismatch", refs["dx-port-mismatch"])["pass"]


def test_equally_supported_category_fails_v1_but_passes_v2(tasks, refs):
    d = copy.deepcopy(refs["dx-crashloop"])
    d["findings"][0]["category"] = "config_missing"
    assert any(f.startswith("category:orders-api") for f in _v1(tasks, "dx-crashloop", d)["failed"])
    assert _v2(tasks, "dx-crashloop", d)["pass"]
    d["findings"][0]["category"] = "oom_killed"                               # still wrong in v2
    assert any(f.startswith("category:orders-api") for f in _v2(tasks, "dx-crashloop", d)["failed"])
    ep = copy.deepcopy(refs["dx-no-endpoints"])
    ep["findings"][0]["category"] = "service_misconfig"
    assert _v2(tasks, "dx-no-endpoints", ep)["pass"] and not _v1(tasks, "dx-no-endpoints", ep)["pass"]


def test_a_real_ref_the_model_never_saw_is_not_evidence(tasks, refs):
    """Review repro: an existing but unrelated ref (another Deployment) supported a pricing finding under v1."""
    d = copy.deepcopy(refs["dx-port-mismatch"])
    d["findings"][0]["evidence"].append("ds-deployment-traffic")
    assert _v1(tasks, "dx-port-mismatch", d)["pass"]                         # exists → v1 accepts it
    r = _v2(tasks, "dx-port-mismatch", d)
    assert not r["pass"] and "ds-deployment-traffic" in r["failed"][0] and "to you by a tool call" in r["failed"][0]
    extra = [["describe", {"kind": "deployment", "namespace": "pricing", "name": "traffic"}]]
    assert _v2(tasks, "dx-port-mismatch", d, extra)["pass"]                 # once shown, it may be cited


def test_ledger_records_only_results_the_model_received():
    b = backend_for(["crashloop"])
    led = observe(new_ledger(), "list_problem_pods", {"namespace": "orders", "problem_pods": [{"ref": "st-a", "owner": {}}]})
    led = observe(led, "describe", {"error": "not found"})                   # errors add nothing
    led = observe(led, "describe", {"ref": "ds-node-n1", "namespace": None})  # cluster-wide results key ""
    assert led == {"refs": {"orders": ["st-a"], "": ["ds-node-n1"]}, "tools": {"orders": ["list_problem_pods"], "": ["describe"]}}
    assert b is not None


def test_duplicates_malformed_findings_and_unknown_keys_are_repairs_not_crashes(tasks, refs):
    t = tasks["dx-crashloop"]
    b = backend_for(t["snapshots"])
    pod = b.objects("pods", "orders")[0]["metadata"]["name"]
    d = copy.deepcopy(refs["dx-crashloop"])
    d["findings"].append(dict(d["findings"][0], kind="Pod", name=pod))       # the same root through its pod
    assert any(f.startswith("duplicate: findings 0 and 1") for f in validate(d, b, ["orders"])["failed"])
    for bad in ({"status": "issue", "findings": [None], "summary": "x"},
                {"status": "issue", "findings": [dict(refs["dx-crashloop"]["findings"][0], extra=1)], "summary": "x"},
                {"status": "issue", "findings": [dict(refs["dx-crashloop"]["findings"][0], root_cause="x" * 301)], "summary": "x"},
                {"status": "fine", "findings": [], "summary": "x"}):
        v = validate(bad, b, ["orders"])
        assert not v["pass"] and all(f.startswith("schema:") for f in v["failed"]), v


def test_healthy_needs_coverage_and_inconclusive_is_a_supported_answer(tasks, refs):
    t = tasks["dx-healthy"]
    b = backend_for(t["snapshots"])
    healthy = refs["dx-healthy"]
    assert any(f.startswith("coverage:") for f in validate(healthy, b, ["status"], observed=new_ledger())["failed"])
    ledger, _ = replay(b, TRAJECTORIES["dx-healthy"])
    assert validate(healthy, b, ["status"], observed=ledger)["pass"]
    unsure = {"status": "inconclusive", "findings": [], "summary": "Logs for the failing pod were unavailable; could not confirm a cause."}
    assert validate(unsure, b, ["status"], observed=ledger)["pass"]
    assert not validate({**unsure, "summary": "unsure"}, b, ["status"])["pass"]
    assert not validate({**unsure, "findings": refs["dx-oom"]["findings"]}, b, ["status"])["pass"]


def test_an_abstaining_model_stops_as_abstained_and_scores_apart_from_fail_closed(tasks, refs):
    t = tasks["dx-oom"]
    b = backend_for(t["snapshots"])
    unsure = {"status": "inconclusive", "findings": [], "summary": "report-worker restarts but I could not read its logs or events."}
    s = Server(("list_problem_pods", {"namespace": "reports"}), ("submit_diagnosis", unsure))
    run = agent.run_task(t, b, llm_for(s))
    assert run["stop"] == "abstained" and run["diagnosis"]["status"] == "inconclusive" and run["observed_refs"] >= 1
    r = check_v2(t, run["diagnosis"], b, observed=run["observed"])
    assert r["failed"] == ["abstained: the model said it could not ground a diagnosis"] and not r["parts"]["submitted"]
    closed = agent.inconclusive(refs["dx-oom"], ["evidence-exists: x"])
    assert check_v2(t, closed, b, observed=None)["failed"] == ["inconclusive: failed closed after repairs"]


def test_near_the_cap_the_nudge_offers_inconclusive_not_healthy(tasks):
    t = {**tasks["dx-oom"], "max_steps": 4}
    s = Server(("list_problem_pods", {"namespace": "reports"}))
    agent.run_task(t, backend_for(t["snapshots"]), llm_for(s))
    nudge = json.dumps(s.requests[-1]["body"]["messages"])
    assert "status inconclusive" in nudge and "healthy only if you checked" in nudge and "status healthy if none" not in nudge


def test_required_tools_are_advisory_in_v2(tasks, refs):
    """init-wait: the right diagnosis without pod_logs failed v1 on must_call; v2 passes and reports it."""
    t = tasks["dx-init-wait"]
    assert "pod_logs" in t["expect"]["must_call"]
    b = backend_for(t["snapshots"])
    path = [c for c in TRAJECTORIES["dx-init-wait"] if c[0] != "pod_logs"]
    ref = copy.deepcopy(refs["dx-init-wait"])
    ledger, records = replay(b, path)
    shown = set(ledger["refs"].get("accounts", []))
    ref["findings"][0]["evidence"] = [r for r in ref["findings"][0]["evidence"] if r in shown]
    assert not check(t, ref, [c[0] for c in path], b)["pass"]
    r = check_v2(t, ref, b, observed=ledger, calls=records)
    assert r["pass"] and any(a.startswith("tools-called: missing ['pod_logs']") for a in r["advisory"])


def test_the_agent_rejects_an_unseen_citation_then_accepts_it_after_the_call(tasks, refs):
    t = tasks["dx-port-mismatch"]
    b = backend_for(t["snapshots"])
    d = copy.deepcopy(refs["dx-port-mismatch"])
    d["findings"][0]["evidence"].append("ds-deployment-traffic")
    script = reference_script("dx-port-mismatch", d)
    s = Server(*script[:-1], script[-1], ("describe", {"kind": "deployment", "namespace": "pricing", "name": "traffic"}), script[-1])
    run = agent.run_task(t, b, llm_for(s))
    assert run["stop"] == "submitted" and run["repairs"] == 1
    assert "to you by a tool call in this run" in json.dumps(s.requests[len(script)]["body"]["messages"][-1])
