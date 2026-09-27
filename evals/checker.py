"""Score one diagnosis against the injected fault's answer key. Deterministic; nothing is model-judged.

    check(task, diagnosis, trace, backend) -> {"pass": bool, "failed": [...], "rules_checked": n}

Rules: everything `doctor.validate` checks (shape; named objects and victims exist; evidence refs exist)
plus, from the answer key (D-28, D-31):
  status          issue vs healthy is right
  found:<root>    a finding names the ROOT object (or an accepted alternative, or a pod/ReplicaSet it owns)
  category:<root> with an allowed category
  evidence-type   citing at least one ref of an expected evidence type
  chain:<root>    for multi-hop faults, the finding's `affects` names every victim
  resize:<root>   for over-provisioning, the recommended requests fall inside the safe band
  no-false-positive  no finding blames a victim, a red herring, or a healthy object (unless also_ok)
  trap:<obj>      no finding hits a forbidden (object, category) — e.g. calling a throttled pod over-provisioned
  tools-called, max-steps
"""
from __future__ import annotations

from doctor import tools as T
from doctor.backends import Backend
from doctor.quantity import cpu_m, mem_mi
from doctor.validate import validate


def _owned_by(b: Backend, ns: str, kind: str, name: str, target: dict) -> bool:
    """Does (kind, name) refer to target itself or to a pod/ReplicaSet it owns?"""
    if (kind, name) == (target["kind"], target["name"]):
        return True
    if kind == "Pod":
        pod = next((p for p in b.objects("pods", ns) if p["metadata"]["name"] == name), None)
        if pod:
            o = T.owner_of(pod, b.objects("replicasets", ns))
            return (o["kind"], o["name"]) == (target["kind"], target["name"])
    if kind == "ReplicaSet" and target["kind"] == "Deployment":
        rs = next((r for r in b.objects("replicasets", ns) if r["metadata"]["name"] == name), None)
        up = ((rs or {}).get("metadata", {}).get("ownerReferences") or [{}])[0]
        return up.get("name") == target["name"]
    return False


def _is(b: Backend, f: dict, target: dict) -> bool:
    ns = target.get("namespace", f["namespace"])
    return f["namespace"] == ns and any(_owned_by(b, ns, f["kind"], f["name"], c) for c in [target, *target.get("alternatives", [])])


def _in_band(value: float | None, band: list[float]) -> bool:
    return value is not None and band[0] <= value <= band[1]


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
        hits = [f for f in findings if _is(b, f, t)]
        rule(f"found:{t['name']}", bool(hits), f"no finding on the root {t['kind']} {t['namespace']}/{t['name']}")
        if not hits:
            continue
        rule(f"category:{t['name']}", any(f["category"] in t["categories"] for f in hits),
             f"{[f['category'] for f in hits]} not in {t['categories']}")
        types = {T.evidence_type(r) for f in hits for r in f["evidence"]}
        rule(f"evidence-type:{t['name']}", bool(types & set(t["evidence_types"])),
             f"cited {sorted(x for x in types if x)}, expected one of {t['evidence_types']}")
        for victim in t.get("affects", []):
            named = any(_is(b, {**a}, {**victim, "namespace": victim.get("namespace", t["namespace"])})
                        for f in hits for a in f["affects"])
            rule(f"chain:{t['name']}", named, f"root found but its victim {victim['kind']} {victim['name']} is not in affects")
        if t.get("resize_band"):
            band = t["resize_band"]
            ok = any(f["category"] == "overprovisioned"
                     and (not f["resize"]["cpu_request"] or _in_band(cpu_m(f["resize"]["cpu_request"]), band["cpu_m"]))
                     and (not f["resize"]["memory_request"] or _in_band(mem_mi(f["resize"]["memory_request"]), band["memory_mi"]))
                     and (f["resize"]["cpu_request"] or f["resize"]["memory_request"]) for f in hits)
            rule(f"resize:{t['name']}", ok, f"recommended {[f['resize'] for f in hits]} outside safe band {band}")

    victims = [dict(a, namespace=a.get("namespace", t["namespace"])) for t in exp["findings"] for a in t.get("affects", [])]
    herrings = exp.get("red_herrings", [])
    for f in findings:
        if any(_is(b, f, t) for t in exp["findings"]):
            continue
        ok = any(_is(b, f, o) and (not o.get("categories") or f["category"] in o["categories"]) for o in exp.get("also_ok", []))
        why = ("blames a victim" if any(_is(b, f, x) for x in victims) else
               "blames a red herring" if any(_is(b, f, x) for x in herrings) else "has nothing wrong")
        rule("no-false-positive", ok, f"{f['kind']} {f['namespace']}/{f['name']} ({f['category']}) {why}")
    for trap in exp.get("forbidden", []):
        rule(f"trap:{trap['name']}", not any(_is(b, f, trap) and f["category"] in trap["categories"] for f in findings),
             f"{trap['kind']} {trap['name']} flagged as {trap['categories']}")
    rule("tools-called", set(exp["must_call"]) <= set(trace), f"missing {sorted(set(exp['must_call']) - set(trace))}")
    if trace:
        rule("max-steps", len(trace) <= task["max_steps"], f"{len(trace)} > {task['max_steps']}")
    return {"pass": not failed, "failed": failed, "rules_checked": n}
