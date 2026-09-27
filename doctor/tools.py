"""The doctor's tools: small, referenced, redacted views over a Backend (D-21). Standard library only.

Every item a tool returns carries a stable `ref`. A diagnosis must cite refs as evidence; the
validator and the checker recompute the full set of refs from the same backend, so a cited ref that
does not exist is a fabrication and is rejected.

    ref prefix   evidence type   example
    st-          status          st-orders-api-7c9f8-abcde            (pod status, from list_problem_pods)
    ev-          event           ev-3f2a91                            (hash of namespace/object/reason/message)
    ds-          describe        ds-deployment-orders-api
    lg-          log             lg-orders-api-7c9f8-abcde-p12        (p/c = previous/current container, line no.)
    rs-          resources       rs-service-checkout
    mt-          metrics         mt-reports-report-worker-5d8f-xyz
    cs-          cost            cs-aws_by_service_7d

Guards: names are validated (RFC 1123); tools only read; results are size-capped; text is redacted;
log lines that look like instructions to an AI are flagged `suspicious`, never obeyed.
"""
from __future__ import annotations

import hashlib
import json
from collections.abc import Callable

from .backends import Backend, Unavailable, check_name
from .redact import looks_like_injection, redact

MAX_ITEMS = 20
MAX_TEXT = 240
MAX_LOG_TAIL = 80
DESCRIBE_KINDS = {"pod": "pods", "deployment": "deployments", "replicaset": "replicasets", "service": "services",
                  "job": "jobs", "node": "nodes"}
LIST_KINDS = {"deployments", "services", "jobs", "pods", "configmaps"}
EVIDENCE_TYPES = {"st": "status", "ev": "event", "ds": "describe", "lg": "log", "rs": "resources", "mt": "metrics", "cs": "cost"}


def _t(s: object, n: int = MAX_TEXT) -> str:
    s = redact(str(s or ""))
    return s if len(s) <= n else s[: n - 1] + "…"


def _h(*parts: str) -> str:
    return hashlib.sha1("|".join(parts).encode(), usedforsecurity=False).hexdigest()[:6]   # an id, not a security hash


def evidence_type(ref: str) -> str | None:
    return EVIDENCE_TYPES.get(ref.split("-", 1)[0])


# ---- ownership: pod → replicaset → deployment, pod → job ----------------------------------

def owner_of(pod: dict, replicasets: list[dict]) -> dict:
    refs = pod.get("metadata", {}).get("ownerReferences") or []
    if not refs:
        return {"kind": "Pod", "name": pod["metadata"]["name"]}
    o = refs[0]
    if o["kind"] == "ReplicaSet":
        for rs in replicasets:
            if rs["metadata"]["name"] == o["name"]:
                up = (rs["metadata"].get("ownerReferences") or [{}])[0]
                if up.get("kind"):
                    return {"kind": up["kind"], "name": up["name"]}
        return {"kind": "ReplicaSet", "name": o["name"]}
    return {"kind": o["kind"], "name": o["name"]}


def _container_problem(cs: dict) -> tuple[str, str]:
    """Most informative (reason, detail) for one containerStatus."""
    st, last = cs.get("state", {}), cs.get("lastState", {})
    if "waiting" in st:
        w = st["waiting"]
        detail = w.get("message", "")
        if "terminated" in last:
            t = last["terminated"]
            detail = f"last exit {t.get('reason')} (code {t.get('exitCode')}); " + detail
        return w.get("reason", "Waiting"), detail
    if "terminated" in st:
        t = st["terminated"]
        return t.get("reason", "Terminated"), f"exit code {t.get('exitCode')}"
    if "terminated" in last:
        t = last["terminated"]
        return f"Restarted after {t.get('reason')}", f"last exit code {t.get('exitCode')}"
    return ("" if cs.get("ready") else "NotReady"), ""


def _pod_row(p: dict, replicasets: list[dict]) -> dict:
    name = p["metadata"]["name"]
    statuses = p.get("status", {}).get("containerStatuses") or []
    ready = sum(1 for c in statuses if c.get("ready"))
    restarts = sum(c.get("restartCount", 0) for c in statuses)
    reason, detail = "", ""
    for cs in statuses:
        r, d = _container_problem(cs)
        if r:
            reason, detail = r, d
            break
    if not statuses:
        cond = next((c for c in p.get("status", {}).get("conditions", []) if c.get("status") == "False"), {})
        reason, detail = cond.get("reason", p.get("status", {}).get("phase", "")), cond.get("message", "")
    return {"ref": f"st-{name}", "pod": name, "phase": p.get("status", {}).get("phase"),
            "ready": f"{ready}/{len(p.get('spec', {}).get('containers', []))}", "restarts": restarts,
            "reason": _t(reason, 60), "detail": _t(detail, 160), "owner": owner_of(p, replicasets)}


