#!/usr/bin/env bash
# Laptop demo of the gateway with no GPU: two fake vLLM workers, the gateway in front of them, and the golden
# set at concurrency CONC through the gateway. Prints the gateway's decisions from its own /metrics and keeps
# everything under .cache/demo. Scores are meaningless here; the fakes answer every run as inconclusive.
#
# OVERFLOW=1 is the saturation demo instead (design/overflow.md): a fake overflow backend stands in for Superlinked,
# both workers report a full KV cache so every request is a 503, and two requests show the stay-or-leave rule: the
# restricted one stays a 503 (blocked_invariant), the internal one is answered by the fake backend (sent).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/.cache/demo
CONC=${CONC:-8}
REPEAT=${REPEAT:-1}
OVERFLOW=${OVERFLOW:-0}
GW=127.0.0.1:18000
mkdir -p "$OUT"

(cd "$ROOT/gateway" && go build -o "$OUT/gateway" ./cmd/gateway && go build -o "$OUT/fakevllm" ./cmd/fakevllm \
  && go build -o "$OUT/fakeoverflow" ./cmd/fakeoverflow)
echo '{"model":"Qwen/Qwen3-8B-AWQ","messages":[{"role":"user","content":"warm-up"}],"max_completion_tokens":1}' > "$OUT/warm.json"

pids=()
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT
kv=0.1
gwflags=()
if [ "$OVERFLOW" = 1 ]; then
  kv=0.95   # saturated from the start; warm-up still passes, so both workers are Ready and every request sheds on kv_free
  "$OUT/fakeoverflow" --listen 127.0.0.1:18010 --key demo-key 2> "$OUT/overflow.log" & pids+=($!)
  export GW_OVERFLOW_API_KEY=demo-key
  gwflags=(--overflow --overflow-base-url http://127.0.0.1:18010 --overflow-model demo-model --overflow-probe-interval 2s)
fi
"$OUT/fakevllm" --listen 127.0.0.1:18001 --kv-usage "$kv" 2> "$OUT/vllm-0.log" & pids+=($!)
"$OUT/fakevllm" --listen 127.0.0.1:18002 --kv-usage "$kv" 2> "$OUT/vllm-1.log" & pids+=($!)
"$OUT/gateway" --listen "$GW" --warm-body "$OUT/warm.json" \
  --workers vllm-0=http://127.0.0.1:18001,vllm-1=http://127.0.0.1:18002 "${gwflags[@]}" > "$OUT/gateway.log" 2>&1 & pids+=($!)

for _ in $(seq 50); do
  curl -sf "http://$GW/readyz" >/dev/null && break
  sleep 0.2
done
curl -sf "http://$GW/readyz" >/dev/null || { echo "gateway never became ready; see $OUT/gateway.log"; exit 1; }
curl -s "http://$GW/debug/workers" | python3 -c 'import json,sys; [print(w["pod"], w["phase"]) for w in json.load(sys.stdin)]'

if [ "$OVERFLOW" = 1 ]; then
  for _ in $(seq 50); do
    grep -q '"overflow backend available"' "$OUT/gateway.log" && break
    sleep 0.2
  done
  send() {  # send <request id> <data class>: one doctor-shaped request, printing the status and the gateway's headers
    curl -s -o "$OUT/$1.json" -D - -X POST "http://$GW/v1/chat/completions" \
      -H 'Content-Type: application/json' -H "X-Request-Id: $1" -H 'X-Tenant: platform' -H 'X-App: cluster-doctor' \
      -H 'X-Priority: interactive' -H "X-Data-Class: $2" --data-binary @"$OUT/warm.json" \
      | grep -iE '^HTTP/|^X-Overflow|^X-Gateway-Reason|^Retry-After' | tr -d '\r' | sed "s/^/  $1 ($2): /"
  }
  echo "--- a restricted 503 stays, an internal 503 leaves"
  send demo-restricted-s1 restricted
  send demo-internal-s1 internal
  echo "--- gateway log ($OUT/gateway.log)"
  grep '"msg":"request"' "$OUT/gateway.log" | python3 "$ROOT/lab/tail.py"
  curl -sf "http://$GW/metrics" > "$OUT/gateway.prom"
  echo "--- overflow metrics ($OUT/gateway.prom)"
  grep -E '^orch_(overflow|restricted_offbox)' "$OUT/gateway.prom"
  grep -E '^orch_completed_total\{.*pod="overflow"' "$OUT/gateway.prom" || true
  exit 0
fi

(cd "$ROOT" && uv run -q python -m evals.run_golden --base-url "http://$GW/v1" --workers 2 --tag demo \
  --concurrency "$CONC" --repeat "$REPEAT" --out "$OUT")

curl -sf "http://$GW/metrics" > "$OUT/gateway.prom"
echo "--- gateway decisions ($OUT/gateway.prom)"
grep -E '^orch_(requests|pick|sticky|shed|overflow|restricted_offbox|prompt_tokens)_total' "$OUT/gateway.prom" | grep -v ' 0$' || true
grep -E '^orch_(overflow|restricted_offbox)_total' "$OUT/gateway.prom"
