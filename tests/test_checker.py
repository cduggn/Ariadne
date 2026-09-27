"""The answer key is ground truth, so it is tested against known-good and deliberately broken diagnoses."""
import copy

from doctor.validate import validate
from evals.build_golden import backend_for
from evals.checker import check


def test_every_reference_diagnosis_passes(tasks, refs):
    for tid, ref in refs.items():
        t = tasks[tid]
        r = check(t, ref, t["expect"]["must_call"], backend_for(t["snapshots"]))
        assert r["pass"], (tid, r["failed"])


def test_invented_evidence_and_objects_are_rejected(tasks, refs):
    t, ref = tasks["dx-oom"], refs["dx-oom"]
    b = backend_for(t["snapshots"])
    bad = copy.deepcopy(ref)
    bad["findings"][0]["evidence"] = ["lg-report-worker-made-up-c9"]
    assert any(f.startswith("evidence-exists") for f in validate(bad, b, t["namespaces"])["failed"])
    bad = copy.deepcopy(ref)
    bad["findings"][0]["name"] = "ghost-service"
    assert any(f.startswith("object-exists") for f in validate(bad, b, t["namespaces"])["failed"])
    bad = copy.deepcopy(ref)
    bad["findings"][0]["namespace"] = "kube-system"
    assert any(f.startswith("scope") for f in validate(bad, b, t["namespaces"])["failed"])


def test_wrong_category_false_positive_and_wrong_status_fail(tasks, refs):
    t, ref = tasks["dx-oom"], refs["dx-oom"]
    b = backend_for(t["snapshots"])
    bad = copy.deepcopy(ref)
    bad["findings"][0]["category"] = "image_pull"
    assert any(f.startswith("category") for f in check(t, bad, t["expect"]["must_call"], b)["failed"])
    h = tasks["dx-healthy"]
    hb = backend_for(h["snapshots"])
    fp = {"status": "issue", "findings": [{"category": "other", "namespace": "status", "kind": "Deployment", "name": "status-page",
                                           "root_cause": "x", "evidence": ["ds-deployment-status-page"], "fix": "x", "confidence": "low"}],
          "summary": "x"}
    failed = check(h, fp, h["expect"]["must_call"], hb)["failed"]
    assert any(f.startswith("status") for f in failed) and any(f.startswith("no-false-positive") for f in failed)


def test_pod_level_finding_counts_for_its_owner(tasks, refs):
    t, ref = tasks["dx-crashloop"], copy.deepcopy(refs["dx-crashloop"])
    b = backend_for(t["snapshots"])
    pod = next(p["metadata"]["name"] for p in b.objects("pods", "orders"))
    ref["findings"][0].update(kind="Pod", name=pod)
    assert check(t, ref, t["expect"]["must_call"], b)["pass"]


def test_mixed_namespace_needs_both_findings(tasks, refs):
    t, ref = tasks["dx-mixed"], copy.deepcopy(refs["dx-mixed"])
    b = backend_for(t["snapshots"])
    ref["findings"] = ref["findings"][:1]
    assert any(f.startswith("found:ledger-api") for f in check(t, ref, t["expect"]["must_call"], b)["failed"])
