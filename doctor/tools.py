"""The doctor's tools: small, referenced, redacted views over a Backend (D-21). Standard library only.

Every item a tool returns carries a stable `ref`. A diagnosis must cite refs as evidence; the
validator and the checker recompute the full set of refs from the same backend, so a cited ref that
does not exist is a fabrication and is rejected.

    ref prefix   evidence type   example
    st-          status          st-orders-api-7c9f8-abcde                  (pod status)
    ev-          event           ev-3f2a91                                  (hash of ns/object/reason/message)
    ds-          describe        ds-deployment-orders-api · ds-limitrange-defaults
    lg-          log             lg-orders-api-7c9f8-abcde-orders-api-c12   (pod, container, current/previous, line)
    rs-          resources       rs-service-checkout · rs-resourcequota-team-quota
    mt-          metrics         mt-reports-report-worker-5d8f-xyz-report-worker
    ct-          certificate     ct-internal-ca-ca.crt                      (public cert in a ConfigMap)
    rz-          rightsizing     rz-analytics-reporting-api-reporting-api   (workload, container)
    cs-          cost            cs-aws_by_service_7d

Guards: names are validated (RFC 1123); tools only read; results are size-capped; text is redacted;
log lines that look like instructions to an AI are flagged `suspicious`, never obeyed.
"""
from __future__ import annotations

import datetime as dt
import hashlib
import json
from collections.abc import Callable

from .backends import Backend, Unavailable, check_name
from .quantity import cpu_m, mem_mi, pct
from .redact import looks_like_injection, redact
from .x509 import parse_pem

MAX_ITEMS = 20
MAX_TEXT = 240
MAX_LOG_TAIL = 80
DESCRIBE_KINDS = {"pod": "pods", "deployment": "deployments", "replicaset": "replicasets", "service": "services",
                  "job": "jobs", "node": "nodes", "configmap": "configmaps", "limitrange": "limitranges",
                  "resourcequota": "resourcequotas", "networkpolicy": "networkpolicies",
                  "persistentvolumeclaim": "persistentvolumeclaims", "ingress": "ingresses"}
LIST_KINDS = {"deployments": "deployment", "services": "service", "jobs": "job", "pods": "pod", "configmaps": "configmap",
              "limitranges": "limitrange", "resourcequotas": "resourcequota", "networkpolicies": "networkpolicy",
              "persistentvolumeclaims": "persistentvolumeclaim", "ingresses": "ingress"}
EVIDENCE_TYPES = {"st": "status", "ev": "event", "ds": "describe", "lg": "log", "rs": "resources", "mt": "metrics",
                  "ct": "certificate", "rz": "rightsizing", "cs": "cost"}
# Price list for the idle-request estimate (OpenCost's on-prem defaults, $/hour) — an estimate, labelled as such.
CPU_CORE_HOUR_USD, RAM_GIB_HOUR_USD, HOURS_PER_MONTH = 0.031611, 0.004237, 730


def _t(s: object, n: int = MAX_TEXT) -> str:
    s = redact(str(s or ""))
    return s if len(s) <= n else s[: n - 1] + "…"


def _h(*parts: str) -> str:
    return hashlib.sha1("|".join(parts).encode(), usedforsecurity=False).hexdigest()[:6]   # an id, not a security hash


def evidence_type(ref: str) -> str | None:
    return EVIDENCE_TYPES.get(ref.split("-", 1)[0])


def _conditions(st: dict) -> list[dict]:
    return [{"type": c["type"], "status": c["status"], "reason": c.get("reason"), "message": _t(c.get("message"), 160)}
            for c in st.get("conditions", [])]


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
    name, st = p["metadata"]["name"], p.get("status", {})
    statuses = st.get("containerStatuses") or []
    ready = sum(1 for c in statuses if c.get("ready"))
    restarts = sum(c.get("restartCount", 0) for c in statuses)
    reason, detail = "", ""
    if st.get("reason"):                                   # pod-level, e.g. Evicted
        reason, detail = st["reason"], st.get("message", "")
    for ic in st.get("initContainerStatuses") or []:
        s = ic.get("state", {})
        if not reason and not ("terminated" in s and s["terminated"].get("exitCode") == 0):
            key = next(iter(s), "waiting")
            reason = f"Init:{s.get(key, {}).get('reason') or key.capitalize()}"
            detail = f"init container {ic['name']} has not completed (restarts {ic.get('restartCount', 0)})"
    for cs in statuses:
        if reason:
            break
        r, d = _container_problem(cs)
        if r:
            reason, detail = r, d
    if not reason and not statuses:
        cond = next((c for c in st.get("conditions", []) if c.get("status") == "False"), {})
        reason, detail = cond.get("reason", st.get("phase", "")), cond.get("message", "")
    return {"ref": f"st-{name}", "pod": name, "phase": st.get("phase"),
            "ready": f"{ready}/{len(p.get('spec', {}).get('containers', []))}", "restarts": restarts,
            "reason": _t(reason, 60), "detail": _t(detail, 200), "owner": owner_of(p, replicasets)}


