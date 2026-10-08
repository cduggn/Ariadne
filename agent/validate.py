"""Expect-blind validation of a submitted diagnosis (used inside the agent loop). Standard library only.

It never reads the task's answer key. It checks:
  schema       the whole diagnosis against diagnosis.schema.json: types, enums, patterns, lengths, no unknown keys
  consistency  issue ⇔ findings; healthy and inconclusive have none; inconclusive says in `summary` what it could not check
  scope        findings and victims are in the task's namespaces, and every named object exists
  evidence     every cited ref is in the observation ledger, which holds what tool calls in this run RETURNED TO THE MODEL (D-41).
               Without a ledger (reference checks, the legacy score) a ref only has to be one the tools could return.
  duplicates   one finding per root cause: two findings on the same object (a pod or ReplicaSet counts as its owner)
  coverage     with a ledger, `healthy` needs a successful list_problem_pods for every namespace in the task
The agent loop sends a failing diagnosis back to the model with the errors. After the repair budget it replaces the
diagnosis with an `inconclusive` answer (fail closed, D-24), so an ungrounded diagnosis never reaches a user.
"""
from __future__ import annotations

import json
import re
from pathlib import Path

from . import tools as T
from .backends import Backend
from .quantity import cpu_m, mem_mi

SCHEMA = json.loads((Path(__file__).parent / "schemas" / "diagnosis.schema.json").read_text())
FINDING = SCHEMA["properties"]["findings"]["items"]
MAX_SCHEMA_ERRORS = 8


# ---- the observation ledger: what the model has actually been shown ------------------------------

def new_ledger() -> dict:
    return {"refs": {}, "tools": {}}


def _refs_in(x) -> list[str]:
    if isinstance(x, dict):
        own = [x["ref"]] if isinstance(x.get("ref"), str) else []
        return own + [r for v in x.values() for r in _refs_in(v)]
    if isinstance(x, list):
        return [r for v in x for r in _refs_in(v)]
    return []


def observe(ledger: dict, tool: str, result) -> dict:
    """Record one tool result the model received. Errors and refusals record nothing. Returns the ledger (a new dict)."""
    if not isinstance(result, dict) or "error" in result or tool == "submit_diagnosis":
        return ledger
    ns = result.get("namespace") or ""             # node, cost and S3 results are cluster-wide, so they go under key ""
    refs = {k: list(v) for k, v in ledger["refs"].items()}
    tools = {k: list(v) for k, v in ledger["tools"].items()}
    refs[ns] = list(dict.fromkeys(refs.get(ns, []) + _refs_in(result)))
    tools[ns] = list(dict.fromkeys(tools.get(ns, []) + [tool]))
    return {"refs": refs, "tools": tools}


def ledger_size(ledger: dict | None) -> int:
    return sum(len(v) for v in (ledger or {}).get("refs", {}).values())


# ---- schema ------------------------------------------------------------------------------------

def _check(v, s: dict, path: str, errs: list[str]) -> None:
    t = s.get("type")
    if t == "object":
        if not isinstance(v, dict):
            errs.append(f"schema: {path} must be an object")
            return
        errs += [f"schema: {path} missing {k}" for k in s.get("required", []) if k not in v]
        props = s.get("properties", {})
        extra = sorted(set(v) - set(props)) if s.get("additionalProperties") is False else []
        if extra:
            errs.append(f"schema: {path} has unknown keys {extra}")
        for k, sub in props.items():
            if k in v:
                _check(v[k], sub, f"{path}.{k}", errs)
    elif t == "array":
        if not isinstance(v, list):
            errs.append(f"schema: {path} must be a list")
            return
        if len(v) < s.get("minItems", 0) or len(v) > s.get("maxItems", 10**6):
            errs.append(f"schema: {path} needs {s.get('minItems', 0)}-{s.get('maxItems', 'any')} items, has {len(v)}")
        for i, x in enumerate(v):
            _check(x, s.get("items", {}), f"{path}[{i}]", errs)
    elif t == "string":
        if not isinstance(v, str):
            errs.append(f"schema: {path} must be a string")
            return
        if "enum" in s and v not in s["enum"]:
            allowed = f"one of {s['enum']}" if len(s["enum"]) <= 6 else "an allowed value (see the tool schema)"
            errs.append(f"schema: {path} is {v[:40]!r}, not {allowed}")
        if "pattern" in s and not re.search(s["pattern"], v):
            errs.append(f"schema: {path} {v[:60]!r} is malformed")
        if len(v) > s.get("maxLength", 10**6):
            errs.append(f"schema: {path} is longer than {s['maxLength']} characters")


