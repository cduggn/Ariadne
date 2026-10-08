"""Score one diagnosis against the injected fault's answer key. Deterministic; nothing is model-judged.

    check(task, diagnosis, trace, backend)                      -> {"pass", "failed", "rules_checked"}          legacy (v1)
    check_v2(task, diagnosis, backend, observed=…, calls=…)     -> {"pass", "failed", "advisory", "parts", …}   corrected (v2)

v1 rules (D-28, D-31), kept unchanged for continuity with earlier runs:
  grounded        everything `agent.validate` checks, with evidence against every ref the tools COULD return
  status          issue vs healthy is right
  found:<root>    a finding names the ROOT object (or an accepted alternative, or a pod/ReplicaSet it owns)
  category:<root> with an allowed category
  evidence-type   citing at least one ref of an expected evidence type
  chain:<root>    for multi-hop faults, the finding's `affects` names every victim
  resize:<root>   for over-provisioning, the recommended requests fall inside the safe band
  no-false-positive  no finding blames a victim, a red herring, or a healthy object (unless also_ok)
  trap:<obj>      no finding hits a forbidden (object, category), e.g. calling a throttled pod over-provisioned
  tools-called, max-steps

v2 (D-41) changes what counts as correct, not the answer key's roots:
  grounded        evidence must be in the run's observation ledger, the refs the model was actually shown
  category        also accepts a root's `also_accept` categories: readings the visible evidence supports equally
                  (e.g. crashloop whose log says "DATABASE_URL is not set" → config_missing)
  mechanism:<root>  the finding's root_cause + fix must state the root's facts and none of its contradictions
                  (e.g. port-mismatch: targetPort 8080 vs container port 80; "set targetPort to 8080" fails)
  abstained       a model that submits `inconclusive` fails, labelled apart from a fail-closed `inconclusive`
  advisory        tools-called and max-steps no longer fail a task: a different investigation path is not wrong.
                  The checker reports them as `advisory` so efficiency stays visible.
`parts` breaks one task into sub-scores (root found, category, mechanism, grounded, no false positive) for the matrix.
"""
from __future__ import annotations

import re

from agent import tools as T
from agent.backends import Backend
from agent.quantity import cpu_m, mem_mi
from agent.validate import validate

PARTS = ("submitted", "grounded", "status", "root", "category", "mechanism", "chain", "no_false_positive")


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


def _text(f: dict, where: str) -> str:
    return f["fix"] if where == "fix" else f"{f['root_cause']} {f['fix']}"


def mechanism_errors(t: dict, hits: list[dict]) -> list[str]:
    """Facts every correct explanation states, and contradictions none may; regexes, case-insensitive, on root_cause + fix."""
    m = t.get("mechanism") or {}
    errs = []
    for fact in m.get("facts", []):
        if not any(re.search(fact["re"], _text(f, fact.get("in", "text")), re.I) for f in hits):
            errs.append(f"missing {fact['id']}")
    for bad in m.get("contradictions", []):
        if any(re.search(bad["re"], _text(f, bad.get("in", "text")), re.I) for f in hits):
            errs.append(f"contradicts: {bad['id']}")
    return errs