def _is_problem(row: dict, phase: str) -> bool:
    if phase == "Succeeded":
        return False
    r, n = row["ready"].split("/")
    return phase != "Running" or row["restarts"] > 0 or r != n or bool(row["reason"])


# ---- investigation tools ---------------------------------------------------------------------

def list_problem_pods(b: Backend, namespace: str) -> dict:
    ns = check_name(namespace, "namespace")
    rss = b.objects("replicasets", ns)
    pods = b.objects("pods", ns)
    rows = [_pod_row(p, rss) for p in pods]
    probs = [r for r, p in zip(rows, pods, strict=True) if _is_problem(r, p.get("status", {}).get("phase", ""))]
    return {"namespace": ns, "pods_total": len(pods), "problem_pods": probs[:MAX_ITEMS]}


def event_ref(ns: str, e: dict) -> str:
    io = e.get("involvedObject", {})
    return "ev-" + _h(ns, io.get("kind", ""), io.get("name", ""), e.get("reason", ""), e.get("message", ""))


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


def _find(b: Backend, kind: str, ns: str, name: str) -> dict:
    for o in b.objects(DESCRIBE_KINDS[kind], ns):
        if o["metadata"]["name"] == name:
            return o
    raise LookupError(f"{kind} {name} not found in namespace {ns or '(cluster)'}")


def _label_match(selector: dict, labels: dict) -> bool:
    return bool(selector) and all(labels.get(k) == v for k, v in selector.items())


def _probe(p: dict | None) -> dict | None:
    if not p:
        return None
    out = {k: p.get(k) for k in ("timeoutSeconds", "periodSeconds", "failureThreshold", "initialDelaySeconds") if k in p}
    for k in ("httpGet", "tcpSocket", "grpc"):
        if k in p:
            out[k] = p[k]
    if "exec" in p:
        out["exec"] = _t(" ".join(p["exec"].get("command", [])), 160)
    return out


def _container(c: dict, cs: dict | None) -> dict:
    cs = cs or {}
    return {"name": c["name"], "image": c.get("image"),
            "command": _t(" ".join(c.get("command", []) + c.get("args", [])), 200) or None,
            "ports": [{"containerPort": p.get("containerPort"), "name": p.get("name")} for p in c.get("ports", [])],
            "resources": c.get("resources", {}),
            "env_names": [e["name"] for e in c.get("env", [])][:15],        # names only — values may be secret
            "env_from": [{"configMap": x["configMapRef"].get("name")} if "configMapRef" in x else
                         {"secret": x.get("secretRef", {}).get("name")} for x in c.get("envFrom", [])],
            "volume_mounts": [{"name": m["name"], "mountPath": m["mountPath"]} for m in c.get("volumeMounts", [])],
            "readiness_probe": _probe(c.get("readinessProbe")), "liveness_probe": _probe(c.get("livenessProbe")),
            "state": _t(json.dumps(cs.get("state", {})), 200), "last_state": _t(json.dumps(cs.get("lastState", {})), 200),
            "restarts": cs.get("restartCount", 0), "ready": cs.get("ready", False)}


