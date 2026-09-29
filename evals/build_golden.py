#!/usr/bin/env python3
"""Build evals/golden/tasks.jsonl and reference diagnoses from the fault catalogue and its snapshots.

    python3 -m evals.build_golden

Tasks (D-28, D-31):
  investigate  one per recorded scenario — one namespace, the user's report, interactive priority
  rightsize    right-sizing scenarios — batch priority
  audit        several namespaces at once — nobody waiting, batch priority (the gateway's batch tenant)
Each task carries its tier (easy · multi_hop · red_herring · rightsizing) so results are reported per tier.

The answer key comes from faults/*/scenario.json (what was injected), never from a model. A reference
solver builds one grounded diagnosis per task using only tool results (plus solver-only hints naming
which ConfigMap or container holds the evidence), and records the tool calls that returned every ref it
cites: its trajectory (D-41). The trajectory is replayed through the same dispatch and observation ledger
the agent uses, within the task's step cap, and the reference must pass BOTH scores (v1 legacy, v2 with
the ledger), so key, tools and checker are proven consistent before any model is scored. The reference
is an answer-key consistency proof, not a demonstration that a model could find the path unaided.
"""
from __future__ import annotations

import json
import math
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from doctor import tools as T  # noqa: E402
from doctor.backends import SnapshotBackend  # noqa: E402
from doctor.validate import new_ledger, observe  # noqa: E402
from evals.checker import check, check_v2  # noqa: E402

GOLDEN = ROOT / "evals" / "golden"
AUDITS = [("audit-1", "easy", ["orders", "status", "checkout"]), ("audit-2", "easy", ["reports", "ml", "web", "billing"]),
          ("audit-3", "multi_hop", ["inventory", "pricing", "finance"])]
ERROR_LINE = re.compile(r"(?i)\b(fatal|error|panic|exception|denied|refused|expired|verify|bad address|unreachable|waiting for)\b")
ROOT_CAUSE = {
    "crashloop_app_error": "the container exits at start-up with an application error (see log)",
    "oom_killed": "the container is killed for exceeding its memory limit",
    "image_pull": "the image or tag cannot be pulled",
    "unschedulable_resources": "no node has enough of the requested resources",
    "unschedulable_constraints": "the pod's node selector matches no node",
    "probe_failure": "the readiness probe fails, so the pod never becomes ready",
    "service_no_endpoints": "the service selector matches no pods, so it has no endpoints",
    "service_misconfig": "the service targetPort does not match the port the container listens on",
    "config_missing": "a referenced ConfigMap does not exist",
    "dependency_missing": "an init container waits for a service name that does not exist",
    "dns_failure": "the pod's DNS config points at a nameserver that does not answer",
    "tls_trust": "the client trusts the wrong CA bundle, so upstream certificate verification fails",
    "tls_expired": "the upstream serving certificate has expired; callers fail verification",
    "cpu_throttling": "a tiny CPU limit makes the liveness probe time out, so kubelet restarts a healthy app",
    "ephemeral_storage": "a sidecar fills ephemeral storage past its limit and kubelet evicts the pod",
    "resource_policy": "a namespace policy (LimitRange or ResourceQuota) constrains the workload",
    "job_failed": "the job's pods fail and it exhausted its retries",
    "rollout_stuck": "the new ReplicaSet cannot become ready, so the rollout stalled on the old version",
    "overprovisioned": "requests are far above observed usage",
}


def backend_for(ids: list[str]) -> SnapshotBackend:
    return SnapshotBackend.load(ROOT / "fixtures" / "cluster.json", *[ROOT / "fixtures" / "snapshots" / f"{i}.json" for i in ids])


def _pods_of(b, ns: str, kind: str, name: str) -> list[dict]:
    rss = b.objects("replicasets", ns)
    return [p for p in b.objects("pods", ns) if T.owner_of(p, rss) == {"kind": kind, "name": name}]


PLURAL = {v: k for k, v in T.LIST_KINDS.items()}         # "service" → "services"