def schema_errors(d) -> list[str]:
    errs: list[str] = []
    _check(d, SCHEMA, "diagnosis", errs)
    return errs[:MAX_SCHEMA_ERRORS]


# ---- identity ----------------------------------------------------------------------------------

def canonical(b: Backend, ns: str, kind: str, name: str) -> tuple[str, str]:
    """The object a finding really names: a pod or ReplicaSet counts as the workload that owns it."""
    if kind == "Pod":
        pod = next((p for p in b.objects("pods", ns) if p["metadata"]["name"] == name), None)
        if pod:
            o = T.owner_of(pod, b.objects("replicasets", ns))
            return o["kind"], o["name"]
    if kind == "ReplicaSet":
        rs = next((r for r in b.objects("replicasets", ns) if r["metadata"]["name"] == name), None)
        up = ((rs or {}).get("metadata", {}).get("ownerReferences") or [{}])[0]
        if up.get("kind"):
            return up["kind"], up["name"]
    return kind, name


# ---- validation --------------------------------------------------------------------------------

def validate(d: dict, b: Backend, namespaces: list[str], observed: dict | None = None) -> dict:
    failed = schema_errors(d)
    if failed:
        return {"pass": False, "failed": failed}
    status, findings = d["status"], d["findings"]
    if status in ("healthy", "inconclusive") and findings:
        failed.append(f"consistency: status {status} takes no findings")
    if status == "issue" and not findings:
        failed.append("consistency: status issue but no findings")
    if status == "inconclusive" and len(d["summary"].strip()) < 20:
        failed.append("consistency: status inconclusive needs a summary of what you could not check")
    if status == "healthy" and observed is not None:
        missing = [ns for ns in namespaces if "list_problem_pods" not in observed["tools"].get(ns, [])]
        if missing:
            failed.append(f"coverage: status healthy needs list_problem_pods for {missing} first; or submit inconclusive")
    registry = {ns: T.all_refs(b, ns) for ns in namespaces} if observed is None else None
    objs = {ns: T.objects_in(b, ns) for ns in namespaces}
    seen: dict[tuple[str, str, str], int] = {}
    for i, f in enumerate(findings):
        ns = f["namespace"]
        if ns not in namespaces:
            failed.append(f"scope: finding names namespace {ns!r}, which is not part of this task")
            continue
        if (f["kind"], f["name"]) not in objs[ns]:
            failed.append(f"object-exists: {f['kind']} {ns}/{f['name']} does not exist")
        else:
            key = (ns, *canonical(b, ns, f["kind"], f["name"]))
            if key in seen:
                failed.append(f"duplicate: findings {seen[key]} and {i} both name {key[1]} {ns}/{key[2]} (a pod counts as its "
                              "owner); one finding per root cause, victims go in affects")
            seen.setdefault(key, i)
        if observed is None:
            bad = [r for r in f["evidence"] if r not in registry[ns]]
            where = "by any tool"
        else:
            shown = set(observed["refs"].get(ns, [])) | set(observed["refs"].get("", []))
            bad = [r for r in f["evidence"] if r not in shown]
            where = "to you by a tool call in this run"
        if bad:
            failed.append(f"evidence-exists: refs not returned {where} for {ns}: {bad}")
        for a in f["affects"]:
            if a["namespace"] not in namespaces or (a["kind"], a["name"]) not in objs[a["namespace"]]:
                failed.append(f"object-exists: affected {a['kind']} {a['namespace']}/{a['name']} does not exist in this task's namespaces")
        if not f["fix"].strip():
            failed.append(f"fix: {f['name']} needs a suggested fix")
        if f["category"] == "overprovisioned":
            try:
                if cpu_m(f["resize"]["cpu_request"]) is None and mem_mi(f["resize"]["memory_request"]) is None:
                    raise ValueError("empty")
            except ValueError:
                failed.append(f"resize: {f['name']} is overprovisioned but resize has no valid new request (e.g. 50m, 64Mi)")
    return {"pass": not failed, "failed": failed}
