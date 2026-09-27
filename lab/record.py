#!/usr/bin/env python3
"""Inject lab faults into the kind cluster, wait until each has settled, and record snapshots.

    python3 -m lab.record                           # all kind scenarios, batches of 4
    python3 -m lab.record --only oom,cascade-db     # a subset (keeps the other fixtures)
    python3 -m lab.record --batch 3 --keep

1. Create every scenario's namespace first, then record fixtures/cluster.json — so every snapshot shares
   one cluster card (the same shared prefix a live cluster would give).
2. Run scenarios in small batches (a 2-CPU / 2 GiB node must not create accidental faults): TLS setup
   (throwaway PKI from lab/make_certs.py → Secrets and public-CA ConfigMaps), apply manifests, poll until
   every settle rule holds, sample usage for right-sizing scenarios, dump, delete the batch's workloads.

Every string is redacted before it is written (fixtures live in git). Private keys exist only in a temp
directory and in lab Secrets, which the doctor never reads. Only this script mutates a cluster (lab only).
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from doctor.backends import KINDS, KubectlBackend  # noqa: E402
from doctor.redact import redact  # noqa: E402

FAULTS = ROOT / "faults"
OUT = ROOT / "fixtures"
KUBECTL = str(ROOT / ".bin" / "kubectl")
CRYPTOGRAPHY = "cryptography==50.0.1"


def kubectl(context: str, *args: str, stdin: str | None = None) -> str:
    r = subprocess.run([KUBECTL, "--context", context, *args], input=stdin, capture_output=True, text=True, timeout=240, check=False)
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


def containers_of(p: dict) -> list[str]:
    return [c["name"] for c in p["spec"].get("initContainers", []) + p["spec"]["containers"]]


def _rule(b: KubectlBackend, ns: str, rule: str) -> bool:
    pods = b.objects("pods", ns)
    cstats = [c for p in pods for c in (p.get("status", {}).get("containerStatuses") or [])]
    if rule.startswith("restarts>="):
        n = int(rule.split(">=")[1])
        return any(c.get("restartCount", 0) >= n for c in cstats)
    if rule == "oom":
        return any(c.get("restartCount", 0) >= 2 and c.get("lastState", {}).get("terminated", {}).get("reason") == "OOMKilled" for c in cstats)
    if rule.startswith("waiting:"):
        want = rule.split(":", 1)[1]
        return any(c.get("state", {}).get("waiting", {}).get("reason") == want for c in cstats)
    if rule.startswith("event:"):
        want = rule.split(":", 1)[1]
        return any(e.get("reason") == want for e in b.objects("events", ns))
    if rule.startswith("log:"):
        want = rule.split(":", 1)[1]
        for p in pods:
            for c in containers_of(p):
                try:
                    if want in b.logs(ns, p["metadata"]["name"], c, False):
                        return True
                except LookupError:
                    pass
        return False
    if rule == "ready_all":
        return bool(cstats) and all(c.get("ready") for c in cstats) and all(p["status"].get("phase") == "Running" for p in pods)
    if rule == "job_failed":
        return any(c.get("type") == "Failed" and c.get("status") == "True"
                   for j in b.objects("jobs", ns) for c in j.get("status", {}).get("conditions", []))
    if rule == "deploy_stalled":
        return any(c.get("type") == "Progressing" and c.get("reason") == "ProgressDeadlineExceeded"
                   for d in b.objects("deployments", ns) for c in d.get("status", {}).get("conditions", []))
    if rule == "init_stuck":
        now = dt.datetime.now(dt.UTC)
        for p in pods:
            started = p["metadata"].get("creationTimestamp")
            age = (now - dt.datetime.strptime(started, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.UTC)).total_seconds() if started else 0
            if age >= 60 and any("running" in ic.get("state", {}) for ic in p.get("status", {}).get("initContainerStatuses") or []):
                return True
        return False
    if rule == "evicted":
        return any(p.get("status", {}).get("reason") == "Evicted" for p in pods)
    raise ValueError(f"unknown settle rule {rule}")


def settled(b: KubectlBackend, sc: dict) -> bool:
    rules = sc["settle"] if isinstance(sc["settle"], list) else [sc["settle"]]
    return all(_rule(b, sc["namespace"], r) for r in rules)


def tls_setup(context: str, sc: dict, pki: Path) -> None:
    ns = sc["namespace"]
    for item in sc.get("tls_setup", []):
        if item["type"] == "secret":
            y = kubectl(context, "-n", ns, "create", "secret", "tls", item["name"], f"--cert={pki / (item['cert'] + '.crt')}",
                        f"--key={pki / (item['cert'] + '.key')}", "--dry-run=client", "-o", "yaml")
        else:
            y = kubectl(context, "-n", ns, "create", "configmap", item["name"], f"--from-file={item['key']}={pki / (item['cert'] + '.crt')}",
                        "--dry-run=client", "-o", "yaml")
        kubectl(context, "apply", "-f", "-", stdin=y)


def dump_namespace(b: KubectlBackend, sc: dict) -> dict:
    ns = sc["namespace"]
    out: dict = {"namespaces": {ns: {}}, "logs": {}, "usage": {}}
    for kind in KINDS:
        if kind != "nodes":
            out["namespaces"][ns][kind] = b.objects(kind, ns)
    for p in out["namespaces"][ns]["pods"]:
        name, st = p["metadata"]["name"], p.get("status", {})
        statuses = {s["name"]: s for s in (st.get("containerStatuses") or []) + (st.get("initContainerStatuses") or [])}
        for c in containers_of(p):
            restarted = statuses.get(c, {}).get("restartCount", 0) > 0
            for prev in ((False, True) if restarted else (False,)):
                try:
                    out["logs"][f"{ns}/{name}/{c}/{'previous' if prev else 'current'}"] = b.logs(ns, name, c, prev)
                except LookupError:
                    pass                          # evicted, never started, or previous container already GC'd — real behaviour
    try:
        out["usage"][ns] = b.usage(ns)
    except LookupError:
        out["usage"][ns] = []
    return out


def sample_usage(b: KubectlBackend, ns: str, window_s: int, every_s: int = 15) -> list[dict]:
    series, end = [], time.time() + window_s
    while time.time() < end:
        try:
            series.append({"t": dt.datetime.now(dt.UTC).strftime("%Y-%m-%dT%H:%M:%SZ"), "rows": b.usage(ns)})
        except LookupError:
            pass
        time.sleep(every_s)
    return series


def ensure_namespace(context: str, ns: str) -> None:
    y = kubectl(context, "create", "namespace", ns, "--dry-run=client", "-o", "yaml")
    kubectl(context, "apply", "-f", "-", stdin=y)
    kubectl(context, "label", "namespace", ns, "doctor.lab/managed=true", "--overwrite")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--context", default="kind-doctor-lab")
    ap.add_argument("--only")
    ap.add_argument("--batch", type=int, default=4)
    ap.add_argument("--keep", action="store_true", help="leave the last batch running")
    ap.add_argument("--timeout", type=int, default=420)
    a = ap.parse_args()

    every = [json.loads(p.read_text()) for p in sorted(FAULTS.glob("*/scenario.json"))]
    every = [s for s in every if not s["live_only"]]
    chosen = [s for s in every if not a.only or s["id"] in a.only.split(",")]
    b = KubectlBackend(context=a.context, kubectl=KUBECTL)
    failed: list[str] = []

    for sc in every:                                   # all namespaces exist before the card is recorded
        ensure_namespace(a.context, sc["namespace"])
    OUT.joinpath("snapshots").mkdir(parents=True, exist_ok=True)
    cluster = redact_all(b.cluster_info())
    (OUT / "cluster.json").write_text(json.dumps({"cluster": cluster}, indent=1, sort_keys=True))

    with tempfile.TemporaryDirectory() as tmp:
        pki = Path(tmp)
        subprocess.run(["uv", "run", "-q", "--with", CRYPTOGRAPHY, "python", str(ROOT / "lab" / "make_certs.py"), str(pki)], check=True)
        for i in range(0, len(chosen), a.batch):
            batch = chosen[i:i + a.batch]
            print(f"batch {i // a.batch + 1}: {[s['id'] for s in batch]}", flush=True)
            for sc in batch:
                tls_setup(a.context, sc, pki)
                kubectl(a.context, "apply", "-f", str(FAULTS / sc["id"] / sc["apply"][0]))
            for sc in batch:
                for extra in sc["apply"][1:]:
                    kubectl(a.context, "-n", sc["namespace"], "rollout", "status", "deployment", "--timeout=120s")
                    kubectl(a.context, "apply", "-f", str(FAULTS / sc["id"] / extra))
            pending, deadline = {s["id"]: s for s in batch}, time.time() + a.timeout
            while pending and time.time() < deadline:
                for id_, sc in list(pending.items()):
                    if settled(b, sc):
                        print(f"  settled  {id_:20} {sc['settle']}", flush=True)
                        pending.pop(id_)
                time.sleep(5)
            if pending:                                # skip and report; keep recording the rest
                print(f"  DID NOT SETTLE within {a.timeout}s (not recorded): {sorted(pending)}", file=sys.stderr, flush=True)
                failed.extend(pending)
            time.sleep(20)                             # let metrics-server catch up
            series = {sc["id"]: sample_usage(b, sc["namespace"], sc["usage_window_s"]) for sc in batch if sc.get("usage_window_s")}
            for sc in batch:
                if sc["id"] in pending:
                    continue
                d = dump_namespace(b, sc)
                if sc["id"] in series:
                    d["usage_series"] = {sc["namespace"]: series[sc["id"]]}
                d["recorded"] = {"scenario": sc["id"], "context": a.context, "kubernetes": cluster.get("version"),
                                 "time": dt.datetime.now(dt.UTC).strftime("%Y-%m-%dT%H:%M:%SZ")}
                (OUT / "snapshots" / f"{sc['id']}.json").write_text(json.dumps(redact_all(d), indent=1, sort_keys=True))
                print(f"  recorded {sc['id']:20} pods={len(d['namespaces'][sc['namespace']]['pods'])} logs={len(d['logs'])}", flush=True)
            if not (a.keep and i + a.batch >= len(chosen)):
                for sc in batch:                       # free the node, keep the namespace the card lists
                    kubectl(a.context, "delete", "namespace", sc["namespace"], "--wait=true", "--timeout=240s")
                    ensure_namespace(a.context, sc["namespace"])
    print(f"done; not settled: {failed or 'none'}", flush=True)
    return 1 if failed else 0


redact_all = scrub

if __name__ == "__main__":
    sys.exit(main())