def evidence_for(b, ns: str, t: dict, hints: dict) -> tuple[list[str], list[tuple[str, dict]]]:
    """Pick refs of the expected types that the tools really return, and the calls that returned them."""
    refs: list[str] = []
    calls: list[tuple[str, dict]] = []

    def use(name: str, args: dict, ref: str) -> None:
        refs.append(ref)
        calls.append((name, args))

    objs = [(t["kind"], t["name"])] + [(a["kind"], a["name"]) for a in t.get("affects", [])]
    for et in t["evidence_types"]:
        if et == "status":
            for kind, name in objs:
                args = {"namespace": ns}
                rows = [r for r in T.call(b, "list_problem_pods", args)["problem_pods"] if (r["owner"]["kind"], r["owner"]["name"]) == (kind, name)]
                if rows:
                    use("list_problem_pods", args, rows[0]["ref"])
                    break
        elif et == "event":
            for name in hints.get("event_objects", []) + [n for _, n in objs]:
                args = {"namespace": ns, "object_name": name, "limit": 20}
                evs = [e for e in T.call(b, "get_events", args)["events"] if e["type"] == "Warning"]
                if evs:
                    use("get_events", args, evs[0]["ref"])
                    break
        elif et == "log":
            names = hints.get("log_objects", []) + [n for _, n in objs]
            for name in names:
                pods = [p for k, n in objs + [("Deployment", x) for x in names] if n == name for p in _pods_of(b, ns, k, n)]
                for p in pods:
                    args = {"namespace": ns, "pod": p["metadata"]["name"], "container": hints.get("log_container", ""),
                            "previous": False, "tail": 80}
                    lines = T.call(b, "pod_logs", args).get("lines", [])
                    hit = [ln for ln in lines if ERROR_LINE.search(ln["text"])]
                    if hit:
                        use("pod_logs", args, hit[-1]["ref"])
                        break
                if refs and refs[-1].startswith("lg-"):
                    break
        elif et == "describe":
            for h in hints.get("describe") or [{"kind": t["kind"].lower(), "name": t["name"]}]:
                args = {"kind": h["kind"], "namespace": ns, "name": h["name"]}
                use("describe", args, T.call(b, "describe", args)["ref"])
        elif et == "resources":
            for h in hints.get("resources") or [{"kind": PLURAL[t["kind"].lower()], "name": t["name"]}]:
                args = {"kind": h["kind"], "namespace": ns}
                use("list_resources", args, next(i["ref"] for i in T.call(b, "list_resources", args)["items"] if i["name"] == h["name"]))
        elif et == "certificate":
            for h in hints.get("certificate", []):
                args = {"namespace": ns, "configmap": h["configmap"], "key": h["key"]}
                use("inspect_certificate", args, T.call(b, "inspect_certificate", args)["ref"])
        elif et == "rightsizing":
            args = {"namespace": ns}
            for w in [w for w in T.call(b, "rightsizing", args)["workloads"] if w["owner"] == {"kind": t["kind"], "name": t["name"]}][:1]:
                use("rightsizing", args, w["ref"])
    return list(dict.fromkeys(refs))[:8], calls


def resize_for(b, ns: str, t: dict) -> dict:
    w = next(w for w in T.rightsizing(b, ns)["workloads"] if w["owner"] == {"kind": t["kind"], "name": t["name"]})
    cpu = max(10, math.ceil(2 * (w["usage"]["cpu_m"]["p95"] or 0) / 5) * 5)
    mem = max(16, math.ceil(2 * (w["usage"]["memory_mi"]["p95"] or 0)))
    return {"cpu_request": f"{cpu}m", "memory_request": f"{mem}Mi"}


def reference(task: dict, b, hints: dict) -> tuple[dict, list[list]]:
    """(reference diagnosis, trajectory): list_problem_pods for every namespace, then each call that returned a cited ref."""
    calls: list[tuple[str, dict]] = [("list_problem_pods", {"namespace": ns}) for ns in task["namespaces"]]
    if task["expect"]["status"] == "healthy":
        return {"status": "healthy", "findings": [], "summary": "No failing workloads, events or endpoints found."}, calls
    findings = []
    for t in task["expect"]["findings"]:
        cat = t["categories"][0]
        evidence, used = evidence_for(b, t["namespace"], t, hints.get(t["namespace"], {}))
        calls += used
        text = (t.get("mechanism") or {}).get("example") or {"root_cause": ROOT_CAUSE.get(cat, "see evidence"),
                                                              "fix": "see root cause; change this object's spec or config"}
        findings.append({"category": cat, "namespace": t["namespace"], "kind": t["kind"], "name": t["name"],
                         "root_cause": text["root_cause"],
                         "affects": [{"kind": a["kind"], "namespace": t["namespace"], "name": a["name"]} for a in t.get("affects", [])],
                         "evidence": evidence, "fix": text["fix"],
                         "resize": resize_for(b, t["namespace"], t) if cat == "overprovisioned" else {"cpu_request": "", "memory_request": ""},
                         "confidence": "high"})
    unique = list({json.dumps([n, a], sort_keys=True): [n, a] for n, a in calls}.values())
    return {"status": "issue", "findings": findings, "summary": f"{len(findings)} root cause(s) found."}, unique


def replay(b, trajectory: list[list]) -> tuple[dict, list[dict]]:
    """Run a trajectory through the tools and the observation ledger, as the agent would; (ledger, call records)."""
    ledger, records = new_ledger(), []
    for name, args in trajectory:
        result = T.call(b, name, args)
        ledger = observe(ledger, name, result)
        records.append({"name": name, "error": result.get("error") if isinstance(result, dict) else None})
    return ledger, records


