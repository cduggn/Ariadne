#!/usr/bin/env bash
# End-to-end check of router mode on a laptop (design/llm-d.md): the real llm-d v1.0.0 EPP and Envoy place
# requests on two fake vLLM workers, and our gateway in front keeps the quota, the refusal contract and
# stay-or-leave. It asserts what the unit tests cannot, that the real EPP's drops arrive as the
# x-llm-d-request-dropped-reason the gateway maps, and that a saturated router still lets an internal
# request leave while a restricted one stays. Needs docker; KEEP=1 leaves the lab up for poking.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
export BIN=$ROOT/.cache/llm-d-lab
GW=http://127.0.0.1:18100
mkdir -p "$BIN"
arch=$(docker version -f '{{.Server.Arch}}')
(cd "$ROOT/gateway" && for c in gateway fakevllm fakeoverflow; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$BIN/$c" "./cmd/$c"; done)

if docker compose version >/dev/null 2>&1; then DC=(docker compose); else DC=(docker-compose); fi
compose() { "${DC[@]}" -f "$HERE/compose.yaml" "$@"; }
[ "${KEEP:-0}" = 1 ] || trap 'compose logs --no-color > "$BIN/lab.log" 2>&1 || true; compose down -v >/dev/null 2>&1 || true' EXIT
compose up -d --quiet-pull >/dev/null

fails=0
check() {  # check <description> <command...>: one assertion, printed either way
  if "${@:2}"; then echo "ok    $1"; else echo "FAIL  $1"; fails=$((fails + 1)); fi
}
wait_for() {  # wait_for <seconds> <command...>
  for _ in $(seq $(($1 * 5))); do "${@:2}" >/dev/null 2>&1 && return 0; sleep 0.2; done
  return 1
}
wait_for 30 curl -sf "$GW/readyz" || { echo "gateway never became ready"; compose logs gateway; exit 1; }
wait_for 30 curl -sf http://127.0.0.1:19000/ready || { echo "envoy never became ready"; compose logs envoy; exit 1; }
wait_for 30 sh -c "${DC[*]} -f '$HERE/compose.yaml' logs gateway | grep -q 'overflow backend available'" \
  || { echo "overflow backend never became available"; compose logs gateway; exit 1; }
# EPP reads the endpoints file and scrapes the fakes before it can place anything.
wait_for 30 sh -c "curl -s http://127.0.0.1:19090/metrics | grep -q '^llm_d_epp_ready_endpoints.* 2$'" \
  || echo "note  llm_d_epp_ready_endpoints never read 2; continuing"

body='{"model":"Qwen/Qwen3-8B-AWQ","messages":[{"role":"system","content":"'"$(printf 'cluster card %.0s' $(seq 200))"'"},{"role":"user","content":"why is checkout crashlooping"}],"max_completion_tokens":16}'
send() {  # send <request id> <data class> <priority>: writes the status and headers to $BIN/<id>.h
  curl -s -o "$BIN/$1.json" -D "$BIN/$1.h" -X POST "$GW/v1/chat/completions" \
    -H 'Content-Type: application/json' -H "X-Request-Id: $1" -H 'X-Tenant: platform' -H 'X-App: cluster-doctor' \
    -H "X-Priority: $3" -H "X-Data-Class: $2" -H 'x-llm-d-inference-fairness-id: spoofed' \
    -H 'x-gateway-destination-endpoint: 10.88.0.30:8010' --data-binary "$body"
}
hdr() { tr -d '\r' < "$BIN/$1.h" | awk -v k="$(echo "$2" | tr 'A-Z' 'a-z')" 'BEGIN{IGNORECASE=1} tolower($1)==k":" {print $2}' | tail -1; }
status() { tr -d '\r' < "$BIN/$1.h" | awk '/^HTTP\//{s=$2} END{print s}'; }

echo "--- one request through gateway, Envoy, EPP and a fake worker"
send solo-s1 internal interactive
check "200 through the router" test "$(status solo-s1)" = 200
check "X-Policy is router" test "$(hdr solo-s1 X-Policy)" = router
check "a spoofed x-gateway-destination-endpoint did not steer the request" test "$(hdr solo-s1 X-Pod)" != 10.88.0.30:8010
echo "      X-Pod: $(hdr solo-s1 X-Pod)"

echo "--- the same prefix three more times stays on one worker (approximate prefix scorer)"
# Envoy does not hand the served endpoint back (X-Pod reads "router"), so its admin per-host counters say who served.
served() { curl -s http://127.0.0.1:19000/clusters | awk -F'::' '$1=="original_destination_cluster" && $3=="rq_total" {print $2"="$4}' | sort; }
before=$(served)
for i in 2 3 4; do send "solo-s$i" internal interactive; done
after=$(served)
echo "      envoy rq_total before: $(echo $before)  after: $(echo $after)"
moved=$(awk -F= 'NR==FNR {b[$1]=$2; next} $2>b[$1]+0 {n++} END{print n+0}' <(echo "$before") <(echo "$after"))
check "all three repeats were served by a single endpoint" test "$moved" = 1

burst() {  # burst <tag> <data class> <n>: n concurrent batch requests
  for i in $(seq "$3"); do send "$1-$i-s1" "$2" batch & done
  wait
}
echo "--- a burst of restricted requests saturates the router; they stay on the box"
burst restricted restricted 8
r_shed=0; r_out=0
for i in $(seq 8); do
  [ "$(hdr "restricted-$i-s1" X-Gateway-Reason)" = router_saturated ] && [ "$(status "restricted-$i-s1")" = 503 ] && r_shed=$((r_shed + 1))
  [ -n "$(hdr "restricted-$i-s1" X-Overflow)" ] && r_out=$((r_out + 1))
done
echo "      $r_shed of 8 answered 503 router_saturated"
check "llm-d's saturation 429 reached the client as a 503 router_saturated" test "$r_shed" -ge 1
check "no restricted request left the box" test "$r_out" = 0
check "a saturated 503 carries Retry-After" sh -c "grep -qi '^retry-after:' $BIN/restricted-*-s1.h"

echo "--- the same burst of internal requests: the ones the router drops overflow"
burst internal internal 8
sent=0
for i in $(seq 8); do [ "$(hdr "internal-$i-s1" X-Overflow)" = sent ] && sent=$((sent + 1)); done
echo "      $sent of 8 answered by the overflow backend"
check "a router drop of an internal request was answered off-box" test "$sent" -ge 1

echo "--- metrics"
gm=$(curl -s "$GW/metrics")
check "orch_shed_total counts router_saturated" grep -q 'orch_shed_total{.*reason="router_saturated"' <<<"$gm"
check "orch_overflow_total counts sent" grep -q 'orch_overflow_total{.*result="sent"' <<<"$gm"
em=$(curl -s http://127.0.0.1:19090/metrics)
check "EPP flow control counted requests" grep -q '^llm_d_epp_flow_control_requests_total' <<<"$em"
grep -E '^llm_d_epp_(flow_control_requests_total|ready_endpoints)' <<<"$em" | sed 's/^/      /'

echo "--- $fails failed"
exit "$fails"
