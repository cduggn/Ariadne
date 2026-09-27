#!/usr/bin/env python3
"""Build evals/golden/tasks.jsonl and reference diagnoses from the fault catalogue and its snapshots.

    python3 -m evals.build_golden

Tasks:
  investigate  one per recorded scenario — one namespace, the user's report, interactive priority
  audit        a few namespaces at once — nobody waiting, batch priority (the gateway's batch tenant)

The answer key comes from faults/*/scenario.json (what was injected), never from a model. A reference
solver builds one grounded diagnosis per task using only tool results; every reference must pass the
checker, so the key and the tools are proven consistent before any model is scored.
"""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from doctor import tools as T  # noqa: E402
from doctor.backends import SnapshotBackend  # noqa: E402
from evals.checker import check  # noqa: E402

GOLDEN = ROOT / "evals" / "golden"
AUDITS = [("audit-1", ["orders", "status", "checkout"]), ("audit-2", ["reports", "ml", "web", "billing"])]
ERROR_LINE = re.compile(r"(?i)\b(fatal|error|panic|exception|denied|refused)\b")
ROOT_CAUSE = {
    "crashloop_app_error": "the container exits at start-up with an application error (see log)",
    "oom_killed": "the container is killed for exceeding its memory limit",
    "image_pull": "the image or tag cannot be pulled",
    "unschedulable_resources": "no node has enough of the requested resources",
    "unschedulable_constraints": "the pod's node selector matches no node",
    "probe_failure": "the readiness probe fails, so the pod never becomes ready",
    "service_no_endpoints": "the service selector matches no pods, so it has no endpoints",
    "config_missing": "a referenced ConfigMap does not exist",
    "job_failed": "the job's pods fail and it exhausted its retries",
    "rollout_stuck": "the new ReplicaSet cannot become ready, so the rollout stalled on the old version",
}


def backend_for(ids: list[str]) -> SnapshotBackend:
    return SnapshotBackend.load(ROOT / "fixtures" / "cluster.json", *[ROOT / "fixtures" / "snapshots" / f"{i}.json" for i in ids])


def evidence_for(b, ns: str, t: dict) -> list[str]:
    """Pick refs of the expected types the tools really return for this object."""
    refs: list[str] = []
    pods = [r for r in T.list_problem_pods(b, ns)["problem_pods"] if (r["owner"]["kind"], r["owner"]["name"]) == (t["kind"], t["name"])]
    for et in t["evidence_types"]:
        if et == "status" and pods:
            refs.append(pods[0]["ref"])
        elif et == "event":
            evs = [e for e in T.get_events(b, ns, t["name"], 20)["events"] if e["type"] == "Warning"]
            refs += [e["ref"] for e in evs[:1]]
        elif et == "log":
            cands = pods or [{"pod": p["metadata"]["name"]} for p in b.objects("pods", ns) if p["metadata"]["name"].startswith(t["name"])]
            for p in cands:
                lines = T.pod_logs(b, ns, p["pod"], False, 80)["lines"]
                hit = [ln for ln in lines if ERROR_LINE.search(ln["text"])] or lines[-1:]
                if hit:
                    refs.append(hit[0]["ref"])
                    break
        elif et == "describe":
            refs.append(f"ds-{t['kind'].lower()}-{t['name']}" if t["kind"] != "Pod" else f"ds-pod-{t['name']}")
        elif et == "resources":
            refs.append(f"rs-{t['kind'].lower()}-{t['name']}")
    return list(dict.fromkeys(refs))[:6]


def reference(task: dict, b) -> dict:
    if task["expect"]["status"] == "healthy":
        return {"status": "healthy", "findings": [], "summary": "No failing workloads, events or endpoints found."}
    findings = [{"category": t["categories"][0], "namespace": t["namespace"], "kind": t["kind"], "name": t["name"],
                 "root_cause": ROOT_CAUSE.get(t["categories"][0], "see evidence"), "evidence": evidence_for(b, t["namespace"], t),
                 "fix": "see root cause; change the object's spec or config", "confidence": "high"} for t in task["expect"]["findings"]]
    return {"status": "issue", "findings": findings, "summary": f"{len(findings)} problem(s) found."}


def truth(sc: dict) -> list[dict]:
    out = []
    for o in sc["objects"]:
        cat = o.get("category", sc["category"])
        out.append({"namespace": sc["namespace"], "kind": o["kind"], "name": o["name"],
                    "categories": [cat] + [c for c in sc["allowed"] if c != cat and "category" not in o],
                    "evidence_types": sc["evidence"]})
    return out


def main() -> int:
    scen = {s["id"]: s for s in (json.loads(p.read_text()) for p in sorted((ROOT / "faults").glob("*/scenario.json")))}
    recorded = {i: s for i, s in scen.items() if not s["live_only"] and (ROOT / "fixtures" / "snapshots" / f"{i}.json").exists()}
    tasks = []
    for i, s in recorded.items():
        tasks.append({"id": f"dx-{i}", "task_type": "investigate", "namespaces": [s["namespace"]], "report": s["report"],
                      "snapshots": [i], "max_steps": 16,
                      "expect": {"status": "healthy" if s["category"] == "healthy" else "issue", "findings": truth(s),
                                 "must_call": s["must_call"]}})
    by_ns = {s["namespace"]: i for i, s in recorded.items()}
    for aid, nss in AUDITS:
        ids = [by_ns[n] for n in nss if n in by_ns]
        found = [f for i in ids for f in truth(recorded[i])]
        tasks.append({"id": f"dx-{aid}", "task_type": "audit", "namespaces": [recorded[i]["namespace"] for i in ids],
                      "report": "Nightly audit: check these namespaces and report every problem, or confirm they are healthy.",
                      "snapshots": ids, "max_steps": 30,
                      "expect": {"status": "issue" if found else "healthy", "findings": found, "must_call": ["list_problem_pods"]}})

    refs = {}
    for t in tasks:
        b = backend_for(t["snapshots"])
        ref = reference(t, b)
        r = check(t, ref, t["expect"]["must_call"], b)
        if not r["pass"]:
            raise SystemExit(f"{t['id']}: reference diagnosis fails its own checker: {r['failed']}")
        refs[t["id"]] = ref
    GOLDEN.mkdir(parents=True, exist_ok=True)
    (GOLDEN / "tasks.jsonl").write_text("".join(json.dumps(t, ensure_ascii=False) + "\n" for t in tasks))
    (GOLDEN / "reference_diagnoses.json").write_text(json.dumps(refs, indent=1, ensure_ascii=False))
    live = sorted(i for i, s in scen.items() if s["live_only"])
    print(f"{len(tasks)} tasks ({sum(t['task_type'] == 'investigate' for t in tasks)} investigate, "
          f"{sum(t['task_type'] == 'audit' for t in tasks)} audit); references pass; live-only scenarios awaiting fixtures: {live}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