def _volume(v: dict) -> dict:
    for src in ("configMap", "secret", "emptyDir", "persistentVolumeClaim", "projected", "hostPath"):
        if src in v:
            s = v[src] or {}
            ref = s.get("name") or s.get("secretName") or s.get("claimName") or s.get("path")
            return {"name": v["name"], "source": src, **({"ref": ref} if ref else {}),
                    **({"sizeLimit": s["sizeLimit"]} if "sizeLimit" in s else {})}
    return {"name": v["name"], "source": "other"}


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
        ics = {c["name"]: c for c in st.get("initContainerStatuses") or []}
        out["init_containers"] = [_container(c, ics.get(c["name"])) for c in spec.get("initContainers", [])]
        out["containers"] = [_container(c, cs.get(c["name"])) for c in spec.get("containers", [])]
        out["volumes"] = [_volume(v) for v in spec.get("volumes", []) if not v["name"].startswith("kube-api-access")]
        out.update({"node": spec.get("nodeName"), "node_selector": spec.get("nodeSelector"), "scheduler": spec.get("schedulerName"),
                    "dns_policy": spec.get("dnsPolicy"), "dns_config": spec.get("dnsConfig"),
                    "status_reason": st.get("reason"), "status_message": _t(st.get("message"), 200),
                    "conditions": _conditions(st), "owner": owner_of(o, b.objects("replicasets", ns))})
    elif kind in ("deployment", "replicaset"):
        pod = spec.get("template", {}).get("spec", {})
        out.update({"replicas": {"desired": spec.get("replicas"), "current": st.get("replicas", 0), "updated": st.get("updatedReplicas", 0),
                                 "ready": st.get("readyReplicas", 0), "available": st.get("availableReplicas", 0),
                                 "unavailable": st.get("unavailableReplicas", 0)},
                    "containers": [_container(c, None) | {"state": None, "last_state": None} for c in pod.get("containers", [])],
                    "init_containers": [c["name"] for c in pod.get("initContainers", [])],
                    "volumes": [_volume(v) for v in pod.get("volumes", [])],
                    "selector": spec.get("selector", {}).get("matchLabels"), "strategy": spec.get("strategy"),
                    "progress_deadline_s": spec.get("progressDeadlineSeconds"), "conditions": _conditions(st)})
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
                    "conditions": _conditions(st)})
    elif kind == "node":
        out.update({"allocatable": st.get("allocatable"), "labels": o["metadata"].get("labels"), "taints": spec.get("taints"),
                    "conditions": [{"type": c["type"], "status": c["status"], "reason": c.get("reason")} for c in st.get("conditions", [])]})
    elif kind == "configmap":
        out.update({"keys": o.get("dataKeys", []), "certificate_keys": sorted(o.get("publicCertificates", {}))})
    elif kind == "limitrange":
        out["limits"] = spec.get("limits", [])
    elif kind == "resourcequota":
        out.update({"hard": st.get("hard") or spec.get("hard"), "used": st.get("used", {})})
    elif kind == "networkpolicy":
        out.update({"pod_selector": spec.get("podSelector"), "policy_types": spec.get("policyTypes"),
                    "ingress": _t(json.dumps(spec.get("ingress", [])), 300), "egress": _t(json.dumps(spec.get("egress", [])), 300)})
    elif kind == "persistentvolumeclaim":
        out.update({"phase": st.get("phase"), "storage_class": spec.get("storageClassName"), "access_modes": spec.get("accessModes"),
                    "requested": spec.get("resources", {}).get("requests")})
    elif kind == "ingress":
        out.update({"class": spec.get("ingressClassName"), "rules": _t(json.dumps(spec.get("rules", [])), 300),
                    "tls_hosts": [h for t in spec.get("tls", []) for h in t.get("hosts", [])],
                    "load_balancer": st.get("loadBalancer", {})})
    return out


def pod_logs(b: Backend, namespace: str, pod: str, container: str, previous: bool, tail: int) -> dict:
    ns, pod = check_name(namespace, "namespace"), check_name(pod, "pod")
    p = next((x for x in b.objects("pods", ns) if x["metadata"]["name"] == pod), None)
    if p is None:
        raise LookupError(f"pod {pod} not found in namespace {ns}")
    names = [c["name"] for c in p["spec"].get("initContainers", []) + p["spec"]["containers"]]
    container = container or p["spec"]["containers"][0]["name"]
    if container not in names:
        raise LookupError(f"pod {pod} has no container {container}; containers: {names}")
    lines = b.logs(ns, pod, container, bool(previous)).splitlines()
    n = max(1, min(int(tail), MAX_LOG_TAIL))
    start = max(0, len(lines) - n)
    tag = "p" if previous else "c"
    out = [{"ref": f"lg-{pod}-{container}-{tag}{i}", "text": _t(line, 300), **({"suspicious": True} if looks_like_injection(line) else {})}
           for i, line in enumerate(lines[start:], start)]
    return {"namespace": ns, "pod": pod, "container": container, "containers": names, "previous": bool(previous),
            "lines": out, "total_lines": len(lines)}