def _is_problem(row: dict, phase: str) -> bool:
    if phase == "Succeeded":
        return False
    return phase != "Running" or row["restarts"] > 0 or row["ready"].split("/")[0] != row["ready"].split("/")[1] or bool(row["reason"])


# ---- tools ---------------------------------------------------------------------------------

def list_problem_pods(b: Backend, namespace: str) -> dict:
    ns = check_name(namespace, "namespace")
    rss = b.objects("replicasets", ns)
    pods = b.objects("pods", ns)
    rows = [_pod_row(p, rss) for p in pods]
    probs = [r for r, p in zip(rows, pods, strict=False) if _is_problem(r, p.get("status", {}).get("phase", ""))]
    return {"namespace": ns, "pods_total": len(pods), "problem_pods": probs[:MAX_ITEMS]}


def get_events(b: Backend, namespace: str, object_name: str, limit: int) -> dict:
    ns = check_name(namespace, "namespace")
    evs = b.objects("events", ns)
    if object_name != "any":
        check_name(object_name)
        evs = [e for e in evs if e.get("involvedObject", {}).get("name", "").startswith(object_name)]
    evs.sort(key=lambda e: (e.get("type") != "Warning", -(e.get("count") or 1), e.get("lastTimestamp") or ""))
    out = []
    for e in evs[: max(1, min(int(limit), MAX_ITEMS))]:
        io = e.get("involvedObject", {})
        out.append({"ref": event_ref(ns, e), "type": e.get("type"), "reason": e.get("reason"),
                    "object": f"{io.get('kind')}/{io.get('name')}", "message": _t(e.get("message")),
                    "count": e.get("count") or 1, "last_seen": e.get("lastTimestamp") or e.get("eventTime")})
    return {"namespace": ns, "events": out, "total": len(evs)}


def event_ref(ns: str, e: dict) -> str:
    io = e.get("involvedObject", {})
    return "ev-" + _h(ns, io.get("kind", ""), io.get("name", ""), e.get("reason", ""), e.get("message", ""))


def _find(b: Backend, kind: str, ns: str, name: str) -> dict:
    for o in b.objects(DESCRIBE_KINDS[kind], ns):
        if o["metadata"]["name"] == name:
            return o
    raise LookupError(f"{kind} {name} not found in namespace {ns}")


def _label_match(selector: dict, labels: dict) -> bool:
    return bool(selector) and all(labels.get(k) == v for k, v in selector.items())