def truth(sc: dict) -> dict:
    ns = sc["namespace"]
    roots = []
    for o in sc["objects"]:
        cat = o.get("category", sc["category"])
        roots.append({"namespace": ns, "kind": o["kind"], "name": o["name"],
                      "categories": [cat] + [c for c in sc["allowed"] if c != cat and "category" not in o],
                      "evidence_types": sc["evidence"], "affects": [dict(a, namespace=ns) for a in o.get("affects", [])],
                      **({"alternatives": o["alternatives"]} if o.get("alternatives") else {}),
                      **({"resize_band": o["resize_band"]} if o.get("resize_band") else {}),
                      **({"also_accept": o["also_accept"]["categories"]} if o.get("also_accept") else {}),
                      **({"mechanism": o["mechanism"]} if o.get("mechanism") else {})})
    ns_ = lambda xs: [dict(x, namespace=ns) for x in xs]  # noqa: E731
    return {"findings": roots, "red_herrings": ns_(sc.get("red_herrings", [])), "also_ok": ns_(sc.get("also_ok", [])),
            "forbidden": ns_(sc.get("forbidden", []))}


def main() -> int:
    scen = {s["id"]: s for s in (json.loads(p.read_text()) for p in sorted((ROOT / "faults").glob("*/scenario.json")))}
    recorded = {i: s for i, s in scen.items() if not s["live_only"] and (ROOT / "fixtures" / "snapshots" / f"{i}.json").exists()}
    tasks, hints = [], {s["namespace"]: s.get("hints", {}) for s in recorded.values()}
    for i, s in recorded.items():
        tr = truth(s)
        tasks.append({"id": f"dx-{i}", "task_type": s.get("task_type", "investigate"), "tier": s["tier"],
                      "namespaces": [s["namespace"]], "report": s["report"], "snapshots": [i],
                      "max_steps": 12 if s.get("task_type") == "rightsize" else 16,
                      "expect": {"status": "healthy" if s["category"] == "healthy" else "issue", **tr, "must_call": s["must_call"]}})
    by_ns = {s["namespace"]: i for i, s in recorded.items()}
    for aid, tier, nss in AUDITS:
        ids = [by_ns[n] for n in nss if n in by_ns]
        if not ids:
            continue
        parts = [truth(recorded[i]) for i in ids]
        merged = {k: [x for p in parts for x in p[k]] for k in ("findings", "red_herrings", "also_ok", "forbidden")}
        tasks.append({"id": f"dx-{aid}", "task_type": "audit", "tier": tier, "namespaces": [recorded[i]["namespace"] for i in ids],
                      "report": "Nightly audit: check these namespaces and report every root cause, or confirm they are healthy.",
                      "snapshots": ids, "max_steps": 30,
                      "expect": {"status": "issue" if merged["findings"] else "healthy", **merged, "must_call": ["list_problem_pods"]}})

    refs, paths = {}, {}
    for t in tasks:
        b = backend_for(t["snapshots"])
        ref, trajectory = reference(t, b, hints)
        r = check(t, ref, t["expect"]["must_call"], b)
        if not r["pass"]:
            raise SystemExit(f"{t['id']}: reference diagnosis fails its own checker (v1): {r['failed']}")
        if len(trajectory) + 1 > t["max_steps"]:
            raise SystemExit(f"{t['id']}: reference trajectory needs {len(trajectory) + 1} model calls > max_steps {t['max_steps']}")
        ledger, records = replay(b, trajectory)
        r2 = check_v2(t, ref, b, observed=ledger, calls=records + [{"name": "submit_diagnosis"}])
        if not r2["pass"]:
            raise SystemExit(f"{t['id']}: reference fails the v2 checker when replayed through its trajectory: {r2['failed']}")
        refs[t["id"]], paths[t["id"]] = ref, trajectory
    GOLDEN.mkdir(parents=True, exist_ok=True)
    (GOLDEN / "tasks.jsonl").write_text("".join(json.dumps(t, ensure_ascii=False) + "\n" for t in tasks))
    (GOLDEN / "reference_diagnoses.json").write_text(json.dumps(refs, indent=1, ensure_ascii=False))
    (GOLDEN / "reference_trajectories.json").write_text(json.dumps(paths, indent=1, ensure_ascii=False))
    from collections import Counter
    live = sorted(i for i, s in scen.items() if s["live_only"])
    missing = sorted(i for i, s in scen.items() if not s["live_only"] and i not in recorded)
    print(f"{len(tasks)} tasks {dict(Counter(t['task_type'] for t in tasks))}, tiers {dict(Counter(t['tier'] for t in tasks))}; "
          f"references pass. Not recorded: {missing or 'none'}. Live-only awaiting fixtures: {live}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
