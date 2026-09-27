"""Expect-blind validation of a submitted diagnosis (used inside the agent loop). Standard library only.

It never reads the task's answer key: it checks shape, that every named object exists, and that every
evidence ref exists in what the tools could have returned for that namespace. A diagnosis that fails
is sent back to the model with the errors; after the repair budget it is replaced by an
`inconclusive` answer (fail closed, D-24) — an ungrounded diagnosis never reaches a user.
"""
from __future__ import annotations

import json
import re
from pathlib import Path

from . import tools as T
from .backends import Backend

SCHEMA = json.loads((Path(__file__).parent / "schemas" / "diagnosis.schema.json").read_text())
FINDING = SCHEMA["properties"]["findings"]["items"]
REF_RE = re.compile(FINDING["properties"]["evidence"]["items"]["pattern"])


def schema_errors(d: dict) -> list[str]:
    errs = []
    if not isinstance(d, dict):
        return ["schema: diagnosis is not an object"]
    for k in SCHEMA["required"]:
        if k not in d:
            errs.append(f"schema: missing {k}")
    if errs:
        return errs
    if d["status"] not in SCHEMA["properties"]["status"]["enum"]:
        errs.append(f"schema: bad status {d['status']!r}")
    if not isinstance(d["findings"], list) or len(d["findings"]) > SCHEMA["properties"]["findings"]["maxItems"]:
        errs.append("schema: findings must be a list of at most 8")
        return errs
    for i, f in enumerate(d["findings"]):
        missing = [k for k in FINDING["required"] if k not in f]
        if missing:
            errs.append(f"schema: finding {i} missing {missing}")
            continue
        if f["category"] not in FINDING["properties"]["category"]["enum"]:
            errs.append(f"schema: finding {i} bad category {f['category']!r}")
        if f["kind"] not in FINDING["properties"]["kind"]["enum"]:
            errs.append(f"schema: finding {i} bad kind {f['kind']!r}")
        if not isinstance(f["evidence"], list) or not 1 <= len(f["evidence"]) <= 6:
            errs.append(f"schema: finding {i} needs 1-6 evidence refs")
        elif any(not isinstance(r, str) or not REF_RE.match(r) for r in f["evidence"]):
            errs.append(f"schema: finding {i} has a malformed ref")
    return errs


def validate(d: dict, b: Backend, namespaces: list[str]) -> dict:
    failed = schema_errors(d)
    if failed:
        return {"pass": False, "failed": failed}
    if d["status"] == "healthy" and d["findings"]:
        failed.append("consistency: status healthy but findings listed")
    if d["status"] == "issue" and not d["findings"]:
        failed.append("consistency: status issue but no findings")
    refs = {ns: T.all_refs(b, ns) for ns in namespaces}
    objs = {ns: T.objects_in(b, ns) for ns in namespaces}
    for f in d["findings"]:
        ns = f["namespace"]
        if ns not in namespaces:
            failed.append(f"scope: finding names namespace {ns!r}, which is not part of this task")
            continue
        if (f["kind"], f["name"]) not in objs[ns]:
            failed.append(f"object-exists: {f['kind']} {ns}/{f['name']} does not exist")
        bad = [r for r in f["evidence"] if r not in refs[ns]]
        if bad:
            failed.append(f"evidence-exists: refs not returned by any tool for {ns}: {bad}")
        if not f["fix"].strip():
            failed.append(f"fix: {f['name']} needs a suggested fix")
    return {"pass": not failed, "failed": failed}