def describe(b: Backend, kind: str, namespace: str, name: str) -> dict:
    if kind not in DESCRIBE_KINDS:
        raise ValueError(f"kind must be one of {sorted(DESCRIBE_KINDS)}")
    ns = "" if kind == "node" else check_name(namespace, "namespace")
    check_name(name)
    o = _find(b, kind, ns, name)
    spec, st = o.get("spec", {}), o.get("status", {})
    out: dict = {"ref": f"ds-{kind}-{name}", "kind": kind, "name": name, "namespace": ns or None}
    if kind == "pod":
        cs = {c["name"]: c for c in st.get("containerStatuses") or []}
        out["containers"] = [{
            "name": c["name"], "image": c.get("image"),
            "resources": c.get("resources", {}),
            "env_from": [ {"configMap": x.get("configMapRef", {}).get("name")} if "configMapRef" in x else
                          {"secret": x.get("secretRef", {}).get("name")} for x in c.get("envFrom", [])],
            "readiness_probe": _probe(c.get("readinessProbe")), "liveness_probe": _probe(c.get("livenessProbe")),
            "state": _t(json.dumps(cs.get(c["name"], {}).get("state", {})), 200),
            "last_state": _t(json.dumps(cs.get(c["name"], {}).get("lastState", {})), 200),
            "restarts": cs.get(c["name"], {}).get("restartCount", 0), "ready": cs.get(c["name"], {}).get("ready", False),
        } for c in spec.get("containers", [])]
        out["node"] = spec.get("nodeName")
        out["node_selector"] = spec.get("nodeSelector")
        out["scheduler"] = spec.get("schedulerName")
        out["conditions"] = [{"type": c["type"], "status": c["status"], "reason": c.get("reason"), "message": _t(c.get("message"), 160)}
                             for c in st.get("conditions", [])]
        out["owner"] = owner_of(o, b.objects("replicasets", ns))
    elif kind in ("deployment", "replicaset"):
        c0 = (spec.get("template", {}).get("spec", {}).get("containers") or [{}])[0]
        out.update({"replicas": {"desired": spec.get("replicas"), "updated": st.get("updatedReplicas", 0), "ready": st.get("readyReplicas", 0),
                                 "available": st.get("availableReplicas", 0), "unavailable": st.get("unavailableReplicas", 0)},
                    "image": c0.get("image"), "selector": spec.get("selector", {}).get("matchLabels"),
                    "strategy": spec.get("strategy"), "progress_deadline_s": spec.get("progressDeadlineSeconds"),
                    "conditions": [{"type": c["type"], "status": c["status"], "reason": c.get("reason"), "message": _t(c.get("message"), 160)}
                                   for c in st.get("conditions", [])]})
    elif kind == "service":
        sel = spec.get("selector") or {}
        pods = b.objects("pods", ns)
        matching = [p["metadata"]["name"] for p in pods if _label_match(sel, p["metadata"].get("labels", {}))]
        eps = [e for e in b.objects("endpointslices", ns) if e.get("metadata", {}).get("labels", {}).get("kubernetes.io/service-name") == name]
        ready_eps = sum(1 for e in eps for ep in (e.get("endpoints") or []) if ep.get("conditions", {}).get("ready"))
        out.update({"type": spec.get("type"), "selector": sel, "ports": spec.get("ports"),
                    "pods_matching_selector": len(matching), "ready_endpoints": ready_eps,
                    "labels_in_namespace": sorted({json.dumps(p["metadata"].get("labels", {}), sort_keys=True) for p in pods})[:5]})
    elif kind == "job":
        out.update({"completions": spec.get("completions"), "backoff_limit": spec.get("backoffLimit"),
                    "succeeded": st.get("succeeded", 0), "failed": st.get("failed", 0), "active": st.get("active", 0),
                    "conditions": [{"type": c["type"], "status": c["status"], "reason": c.get("reason"), "message": _t(c.get("message"), 160)}
                                   for c in st.get("conditions", [])]})
    elif kind == "node":
        out.update({"allocatable": st.get("allocatable"), "labels": o["metadata"].get("labels"),
                    "taints": spec.get("taints"),
                    "conditions": [{"type": c["type"], "status": c["status"], "reason": c.get("reason")} for c in st.get("conditions", [])]})
    return out


def _probe(p: dict | None) -> dict | None:
    if not p:
        return None
    for k in ("httpGet", "tcpSocket", "exec", "grpc"):
        if k in p:
            return {k: p[k], "period_s": p.get("periodSeconds"), "failure_threshold": p.get("failureThreshold")}
    return {"raw": _t(json.dumps(p), 120)}


def pod_logs(b: Backend, namespace: str, pod: str, previous: bool, tail: int) -> dict:
    ns, pod = check_name(namespace, "namespace"), check_name(pod, "pod")
    p = next((x for x in b.objects("pods", ns) if x["metadata"]["name"] == pod), None)
    if p is None:
        raise LookupError(f"pod {pod} not found in namespace {ns}")
    container = p["spec"]["containers"][0]["name"]
    text = b.logs(ns, pod, container, bool(previous))
    lines = text.splitlines()
    n = max(1, min(int(tail), MAX_LOG_TAIL))
    start = max(0, len(lines) - n)
    tag = "p" if previous else "c"
    out = [{"ref": f"lg-{pod}-{tag}{i}", "text": _t(line, 300), **({"suspicious": True} if looks_like_injection(line) else {})}
           for i, line in enumerate(lines[start:], start)]
    return {"namespace": ns, "pod": pod, "container": container, "previous": bool(previous), "lines": out, "total_lines": len(lines)}


def list_resources(b: Backend, kind: str, namespace: str) -> dict:
    if kind not in LIST_KINDS:
        raise ValueError(f"kind must be one of {sorted(LIST_KINDS)}")
    ns = check_name(namespace, "namespace")
    rows = []
    for o in b.objects(kind, ns)[:MAX_ITEMS]:
        n, st, sp = o["metadata"]["name"], o.get("status", {}), o.get("spec", {})
        row: dict = {"ref": f"rs-{kind[:-1]}-{n}", "name": n}
        if kind == "deployments":
            row["ready"] = f"{st.get('readyReplicas', 0)}/{sp.get('replicas')}"
        elif kind == "services":
            row["selector"] = sp.get("selector")
        elif kind == "jobs":
            row.update({"succeeded": st.get("succeeded", 0), "failed": st.get("failed", 0)})
        elif kind == "pods":
            row["phase"] = st.get("phase")
            row["labels"] = o["metadata"].get("labels")
        rows.append(row)
    return {"namespace": ns, "kind": kind, "items": rows}