def list_resources(b: Backend, kind: str, namespace: str) -> dict:
    if kind not in LIST_KINDS:
        raise ValueError(f"kind must be one of {sorted(LIST_KINDS)}")
    ns = check_name(namespace, "namespace")
    rows = []
    for o in b.objects(kind, ns)[:MAX_ITEMS]:
        n, st, sp = o["metadata"]["name"], o.get("status", {}), o.get("spec", {})
        row: dict = {"ref": f"rs-{LIST_KINDS[kind]}-{n}", "name": n}
        if kind == "deployments":
            row["ready"] = f"{st.get('readyReplicas', 0)}/{sp.get('replicas')}"
        elif kind == "services":
            row["selector"], row["ports"] = sp.get("selector"), [f"{p.get('port')}->{p.get('targetPort')}" for p in sp.get("ports", [])]
        elif kind == "jobs":
            row.update({"succeeded": st.get("succeeded", 0), "failed": st.get("failed", 0)})
        elif kind == "pods":
            row.update({"phase": st.get("phase"), "labels": o["metadata"].get("labels")})
        elif kind == "configmaps":
            row.update({"keys": o.get("dataKeys", []), "has_certificate": bool(o.get("publicCertificates"))})
        elif kind == "limitranges":
            row["types"] = [x.get("type") for x in sp.get("limits", [])]
        elif kind == "resourcequotas":
            row.update({"hard": st.get("hard") or sp.get("hard"), "used": st.get("used", {})})
        elif kind == "networkpolicies":
            row.update({"pod_selector": sp.get("podSelector"), "policy_types": sp.get("policyTypes")})
        elif kind == "persistentvolumeclaims":
            row.update({"phase": st.get("phase"), "storage_class": sp.get("storageClassName")})
        elif kind == "ingresses":
            row["hosts"] = [r.get("host") for r in sp.get("rules", [])]
        rows.append(row)
    return {"namespace": ns, "kind": kind, "items": rows}


def resource_usage(b: Backend, namespace: str, preset: str) -> dict:
    ns = check_name(namespace, "namespace")
    if preset == "pods":
        return {"namespace": ns, "preset": preset,
                "containers": [{"ref": f"mt-{ns}-{r['pod']}-{r.get('container', '')}".rstrip("-"), **r} for r in b.usage(ns)[:MAX_ITEMS]]}
    try:
        m = b.metric(preset, ns)
    except Unavailable as e:
        return {"namespace": ns, "preset": preset, "unavailable": str(e)}
    return {"namespace": ns, "preset": preset, "ref": f"mt-{ns}-{preset}", **m}


