"""The answer key is ground truth, so it is tested against known-good and deliberately wrong diagnoses, per tier."""
import copy

from agent.validate import validate
from evals.build_golden import backend_for
from evals.checker import check

NONE = {"cpu_request": "", "memory_request": ""}


def _run(tasks, tid, d):
    t = tasks[tid]
    return check(t, d, t["expect"]["must_call"], backend_for(t["snapshots"]))


def _finding(ns, kind, name, cat, evidence, affects=(), resize=NONE):
    return {"category": cat, "namespace": ns, "kind": kind, "name": name, "root_cause": "x", "affects": list(affects),
            "evidence": evidence, "fix": "x", "resize": dict(resize), "confidence": "high"}


def test_every_reference_diagnosis_passes(tasks, refs):
    for tid, ref in refs.items():
        r = _run(tasks, tid, ref)
        assert r["pass"], (tid, r["failed"])


def test_every_tier_is_represented(tasks):
    assert {"easy", "multi_hop", "red_herring", "rightsizing"} <= {t["tier"] for t in tasks.values()}


def test_invented_evidence_objects_and_scope_are_rejected(tasks, refs):
    t, ref = tasks["dx-oom"], refs["dx-oom"]
    b = backend_for(t["snapshots"])
    for field, value, prefix in (("evidence", ["lg-report-worker-made-up-c9"], "evidence-exists"), ("name", "ghost-service", "object-exists"),
                                 ("namespace", "kube-system", "scope")):
        bad = copy.deepcopy(ref)
        bad["findings"][0][field] = value
        assert any(f.startswith(prefix) for f in validate(bad, b, t["namespaces"])["failed"]), field


def test_blaming_the_victim_fails_even_if_it_crashes(tasks, refs):
    ref = refs["dx-cascade-db"]
    victim = _finding("inventory", "Deployment", "stock-api", "crashloop_app_error", ref["findings"][0]["evidence"][:1])
    failed = _run(tasks, "dx-cascade-db", {"status": "issue", "findings": [victim], "summary": "x"})["failed"]
    assert any(f.startswith("found:stock-db") for f in failed) and any("blames a victim" in f for f in failed)


def test_root_without_its_victim_breaks_the_chain(tasks, refs):
    d = copy.deepcopy(refs["dx-cascade-db"])
    d["findings"][0]["affects"] = []
    assert any(f.startswith("chain:stock-db") for f in _run(tasks, "dx-cascade-db", d)["failed"])


def test_blaming_the_red_herring_fails(tasks, refs):
    ev = refs["dx-tls-truststore"]["findings"][0]["evidence"]
    d = {"status": "issue", "findings": [_finding("payments", "Deployment", "payments-api", "tls_trust", ev)], "summary": "x"}
    assert any("blames a red herring" in f for f in _run(tasks, "dx-tls-truststore", d)["failed"])


def test_alternative_root_is_accepted(tasks, refs):
    d = copy.deepcopy(refs["dx-tls-expired"])
    d["findings"][0].update(kind="ConfigMap", name="auth-api-cert")
    assert _run(tasks, "dx-tls-expired", d)["pass"]


def test_policy_root_needed_but_workload_finding_is_ok_alongside(tasks, refs):
    d = copy.deepcopy(refs["dx-limitrange-oom"])
    d["findings"].append(_finding("finance", "Deployment", "statement-builder", "oom_killed", d["findings"][0]["evidence"]))
    assert _run(tasks, "dx-limitrange-oom", d)["pass"]
    only_workload = {"status": "issue", "findings": d["findings"][1:], "summary": "x"}
    assert any(f.startswith("found:defaults") for f in _run(tasks, "dx-limitrange-oom", only_workload)["failed"])


def test_rightsizing_trap_and_band(tasks, refs):
    d = copy.deepcopy(refs["dx-rightsizing"])
    ev = d["findings"][0]["evidence"]
    trap = copy.deepcopy(d)
    trap["findings"].append(_finding("analytics", "Deployment", "ingest-worker", "overprovisioned", ev, resize={"cpu_request": "10m", "memory_request": "16Mi"}))
    assert any(f.startswith("trap:ingest-worker") for f in _run(tasks, "dx-rightsizing", trap)["failed"])
    greedy = copy.deepcopy(d)
    greedy["findings"][0]["resize"] = {"cpu_request": "1m", "memory_request": "1Mi"}      # below observed usage
    assert any(f.startswith("resize:reporting-api") for f in _run(tasks, "dx-rightsizing", greedy)["failed"])
    throttling_ok = copy.deepcopy(d)
    throttling_ok["findings"].append(_finding("analytics", "Deployment", "ingest-worker", "cpu_throttling", ev))
    assert _run(tasks, "dx-rightsizing", throttling_ok)["pass"]


def test_false_positive_on_healthy_and_wrong_category(tasks, refs):
    fp = {"status": "issue", "findings": [_finding("status", "Deployment", "status-page", "other", ["ds-deployment-status-page"])], "summary": "x"}
    failed = _run(tasks, "dx-healthy", fp)["failed"]
    assert any(f.startswith("status") for f in failed) and any("has nothing wrong" in f for f in failed)
    d = copy.deepcopy(refs["dx-oom"])
    d["findings"][0]["category"] = "image_pull"
    assert any(f.startswith("category") for f in _run(tasks, "dx-oom", d)["failed"])


def test_pod_level_finding_counts_for_its_owner(tasks, refs):
    t, d = tasks["dx-crashloop"], copy.deepcopy(refs["dx-crashloop"])
    pod = next(p["metadata"]["name"] for p in backend_for(t["snapshots"]).objects("pods", "orders"))
    d["findings"][0].update(kind="Pod", name=pod)
    assert _run(tasks, "dx-crashloop", d)["pass"]
