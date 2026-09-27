"""Score one diagnosis against the injected fault's answer key. Deterministic; nothing is model-judged.

    check(task, diagnosis, trace, backend) -> {"pass": bool, "failed": [...], "rules_checked": n}

Rules: everything `doctor.validate` checks (shape, objects exist, evidence refs exist) plus, from the
answer key: the status is right; every injected fault is found on the right object with an allowed
category; each finding cites at least one evidence ref of an expected type; no finding lands on an
object that has nothing wrong (false positive); required tools were called; the step cap held.
"""
from __future__ import annotations

from doctor import tools as T
from doctor.backends import Backend
from doctor.validate import validate


def _owned_by(b: Backend, ns: str, kind: str, name: str, truth: dict) -> bool:
    """Does (kind, name) refer to the truth object itself or a pod/replicaset it owns?"""
    if (kind, name) == (truth["kind"], truth["name"]):
        return True
    if kind == "Pod":
        pod = next((p for p in b.objects("pods", ns) if p["metadata"]["name"] == name), None)
        if pod:
            o = T.owner_of(pod, b.objects("replicasets", ns))
            return (o["kind"], o["name"]) == (truth["kind"], truth["name"])
    if kind == "ReplicaSet" and truth["kind"] == "Deployment":
        rs = next((r for r in b.objects("replicasets", ns) if r["metadata"]["name"] == name), None)
        up = ((rs or {}).get("metadata", {}).get("ownerReferences") or [{}])[0]
        return up.get("name") == truth["name"]
    return False


def check(task: dict, d: dict | None, trace: list[str], b: Backend) -> dict:
    exp = task["expect"]
    failed: list[str] = []
    n = 0

    def rule(name: str, ok: bool, detail: str = "") -> None:
        nonlocal n
        n += 1
        if not ok:
            failed.append(f"{name}: {detail}".rstrip(": "))

    if d is None:
        return {"pass": False, "failed": ["no-diagnosis"], "rules_checked": 1}
    if d.get("status") == "inconclusive":
        return {"pass": False, "failed": ["inconclusive: failed closed after repairs"], "rules_checked": 1}

    v = validate(d, b, task["namespaces"])
    rule("grounded", v["pass"], "; ".join(v["failed"])[:300])
    if any(f.startswith("schema") for f in v["failed"]):
        return {"pass": False, "failed": failed, "rules_checked": n}

    rule("status", d["status"] == exp["status"], f"{d['status']} != {exp['status']}")
    findings = d["findings"]
    for t in exp["findings"]:
        hits = [f for f in findings if f["namespace"] == t["namespace"] and _owned_by(b, t["namespace"], f["kind"], f["name"], t)]
        rule(f"found:{t['name']}", bool(hits), f"no finding on {t['kind']} {t['namespace']}/{t['name']}")
        if not hits:
            continue
        rule(f"category:{t['name']}", any(f["category"] in t["categories"] for f in hits),
             f"{[f['category'] for f in hits]} not in {t['categories']}")
        types = {T.evidence_type(r) for f in hits for r in f["evidence"]}
        rule(f"evidence-type:{t['name']}", bool(types & set(t["evidence_types"])),
             f"cited {sorted(x for x in types if x)}, expected one of {t['evidence_types']}")
    for f in findings:
        legit = any(f["namespace"] == t["namespace"] and _owned_by(b, t["namespace"], f["kind"], f["name"], t) for t in exp["findings"])
        rule("no-false-positive", legit, f"{f['kind']} {f['namespace']}/{f['name']} has nothing wrong")
    rule("tools-called", set(exp["must_call"]) <= set(trace), f"missing {sorted(set(exp['must_call']) - set(trace))}")
    if trace:
        rule("max-steps", len(trace) <= task["max_steps"], f"{len(trace)} > {task['max_steps']}")
    return {"pass": not failed, "failed": failed, "rules_checked": n}
