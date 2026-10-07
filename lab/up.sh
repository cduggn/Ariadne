#!/usr/bin/env bash
# Launch one GPU node: the first type in TYPES with capacity in any region, re-checked every minute until RETRY
# minutes pass. Cloud-init (deploy/cloud-init.yaml) reads the hardware it lands on, so every type uses the same file.
# Then writes .cache/node.env (GPU, ARCH, MODEL, TOPO, HOP) from the node's ready.json and serving.json's boot
# settings for that GPU; the Makefile includes it, so `make deploy`, `make gateway` and `make golden` follow whichever
# GPU came up.
#
#   lab/up.sh            launch, then write node.env
#   lab/up.sh --resume   no launch: wait for the instance named NAME (a launch that timed out while Lambda was still
#                        provisioning it), then write node.env
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
NAME=${NAME:-cluster-doctor}
TYPES=${TYPES:-gpu_1x_h100_pcie gpu_1x_h100_sxm5 gpu_1x_gh200 gpu_1x_a100_sxm4}
RETRY=${RETRY:-30}
PREFER=$(lam config 2>/dev/null | awk '$1 == "LAM_REGION:" {print $2}')

# An instance is ours if lam ls shows its name as a whole word.
exists() { lam ls 2>/dev/null | grep -qw -- "$NAME"; }

if [ "${1:-}" = --resume ]; then
  exists || { echo "no instance named $NAME to resume (lam ls)" >&2; exit 1; }
  echo "== waiting for $NAME: active, sshd up and cloud-init finished"
  lam wait "$NAME"
elif exists; then
  echo "an instance named $NAME is already running (lam ls); make down first, make resume, or pick another NAME=" >&2
  exit 1
else
  launch_until_deadline=1
fi

launch() {
  local avail t regions region
  avail=$(lam types --available)
  for t in $TYPES; do
    regions=$(awk -v t="$t" '$1 == t {print $NF}' <<<"$avail")
    [ -z "$regions" ] || [ "$regions" = "-" ] && continue
    region=${regions%%,*}
    [[ ",$regions," == *",$PREFER,"* ]] && region=$PREFER
    echo "== $t has capacity in $regions; launching in $region"
    # Capacity can vanish between the listing and the launch; then try the next type. But a launch that created
    # the instance and failed later (waiting for ssh or cloud-init) must not be retried, or each retry bills a
    # second node.
    lam launch -c "$ROOT/deploy/cloud-init.yaml" --name "$NAME" --type "$t" --region "$region" && return 0
    if exists; then
      echo "$t was launched as $NAME but did not finish booting; not launching another." >&2
      echo "make resume waits for it and finishes setup; make down removes it." >&2
      exit 1
    fi
  done
  return 1
}

if [ -n "${launch_until_deadline:-}" ]; then
  deadline=$(( $(date +%s) + RETRY * 60 ))
  until launch; do
    [ "$(date +%s)" -ge "$deadline" ] && { echo "no capacity for any of: $TYPES (gave up after $RETRY min)"; exit 1; }
    echo "== no capacity for: $TYPES; checking again in 60 s"
    sleep 60
  done
fi

mkdir -p "$ROOT/.cache"
lam ssh "$NAME" -- cat /var/lib/bootstrap/ready.json | tee "$ROOT/.cache/ready.json"
python3 - "$ROOT" <<'EOF'
import json, sys
from pathlib import Path
root = Path(sys.argv[1])
ready = json.loads((root / ".cache" / "ready.json").read_text())
boot = json.loads((root / "deploy" / "serving.json").read_text())["gpus"][ready["gpu"]]["boot"]
env = {"GPU": ready["gpu"], "ARCH": ready["arch"], "MODEL": boot["model"], "TOPO": boot["topology"],
       "HOP": int(boot.get("hop", False))}
(root / ".cache" / "node.env").write_text("".join(f"{k} = {v}\n" for k, v in env.items()))
print("node: " + "  ".join(f"{k}={v}" for k, v in env.items()) + "   (.cache/node.env; override any of them on the make line)")
EOF