def resource_usage(b: Backend, namespace: str, preset: str) -> dict:
    ns = check_name(namespace, "namespace")
    if preset == "pods":
        return {"namespace": ns, "preset": preset,
                "pods": [{"ref": f"mt-{ns}-{r['pod']}", **r} for r in b.usage(ns)[:MAX_ITEMS]]}
    try:
        m = b.metric(preset, ns)
    except Unavailable as e:
        return {"namespace": ns, "preset": preset, "unavailable": str(e)}
    return {"namespace": ns, "preset": preset, "ref": f"mt-{ns}-{preset}", **m}


def s3_bucket_stats(b: Backend, bucket: str) -> dict:
    try:
        return {"ref": f"cs-s3-{bucket}", **b.s3_bucket_stats(check_name(bucket, "bucket"))}
    except Unavailable as e:
        return {"bucket": bucket, "unavailable": str(e)}


def cost_report(b: Backend, query: str) -> dict:
    try:
        return {"ref": f"cs-{query}", **b.cost(query)}
    except Unavailable as e:
        return {"query": query, "unavailable": str(e)}


def submit_diagnosis(b: Backend, **diagnosis) -> dict:          # the agent loop validates before accepting
    return {"accepted": True, "findings": len(diagnosis.get("findings", []))}


TOOLS: dict[str, Callable] = {f.__name__: f for f in (list_problem_pods, get_events, describe, pod_logs, list_resources,
                                                       resource_usage, s3_bucket_stats, cost_report, submit_diagnosis)}


def call(b: Backend, name: str, args: dict):
    """Dispatch one model tool call. Bad names, arguments or lookups return {"error": …}; never raises."""
    fn = TOOLS.get(name)
    if fn is None:
        return {"error": f"unknown tool {name}"}
    try:
        return fn(b, **args)
    except TypeError as e:
        return {"error": f"bad arguments for {name}: {e}"}
    except (ValueError, LookupError, PermissionError) as e:
        return {"error": _t(e)}


# ---- everything a diagnosis may cite, recomputed from the backend -------------------------

def all_refs(b: Backend, namespace: str) -> set[str]:
    """Every ref any tool could return for this namespace — the grounding registry."""
    refs: set[str] = set()
    ns = namespace
    pods = b.objects("pods", ns)
    for p in pods:
        name = p["metadata"]["name"]
        refs |= {f"st-{name}", f"ds-pod-{name}", f"rs-pod-{name}", f"mt-{ns}-{name}"}
        container = p["spec"]["containers"][0]["name"]
        for prev, tag in ((False, "c"), (True, "p")):
            try:
                n = len(b.logs(ns, name, container, prev).splitlines())
            except LookupError:
                continue
            refs |= {f"lg-{name}-{tag}{i}" for i in range(n)}
    for kind, plural in (("deployment", "deployments"), ("replicaset", "replicasets"), ("service", "services"), ("job", "jobs")):
        for o in b.objects(plural, ns):
            refs.add(f"ds-{kind}-{o['metadata']['name']}")
            if plural in LIST_KINDS:
                refs.add(f"rs-{kind}-{o['metadata']['name']}")
    for o in b.objects("configmaps", ns):
        refs.add(f"rs-configmap-{o['metadata']['name']}")
    for e in b.objects("events", ns):
        refs.add(event_ref(ns, e))
    for node in b.objects("nodes", ""):
        refs.add(f"ds-node-{node['metadata']['name']}")
    for key in getattr(b, "dump", {}).get("metrics", {}):
        preset, mns = key.split("/", 1)
        if mns == ns:
            refs.add(f"mt-{ns}-{preset}")
    for key in getattr(b, "dump", {}).get("aws", {}):
        kind, rest = key.split("/", 1)
        refs.add(f"cs-s3-{rest}" if kind == "s3" else f"cs-{rest}")
    return refs


def objects_in(b: Backend, namespace: str) -> set[tuple[str, str]]:
    """(Kind, name) pairs that exist — findings may only name these."""
    out = set()
    for kind, plural in (("Pod", "pods"), ("Deployment", "deployments"), ("ReplicaSet", "replicasets"),
                         ("Service", "services"), ("Job", "jobs"), ("ConfigMap", "configmaps")):
        out |= {(kind, o["metadata"]["name"]) for o in b.objects(plural, namespace)}
    out |= {("Node", n["metadata"]["name"]) for n in b.objects("nodes", "")}
    return out
