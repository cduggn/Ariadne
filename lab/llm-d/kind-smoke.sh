#!/usr/bin/env bash
# Kubernetes half of the llm-d check (design/llm-d.md), on a throwaway kind cluster with no GPU: applies the CRDs
# from deploy/serving.json, installs llm-d-router-standalone with the values serving/llmd.py renders, puts two fake
# vLLM pods labelled app=vllm behind it, and drives it through the gateway in router mode. It proves what e2e.sh
# cannot without Kubernetes: the CRD URLs apply, the chart accepts our values, the EPP discovers the pods by label,
# and the gateway's two objective names land in flow-control bands 100 and 0.
#
# The fakes have no tokenizer and publish no KV events, so it renders an approximate-prefix profile
# (MODEL=qwen3.5-9b by default) and lowers maxConcurrency to 1 so a small burst saturates. Precise routing needs
# real vLLM. KEEP=1 leaves the cluster up.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
OUT=$ROOT/.cache/llm-d-kind
MODEL=${MODEL:-qwen3.5-9b}
TOPO=${TOPO:-sliced}
CLUSTER=llmd-smoke
KUBE=(kubectl --context "kind-$CLUSTER")
KIND=${KIND:-$ROOT/.bin/kind}
mkdir -p "$OUT"

arch=$(docker version -f '{{.Server.Arch}}')
(cd "$ROOT/gateway" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$OUT/fakevllm" ./cmd/fakevllm \
  && go build -o "$OUT/gateway" ./cmd/gateway)
cat > "$OUT/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY fakevllm /fakevllm
ENTRYPOINT ["/fakevllm"]
EOF
docker build -q -t doctor/fakevllm:smoke "$OUT" >/dev/null

(cd "$ROOT" && uv run -q python -m serving.llmd "$MODEL" "$TOPO" > "$OUT/values.yaml")
python3 - "$OUT/values.yaml" <<'EOF'
import json, sys
p = sys.argv[1]
text = open(p).read()
head = "".join(l for l in text.splitlines(True) if l.startswith("#"))
v = json.loads("".join(l for l in text.splitlines(True) if not l.startswith("#")))
for plugin in v["router"]["epp"]["pluginsConfig"]["plugins"]:
    if plugin["type"] == "concurrency-detector":
        plugin["parameters"]["maxConcurrency"] = 1
# An empty map would merge back to the chart's defaults (8 CPU for the EPP), so ask for a little instead.
v["router"]["epp"]["resources"] = v["router"]["proxy"]["resources"] = {"requests": {"cpu": "100m", "memory": "128Mi"}}
open(p, "w").write(head + "# kind-smoke: maxConcurrency 1 and small resource requests\n" + json.dumps(v, indent=2) + "\n")
EOF

"$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER" || "$KIND" create cluster --name "$CLUSTER" --wait 120s >/dev/null
pids=()
cleanup() {
  [ ${#pids[@]} -eq 0 ] || kill "${pids[@]}" 2>/dev/null || true
  [ "${KEEP:-0}" = 1 ] || "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT
"$KIND" load docker-image doctor/fakevllm:smoke --name "$CLUSTER" >/dev/null

crds=$(python3 -c "import json; print(' '.join(json.load(open('$ROOT/deploy/serving.json'))['llm_d']['crds']))")
for c in $crds; do "${KUBE[@]}" apply -f "$c" >/dev/null; done
"${KUBE[@]}" apply -f - >/dev/null <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: vllm}
spec:
  replicas: 2
  selector: {matchLabels: {app: vllm}}
  template:
    metadata: {labels: {app: vllm}}
    spec:
      containers:
        - name: vllm
          image: doctor/fakevllm:smoke
          imagePullPolicy: Never
          args: [--listen, 0.0.0.0:8000, --latency, 1500ms]
          ports: [{containerPort: 8000}]
          readinessProbe: {httpGet: {path: /metrics, port: 8000}, periodSeconds: 2}
EOF
chart=$(python3 -c "import json; d=json.load(open('$ROOT/deploy/serving.json'))['llm_d']; print(d['chart'], d['chart_version'], d['release'])")
read -r CHART VERSION RELEASE <<<"$chart"
helm --kube-context "kind-$CLUSTER" upgrade --install "$RELEASE" "$CHART" --version "$VERSION" -f "$OUT/values.yaml" \
  --wait --timeout 5m >/dev/null
"${KUBE[@]}" rollout status deployment/vllm --timeout=120s >/dev/null
# The worker NetworkPolicy is the last document of the llm-d worker manifest.
(cd "$ROOT" && uv run -q python -c "from serving import profiles as p; \
print('---' + p.render_manifest(p.load_profile('$MODEL'), '$TOPO', llmd=True).rsplit('\n---', 1)[1])") \
  | "${KUBE[@]}" apply -f - >/dev/null
echo "--- installed: $("${KUBE[@]}" get inferencepool,inferenceobjective,networkpolicy -o name | tr '\n' ' ')"

# EPP's /metrics checks the caller's token (--metrics-endpoint-auth defaults to true), so read it as a scraper
# would: a ServiceAccount allowed get on the /metrics URL.
"${KUBE[@]}" create clusterrole epp-metrics-reader --verb=get --non-resource-url=/metrics --dry-run=client -o yaml | "${KUBE[@]}" apply -f - >/dev/null
"${KUBE[@]}" create serviceaccount metrics-reader --dry-run=client -o yaml | "${KUBE[@]}" apply -f - >/dev/null
"${KUBE[@]}" create clusterrolebinding epp-metrics-reader --clusterrole=epp-metrics-reader --serviceaccount=default:metrics-reader \
  --dry-run=client -o yaml | "${KUBE[@]}" apply -f - >/dev/null
TOKEN=$("${KUBE[@]}" create token metrics-reader)
epm() { curl -s -m 5 -H "Authorization: Bearer $TOKEN" http://127.0.0.1:18290/metrics; }
"${KUBE[@]}" port-forward "svc/$RELEASE-proxy" 18281:8081 >/dev/null 2>&1 & pids+=($!)
"${KUBE[@]}" port-forward "svc/$RELEASE-epp" 18290:9090 >/dev/null 2>&1 & pids+=($!)
"$OUT/gateway" --listen 127.0.0.1:18200 --router-url http://127.0.0.1:18281 > "$OUT/gateway.log" 2>&1 & pids+=($!)
for _ in $(seq 100); do curl -sf http://127.0.0.1:18200/readyz >/dev/null && epm >/dev/null && break; sleep 0.3; done
for _ in $(seq 60); do
  epm | grep -qE '^llm_d_epp_ready_endpoints\{.*\} 2$' && break; sleep 1
done

fails=0
check() { if "${@:2}"; then echo "ok    $1"; else echo "FAIL  $1"; fails=$((fails + 1)); fi; }
body='{"model":"fake","messages":[{"role":"user","content":"why is checkout crashlooping"}],"max_completion_tokens":8}'
send() {  # send <id> <priority>: prints the status
  curl -s -m 60 -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18200/v1/chat/completions -H 'Content-Type: application/json' \
    -H "X-Request-Id: $1" -H 'X-Tenant: platform' -H "X-Priority: $2" -H 'X-Data-Class: restricted' --data-binary "$body"
}
check "EPP discovered both pods by label" grep -qE '^llm_d_epp_ready_endpoints\{.*\} 2$' <<<"$(epm)"
check "one request answers 200 through the chart's proxy" test "$(send solo-s1 interactive)" = 200
pod_ip=$("${KUBE[@]}" get pods -l app=vllm -o jsonpath='{.items[0].status.podIP}')
stranger=$("${KUBE[@]}" run stranger --image=curlimages/curl:8.11.0 --restart=Never --rm -i --quiet --command -- \
  curl -s -m 3 -o /dev/null -w '%{http_code}' "http://$pod_ip:8000/metrics" 2>/dev/null || true)
echo "      a pod with no allowed label calling a worker's :8000 got: ${stranger:-nothing}"
check "the worker NetworkPolicy refuses a pod that is not the router, the gateway or Prometheus" test "${stranger:-000}" = 000
burst=()
for i in $(seq 4); do
  send "i-$i-s1" interactive > "$OUT/i-$i" & burst+=($!)
  send "b-$i-s1" batch > "$OUT/b-$i" & burst+=($!)
done
wait "${burst[@]}"
codes=$(cat "$OUT"/i-* "$OUT"/b-* | sort | uniq -c | tr '\n' ' ')
echo "      burst of 4 interactive + 4 batch at maxConcurrency 1: $codes"
em=$(epm)
{ grep -E '^llm_d_epp_flow_control_requests_total' <<<"$em" || true; } | sed 's/^/      /'
check "doctor-interactive landed in band 100" grep -qE '^llm_d_epp_flow_control_requests_total\{.*priority="100".*\} [1-9]' <<<"$em"
check "doctor-batch landed in band 0" grep -qE '^llm_d_epp_flow_control_requests_total\{.*priority="0".*\} [1-9]' <<<"$em"
# The rendered bands cap waiting time (the gateway's queue budgets), not queue length, so a burst this small waits
# its turn instead of being shed; e2e.sh covers the shed path with a deliberately tiny queue.
check "every queued request was served within its band's TTL" test "$(cat "$OUT"/i-* "$OUT"/b-* | grep -cx 200)" = 8
check "the saturated pool queued requests in flow control" grep -qE '^llm_d_epp_flow_control_request_queue_duration_seconds_count\{.*\} [1-9]' <<<"$em"
echo "--- $fails failed"
exit "$fails"
