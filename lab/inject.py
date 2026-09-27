#!/usr/bin/env python3
"""Break things on purpose: apply lab faults to a cluster and leave them running, so `doctor watch` finds them.

    python -m lab.inject --context lambda --faults crashloop,cascade-db,port-mismatch
    python -m lab.inject --context lambda --faults all-multi_hop --stagger 90   # one fault every 90 s
    python -m lab.inject --context lambda --clear                              # delete what this script created
    python -m lab.inject --list

--faults takes fault ids, `all`, or `all-<tier>` (easy, multi_hop, red_herring, rightsizing). Live-only faults
(GPU, S3) are applied only when named. Every namespace it creates is labelled doctor.lab/managed=true, and
--clear deletes only namespaces with that label. Lab clusters only (kind, the Lambda k3s node) — this is the
one part of the project, with lab/record.py, that writes to a cluster; the doctor itself never does.
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from lab.record import CRYPTOGRAPHY, FAULTS, ensure_namespace, kubectl, tls_setup  # noqa: E402

LABEL = "doctor.lab/managed=true"


def catalogue() -> list[dict]:
    return [json.loads(p.read_text()) for p in sorted(FAULTS.glob("*/scenario.json"))]


def choose(spec: str, every: list[dict]) -> list[dict]:
    by_id, out = {s["id"]: s for s in every}, []
    for item in (x.strip() for x in spec.split(",") if x.strip()):
        if item == "all" or item.startswith("all-"):
            tier = item.removeprefix("all").removeprefix("-")
            out += [s for s in every if not s["live_only"] and (not tier or s["tier"] == tier)]
        elif item in by_id:
            out.append(by_id[item])
        else:
            raise SystemExit(f"unknown fault {item!r}; see --list")
    return list({s["id"]: s for s in out}.values())


def apply(context: str, sc: dict, pki: Path | None) -> None:
    ensure_namespace(context, sc["namespace"])
    if sc.get("tls_setup"):
        tls_setup(context, sc, pki)
    kubectl(context, "apply", "-f", str(FAULTS / sc["id"] / sc["apply"][0]))
    for extra in sc["apply"][1:]:                       # e.g. a bad rollout on top of a healthy one
        kubectl(context, "-n", sc["namespace"], "rollout", "status", "deployment", "--timeout=180s")
        kubectl(context, "apply", "-f", str(FAULTS / sc["id"] / extra))


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--context", default="kind-doctor-lab")
    ap.add_argument("--faults", default="")
    ap.add_argument("--stagger", type=float, default=0, help="seconds between faults (simulates incidents arriving over time)")
    ap.add_argument("--clear", action="store_true", help="delete every namespace labelled doctor.lab/managed=true")
    ap.add_argument("--list", action="store_true")
    a = ap.parse_args()
    every = catalogue()
    if a.list:
        for s in every:
            print(f"{s['id']:20} {s['tier']:12} ns={s['namespace']:16} {'live-only ' if s['live_only'] else ''}{s['title']}")
        return 0
    if a.clear:
        names = kubectl(a.context, "get", "namespaces", "-l", LABEL, "-o", "jsonpath={.items[*].metadata.name}").split()
        for ns in names:
            kubectl(a.context, "delete", "namespace", ns, "--wait=false")
        print(f"deleting {len(names)} lab namespaces: {' '.join(names) or '(none)'}")
        return 0
    chosen = choose(a.faults, every)
    if not chosen:
        raise SystemExit("nothing to inject: pass --faults (ids, all, all-<tier>) or --list")
    with tempfile.TemporaryDirectory() as tmp:
        pki = None
        if any(s.get("tls_setup") for s in chosen):
            pki = Path(tmp)
            subprocess.run(["uv", "run", "-q", "--with", CRYPTOGRAPHY, "python", str(ROOT / "lab" / "make_certs.py"), str(pki)], check=True)
        for i, sc in enumerate(chosen):
            if i and a.stagger:
                time.sleep(a.stagger)
            apply(a.context, sc, pki)
            print(f"{time.strftime('%H:%M:%S')} injected {sc['id']:20} {sc['tier']:12} ns={sc['namespace']}  expect: "
                  f"{sc['category']} on {', '.join(o['kind'] + '/' + o['name'] for o in sc['objects'])}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