def _score(task: dict, d: dict | None, b: Backend, *, v2: bool, trace: list[str] | None = None,
           observed: dict | None = None, calls: list[dict] | None = None) -> dict:
    exp = task["expect"]
    failed: list[str] = []
    advisory: list[str] = []
    parts = dict.fromkeys(PARTS, False)
    n = 0

    def rule(name: str, ok: bool, detail: str = "") -> bool:
        nonlocal n
        n += 1
        if not ok:
            failed.append(f"{name}: {detail}".rstrip(": "))
        return ok

    def done() -> dict:
        out = {"pass": not failed, "failed": failed, "rules_checked": n}
        return {**out, "advisory": advisory, "parts": parts} if v2 else out

    if d is None:
        failed.append("no-diagnosis")
        return done()
    if d.get("status") == "inconclusive":
        failed.append("inconclusive: failed closed after repairs" if (not v2 or "rejected_submission" in d)
                      else "abstained: the model said it could not ground a diagnosis")
        return done()
    parts["submitted"] = True

    v = validate(d, b, task["namespaces"], observed=observed if v2 else None)
    parts["grounded"] = rule("grounded", v["pass"], "; ".join(v["failed"])[:300])
    if any(f.startswith("schema") for f in v["failed"]):
        return done()

    parts["status"] = rule("status", d["status"] == exp["status"], f"{d['status']} != {exp['status']}")
    findings = d["findings"]
    roots, cats, mechs, chains = [], [], [], []
    for t in exp["findings"]:
        hits = [f for f in findings if _is(b, f, t)]
        roots.append(rule(f"found:{t['name']}", bool(hits), f"no finding on the root {t['kind']} {t['namespace']}/{t['name']}"))
        if not hits:
            continue
        allowed = t["categories"] + (t.get("also_accept", []) if v2 else [])
        cats.append(rule(f"category:{t['name']}", any(f["category"] in allowed for f in hits),
                         f"{[f['category'] for f in hits]} not in {allowed}"))
        types = {T.evidence_type(r) for f in hits for r in f["evidence"]}
        rule(f"evidence-type:{t['name']}", bool(types & set(t["evidence_types"])),
             f"cited {sorted(x for x in types if x)}, expected one of {t['evidence_types']}")
        if v2:
            errs = mechanism_errors(t, hits)
            mechs.append(rule(f"mechanism:{t['name']}", not errs, "; ".join(errs)))
        for victim in t.get("affects", []):
            named = any(_is(b, {**a}, {**victim, "namespace": victim.get("namespace", t["namespace"])})
                        for f in hits for a in f["affects"])
            chains.append(rule(f"chain:{t['name']}", named, f"root found but its victim {victim['kind']} {victim['name']} is not in affects"))
        if t.get("resize_band"):
            band = t["resize_band"]
            ok = any(f["category"] == "overprovisioned"
                     and (not f["resize"]["cpu_request"] or _in_band(cpu_m(f["resize"]["cpu_request"]), band["cpu_m"]))
                     and (not f["resize"]["memory_request"] or _in_band(mem_mi(f["resize"]["memory_request"]), band["memory_mi"]))
                     and (f["resize"]["cpu_request"] or f["resize"]["memory_request"]) for f in hits)
            rule(f"resize:{t['name']}", ok, f"recommended {[f['resize'] for f in hits]} outside safe band {band}")
    parts["root"] = all(roots)
    parts["category"] = parts["root"] and all(cats)
    parts["mechanism"] = parts["root"] and all(mechs)
    parts["chain"] = parts["root"] and all(chains)

    before = len(failed)
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
    parts["no_false_positive"] = len(failed) == before

    if v2:
        ok_tools = {c["name"] for c in (calls or []) if c.get("name") and not c.get("error")}
        missing = sorted(set(exp["must_call"]) - ok_tools)
        if missing:
            advisory.append(f"tools-called: missing {missing} (a different path is allowed)")
        n_calls = len([c for c in calls or [] if c.get("name")])
        if n_calls > task["max_steps"]:
            advisory.append(f"max-steps: {n_calls} tool calls > {task['max_steps']}")
        return done()
    trace = trace or []
    rule("tools-called", set(exp["must_call"]) <= set(trace), f"missing {sorted(set(exp['must_call']) - set(trace))}")
    if trace:
        rule("max-steps", len(trace) <= task["max_steps"], f"{len(trace)} > {task['max_steps']}")
    return done()


def check(task: dict, d: dict | None, trace: list[str], b: Backend) -> dict:
    """Legacy (v1) score: strict categories, required tools, evidence the tools could return."""
    return _score(task, d, b, v2=False, trace=trace)


def check_v2(task: dict, d: dict | None, b: Backend, *, observed: dict | None, calls: list[dict] | None = None) -> dict:
    """Corrected (v2) score, D-41. `observed` is the run's ledger (None only for reference checks without a trajectory)."""
    return _score(task, d, b, v2=True, observed=observed, calls=calls)
