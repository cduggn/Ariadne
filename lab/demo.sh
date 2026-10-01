#!/usr/bin/env bash
# Laptop demo of the gateway with no GPU: two fake vLLM workers, the gateway in front of them, and the golden
# set at concurrency CONC through the gateway. Prints the gateway's decisions from its own /metrics and keeps
# everything under .cache/demo. Scores are meaningless here; the fakes answer every run as inconclusive.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/.cache/demo
CONC=${CONC:-8}
REPEAT=${REPEAT:-1}
GW=127.0.0.1:18000
mkdir -p "$OUT"

(cd "$ROOT/gateway" && go build -o "$OUT/gateway" ./cmd/gateway && go build -o "$OUT/fakevllm" ./cmd/fakevllm)
echo '{"model":"Qwen/Qwen3-8B-AWQ","messages":[{"role":"user","content":"warm-up"}],"max_completion_tokens":1}' > "$OUT/warm.json"

pids=()
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT
"$OUT/fakevllm" --listen 127.0.0.1:18001 2> "$OUT/vllm-0.log" & pids+=($!)
"$OUT/fakevllm" --listen 127.0.0.1:18002 2> "$OUT/vllm-1.log" & pids+=($!)
"$OUT/gateway" --listen "$GW" --warm-body "$OUT/warm.json" \
  --workers vllm-0=http://127.0.0.1:18001,vllm-1=http://127.0.0.1:18002 > "$OUT/gateway.log" 2>&1 & pids+=($!)

for _ in $(seq 50); do
  curl -sf "http://$GW/readyz" >/dev/null && break
  sleep 0.2
done
curl -sf "http://$GW/readyz" >/dev/null || { echo "gateway never became ready; see $OUT/gateway.log"; exit 1; }
curl -s "http://$GW/debug/workers" | python3 -c 'import json,sys; [print(w["pod"], w["phase"]) for w in json.load(sys.stdin)]'

(cd "$ROOT" && uv run -q python -m evals.run_golden --base-url "http://$GW/v1" --workers 2 --tag demo \
  --concurrency "$CONC" --repeat "$REPEAT" --out "$OUT")

curl -sf "http://$GW/metrics" > "$OUT/gateway.prom"
echo "--- gateway decisions ($OUT/gateway.prom)"
grep -E '^orch_(requests|pick|sticky|shed|overflow|restricted_offbox|prompt_tokens)_total' "$OUT/gateway.prom" | grep -v ' 0$' || true
grep -E '^orch_(overflow|restricted_offbox)_total' "$OUT/gateway.prom"