def inspect_certificate(b: Backend, namespace: str, configmap: str, key: str) -> dict:
    """Subject, issuer, validity and names of a PUBLIC certificate stored in a ConfigMap (never a Secret)."""
    ns = check_name(namespace, "namespace")
    cm = next((o for o in b.objects("configmaps", ns) if o["metadata"]["name"] == check_name(configmap, "configmap")), None)
    if cm is None:
        raise LookupError(f"configmap {configmap} not found in namespace {ns}")
    pem = (cm.get("publicCertificates") or {}).get(key)
    if pem is None:
        raise LookupError(f"configmap {configmap} has no public certificate under key {key!r}; certificate keys: "
                          f"{sorted(cm.get('publicCertificates') or {})}")
    now = b.now()
    certs = []
    for c in parse_pem(pem):
        na = dt.datetime.strptime(c["not_after"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.UTC)
        certs.append({**c, "expired": na < now, "days_left": round((na - now).total_seconds() / 86400, 1)})
    return {"ref": f"ct-{configmap}-{key}", "namespace": ns, "configmap": configmap, "key": key,
            "checked_at": now.strftime("%Y-%m-%dT%H:%M:%SZ"), "certificates": certs}


def _stats(xs: list[float]) -> dict:
    return {"p50": _r(pct(xs, 0.5)), "p95": _r(pct(xs, 0.95)), "max": _r(max(xs) if xs else None)}


def _r(x):
    return None if x is None else round(x, 1)


def rightsizing(b: Backend, namespace: str) -> dict:
    """Requests and limits against observed usage, per workload container, with an idle-request cost estimate."""
    ns = check_name(namespace, "namespace")
    rss = b.objects("replicasets", ns)
    series = b.usage_series(ns)
    groups: dict[tuple[str, str, str], dict] = {}
    for p in b.objects("pods", ns):
        if p.get("status", {}).get("phase") != "Running":
            continue
        own = owner_of(p, rss)
        for c in p["spec"]["containers"]:
            g = groups.setdefault((own["kind"], own["name"], c["name"]), {"pods": set(), "spec": c})
            g["pods"].add(p["metadata"]["name"])
    out = []
    for (okind, oname, cname), g in sorted(groups.items()):
        cpu, mem = [], []
        for sample in series:
            for row in sample.get("rows", []):
                if row.get("pod") in g["pods"] and row.get("container", cname) == cname:
                    cpu.append(cpu_m(row.get("cpu")))
                    mem.append(mem_mi(row.get("memory")))
        res = g["spec"].get("resources", {})
        req, lim = res.get("requests", {}), res.get("limits", {})
        rq = {"cpu_m": cpu_m(req.get("cpu")), "memory_mi": mem_mi(req.get("memory"))}
        lm = {"cpu_m": cpu_m(lim.get("cpu")), "memory_mi": mem_mi(lim.get("memory"))}
        u = {"cpu_m": _stats(cpu), "memory_mi": _stats(mem)}
        at_limit = (sum(1 for x in cpu if lm["cpu_m"] and x >= 0.9 * lm["cpu_m"]) / len(cpu)) if cpu and lm["cpu_m"] else 0.0
        idle_cpu = max(0.0, (rq["cpu_m"] or 0) - (u["cpu_m"]["p95"] or 0))
        idle_mem = max(0.0, (rq["memory_mi"] or 0) - (u["memory_mi"]["p95"] or 0))
        replicas = len(g["pods"])
        monthly = (idle_cpu / 1000 * CPU_CORE_HOUR_USD + idle_mem / 1024 * RAM_GIB_HOUR_USD) * HOURS_PER_MONTH * replicas
        out.append({"ref": f"rz-{ns}-{oname}-{cname}", "owner": {"kind": okind, "name": oname}, "container": cname,
                    "replicas": replicas, "requests": rq, "limits": lm, "usage": u, "samples": len(cpu),
                    "cpu_at_limit_share": round(at_limit, 2), "idle_request": {"cpu_m": _r(idle_cpu), "memory_mi": _r(idle_mem)},
                    "est_monthly_idle_usd": round(monthly, 2)})
    window = f"{series[0].get('t')} → {series[-1].get('t')}" if series else None
    return {"namespace": ns, "window": window, "samples_per_container_note": "few samples → low confidence",
            "price_basis": "estimate: $0.031611 per core-hour, $0.004237 per GiB-hour", "workloads": out[:MAX_ITEMS]}


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
                                                       resource_usage, inspect_certificate, rightsizing, s3_bucket_stats,
                                                       cost_report, submit_diagnosis)}


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
    for p in b.objects("pods", ns):
        name = p["metadata"]["name"]
        refs |= {f"st-{name}", f"ds-pod-{name}", f"rs-pod-{name}", f"mt-{ns}-{name}"}
        for c in p["spec"].get("initContainers", []) + p["spec"]["containers"]:
            refs.add(f"mt-{ns}-{name}-{c['name']}")
            for prev, tag in ((False, "c"), (True, "p")):
                try:
                    n = len(b.logs(ns, name, c["name"], prev).splitlines())
                except LookupError:
                    continue
                refs |= {f"lg-{name}-{c['name']}-{tag}{i}" for i in range(n)}
    for plural, singular in LIST_KINDS.items():
        if plural == "pods":
            continue
        for o in b.objects(plural, ns):
            refs |= {f"rs-{singular}-{o['metadata']['name']}", f"ds-{singular}-{o['metadata']['name']}"}
            for key in (o.get("publicCertificates") or {}):
                refs.add(f"ct-{o['metadata']['name']}-{key}")
    for o in b.objects("replicasets", ns):
        refs.add(f"ds-replicaset-{o['metadata']['name']}")
    for e in b.objects("events", ns):
        refs.add(event_ref(ns, e))
    for node in b.objects("nodes", ""):
        refs.add(f"ds-node-{node['metadata']['name']}")
    refs |= {w["ref"] for w in rightsizing(b, ns)["workloads"]}
    for key in getattr(b, "dump", {}).get("metrics", {}):
        preset, mns = key.split("/", 1)
        if mns == ns:
            refs.add(f"mt-{ns}-{preset}")
    for key in getattr(b, "dump", {}).get("aws", {}):
        kind, rest = key.split("/", 1)
        refs.add(f"cs-s3-{rest}" if kind == "s3" else f"cs-{rest}")
    return refs


OBJECT_KINDS = {"Pod": "pods", "Deployment": "deployments", "ReplicaSet": "replicasets", "Service": "services", "Job": "jobs",
                "ConfigMap": "configmaps", "LimitRange": "limitranges", "ResourceQuota": "resourcequotas",
                "NetworkPolicy": "networkpolicies", "PersistentVolumeClaim": "persistentvolumeclaims", "Ingress": "ingresses"}


def objects_in(b: Backend, namespace: str) -> set[tuple[str, str]]:
    """(Kind, name) pairs that exist — findings may only name these."""
    out = set()
    for kind, plural in OBJECT_KINDS.items():
        out |= {(kind, o["metadata"]["name"]) for o in b.objects(plural, namespace)}
    out |= {("Node", n["metadata"]["name"]) for n in b.objects("nodes", "")}
    return out
