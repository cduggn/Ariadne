#!/usr/bin/env python3
"""Inject every lab fault into the kind cluster, wait until each has settled, and record snapshots.

    python3 -m lab.record                      # all kind scenarios, context kind-doctor-lab
    python3 -m lab.record --only oom,crashloop
    python3 -m lab.record --context kind-doctor-lab --keep   # leave faults running afterwards

Writes fixtures/cluster.json (the shared cluster card data) and fixtures/snapshots/<id>.json (one
namespace each). All scenarios are applied into ONE cluster before recording, so every snapshot
shares the same cluster card — the same shared prefix a live cluster would give.

Every string in the dump is redacted (doctor/redact.py) before it is written: fixtures live in git.
Only this script mutates a cluster (kubectl apply/delete, lab only); the doctor never does.
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from doctor.backends import KINDS, KubectlBackend  # noqa: E402
from doctor.redact import redact  # noqa: E402

FAULTS = ROOT / "faults"
OUT = ROOT / "fixtures"
KUBECTL = str(ROOT / ".bin" / "kubectl")


def kubectl(context: str, *args: str) -> str:
    r = subprocess.run([KUBECTL, "--context", context, *args], capture_output=True, text=True, timeout=120, check=False)
    if r.returncode != 0:
        raise RuntimeError(f"kubectl {' '.join(args)}: {r.stderr.strip()}")
    return r.stdout


def scrub(x):
    if isinstance(x, str):
        return redact(x)
    if isinstance(x, list):
        return [scrub(v) for v in x]
    if isinstance(x, dict):
        return {k: scrub(v) for k, v in x.items()}
    return x


def settled(b: KubectlBackend, sc: dict) -> bool:
    ns, rule = sc["namespace"], sc["settle"]
    pods = b.objects("pods", ns)
    cstats = [c for p in pods for c in (p.get("status", {}).get("containerStatuses") or [])]
    if rule == "restarts>=2":
        return any(c.get("restartCount", 0) >= 2 for c in cstats)
    if rule == "oom":
        return any(c.get("restartCount", 0) >= 2 and c.get("lastState", {}).get("terminated", {}).get("reason") == "OOMKilled" for c in cstats)
    if rule.startswith("waiting:"):
        want = rule.split(":", 1)[1]
        return any(c.get("state", {}).get("waiting", {}).get("reason") == want for c in cstats)
    if rule.startswith("event:"):
        want = rule.split(":", 1)[1]
        return any(e.get("reason") == want for e in b.objects("events", ns))
    if rule == "ready_all":
        return bool(cstats) and all(c.get("ready") for c in cstats) and all(p["status"].get("phase") == "Running" for p in pods)
    if rule == "job_failed":
        return any(c.get("type") == "Failed" and c.get("status") == "True"
                   for j in b.objects("jobs", ns) for c in j.get("status", {}).get("conditions", []))
    if rule == "deploy_stalled":
        return any(c.get("type") == "Progressing" and c.get("reason") == "ProgressDeadlineExceeded"
                   for d in b.objects("deployments", ns) for c in d.get("status", {}).get("conditions", []))
    raise ValueError(f"unknown settle rule {rule}")


def dump_namespace(b: KubectlBackend, ns: str) -> dict:
    out: dict = {"namespaces": {ns: {}}, "logs": {}, "usage": {}}
    for kind in KINDS:
        if kind != "nodes":
            out["namespaces"][ns][kind] = b.objects(kind, ns)
    for p in out["namespaces"][ns]["pods"]:
        name = p["metadata"]["name"]
        c = p["spec"]["containers"][0]["name"]
        restarted = any(s.get("restartCount", 0) > 0 for s in (p.get("status", {}).get("containerStatuses") or []))
        for prev in ((False, True) if restarted else (False,)):
            try:
                out["logs"][f"{ns}/{name}/{c}/{'previous' if prev else 'current'}"] = b.logs(ns, name, c, prev)
            except LookupError:
                pass                                      # previous container already garbage-collected — real behaviour
    try:
        out["usage"][ns] = b.usage(ns)
    except LookupError:
        out["usage"][ns] = []
    return scrub(out)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--context", default="kind-doctor-lab")
    ap.add_argument("--only")
    ap.add_argument("--keep", action="store_true", help="leave the faults running after recording")
    ap.add_argument("--timeout", type=int, default=300)
    a = ap.parse_args()

    scenarios = [json.loads(p.read_text()) for p in sorted(FAULTS.glob("*/scenario.json"))]
    scenarios = [s for s in scenarios if not s["live_only"] and (not a.only or s["id"] in a.only.split(","))]
    b = KubectlBackend(context=a.context, kubectl=KUBECTL)

    for sc in scenarios:                                  # first manifest of every scenario
        kubectl(a.context, "apply", "-f", str(FAULTS / sc["id"] / sc["apply"][0]))
    for sc in scenarios:                                  # follow-up manifests (e.g. the bad rollout) once v1 is up
        for extra in sc["apply"][1:]:
            kubectl(a.context, "-n", sc["namespace"], "rollout", "status", "deployment", "--timeout=120s")
            kubectl(a.context, "apply", "-f", str(FAULTS / sc["id"] / extra))

    pending = {sc["id"]: sc for sc in scenarios}
    deadline = time.time() + a.timeout
    while pending and time.time() < deadline:
        for id_, sc in list(pending.items()):
            if settled(b, sc):
                print(f"settled  {id_:20} ({sc['settle']})", flush=True)
                pending.pop(id_)
        time.sleep(5)
    if pending:
        print(f"did not settle within {a.timeout}s: {sorted(pending)}", file=sys.stderr)
        return 1
    time.sleep(20)                                        # let metrics-server catch up with the new pods

    (OUT / "snapshots").mkdir(parents=True, exist_ok=True)
    cluster = scrub(b.cluster_info())
    (OUT / "cluster.json").write_text(json.dumps({"cluster": cluster}, indent=1, sort_keys=True))
    for sc in scenarios:
        d = dump_namespace(b, sc["namespace"])
        d["recorded"] = {"scenario": sc["id"], "context": a.context, "time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                         "kubernetes": cluster.get("version")}
        (OUT / "snapshots" / f"{sc['id']}.json").write_text(json.dumps(d, indent=1, sort_keys=True))
        print(f"recorded {sc['id']:20} pods={len(d['namespaces'][sc['namespace']]['pods'])} logs={len(d['logs'])}")

    if not a.keep:
        for sc in scenarios:
            kubectl(a.context, "delete", "namespace", sc["namespace"], "--wait=false")
    return 0


if __name__ == "__main__":
    sys.exit(main())
