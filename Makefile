# cluster-doctor — local fault lab (kind), offline evaluation, and the Lambda A100 lifecycle.
#
#   make tools                      fetch pinned kind + kubectl into .bin/ (checksums verified)
#   make test / lint                offline: unit tests over recorded snapshots, gateway go vet + tests, ruff
#   make lab-up / lab-record / lab-down   kind cluster, inject faults, record fixtures/  (lab only mutates kind)
#   make golden-build               rebuild evals/golden from faults/ + fixtures/ (references must pass)
#   make gateway-image              build + push the gateway image (Docker Hub, public), pin its digest
#   make up / deploy / kv / tunnel / grafana / dashboards / down   Lambda GPU node via the `lam` CLI: up takes the first of TYPES with capacity (lab/up.sh)
#   make gateway [POLICY=least_loaded]   apply the pinned gateway image + warm-up body on the node and roll it out (D-42)
#   make tunnel [TUNNEL=pod/vllm-0]      localhost:8000 → the gateway (default) or one vLLM pod directly
#   make demo                       laptop only: two fake vLLM workers behind the gateway, golden set at concurrency 8
#   make models / fit [MODEL=… TOPO=…] / fit-all   model profiles (deploy/models) and whether each fits sliced | full (D-40)
#   make deploy MODEL=… TOPO=…      render, fetch weights, serve that model on that topology (fit gate first)
#   make golden TAG=… [BASE=…]      run the golden set against a model endpoint (vLLM or the gateway); v1 + v2 scores
#   make matrix                     design/model-matrix.md: fit + results + ranking from deploy/ and metrics/
#   make sweep [LEVELS="1 4 8 16 32"] REPEAT=2   golden set at each concurrency + a vLLM /metrics scrape per level
#   make preflight                  everything that must be true before paying for a GPU
#   make kubeconfig / k8s-tunnel / record-live ONLY=gpu-unavailable   live-only faults on the Lambda k3s cluster
#   make watch [WATCH_ARGS=…]       the doctor, autonomous, against CTX (default lambda); /metrics on :9109
#   make inject FAULTS=… [STAGGER=60] / heal   break the lab cluster on purpose / remove what inject created
#   make faults                     list injectable faults by tier
#   make prom / opencost            port-forward Prometheus (:9090) / the OpenCost API (:9003) from the Lambda node
# `make up` writes the node it got (GPU, ARCH, MODEL, TOPO) here; anything on the make line still wins.
-include .cache/node.env
NAME   ?= cluster-doctor
TAG    ?= baseline
N      ?= 2
PORT   ?= 8000
MODEL  ?= qwen3-8b-awq
TOPO   ?= sliced
ARCH   ?= amd64
TYPES  ?= gpu_1x_gh200 gpu_1x_h100_pcie gpu_1x_a100_sxm4
WORKERS ?= 1
BASE   ?= http://127.0.0.1:$(PORT)/v1
CONC   ?= 1
LEVELS ?= 1 4 8 16 32
REPEAT ?= 2
CTX    ?= lambda
TUNNEL ?= svc/gateway
POLICY ?= prefix_then_load
FAULTS ?= crashloop,cascade-db,port-mismatch,tls-truststore
STAGGER ?= 0
WATCH_EXCLUDE ?= monitoring,opencost,doctor,default
KCFG   := $(CURDIR)/.cache/lambda-kubeconfig
STAMP  := $(shell date +%Y%m%d-%H%M%S)
KUBECTL := $(CURDIR)/.bin/kubectl
KIND    := $(CURDIR)/.bin/kind
REMOTE  = lam ssh $(NAME) --
PROFILE = deploy/models/$(MODEL).json
RENDER  = .cache/deploy/$(MODEL)-$(TOPO)
PY      = uv run -q python
RUFF    = uvx -q ruff@0.13.2
KENV    = $(if $(filter lambda,$(CTX)),KUBECONFIG=$(KCFG))
GWBUILD = .cache/gateway
GW_IMAGE ?= docker.io/cdugga/cluster-doctor-gateway
GW_TAG  = $(shell git rev-parse --short HEAD)$(shell git diff --quiet HEAD -- gateway || echo -dirty)
SCRAPE  = $(if $(filter svc/gateway,$(TUNNEL)),gateway,vllm)
PODS    = vllm-0 vllm-1

.PHONY: demo gateway gateway-image models fit fit-all gate render prefetch matrix tools preflight sweep kubeconfig k8s-tunnel record-live watch watch-metrics inject heal prom opencost faults test lint golden-build lab-up lab-record lab-down up status deploy scale logs kv tunnel dashboards grafana golden metrics down

tools:
	bash lab/get-tools.sh

test:
	uv run -q python -m pytest
	cd gateway && go vet ./... && go test ./...

lint:
	$(RUFF) check doctor evals lab serving tests

models:
	$(PY) -m serving.profiles list

fit:
	$(PY) -m serving.fit $(MODEL) $(TOPO)

fit-all:
	$(PY) -m serving.fit all

render:
	$(PY) -m serving.profiles render $(MODEL) $(TOPO) --out $(RENDER)

matrix:
	$(PY) -m serving.matrix

golden-build:
	uv run -q python -m evals.build_golden

lab-up:
	$(KIND) create cluster --config lab/kind-config.yaml --wait 180s
	$(KUBECTL) --context kind-doctor-lab apply -f lab/metrics-server-v0.9.0.yaml
	$(KUBECTL) --context kind-doctor-lab -n kube-system patch deployment metrics-server --type=json \
	  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'

lab-record:
	uv run -q python -m lab.record --context kind-doctor-lab

lab-down:
	$(KIND) delete cluster --name doctor-lab

up:
	NAME=$(NAME) TYPES="$(TYPES)" lab/up.sh

status:
	lam ls --uptime
	$(REMOTE) kubectl get pods -A -o wide

gate:
	$(PY) -m serving.fit $(MODEL) $(TOPO) --gate

prefetch: gate render
	lam push $(RENDER)/ '~/deploy/' --delete
	$(REMOTE) sudo bash '~/deploy/prefetch.sh'

deploy: prefetch
	$(REMOTE) kubectl apply -f '~/deploy/k8s/'
	$(REMOTE) kubectl rollout status statefulset/vllm --timeout=20m

scale:
	@$(PY) -c "import json,sys; t=json.load(open('deploy/serving.json'))['topologies']['$(TOPO)']; 	  sys.exit(0 if $(N) <= t['max_replicas'] else f'TOPO=$(TOPO) allows at most {t[\"max_replicas\"]} replicas')"
	$(REMOTE) kubectl scale statefulset/vllm --replicas=$(N)
	$(REMOTE) kubectl rollout status statefulset/vllm --timeout=15m

logs:
	$(REMOTE) kubectl logs vllm-0 --tail=100

kv:
	@mkdir -p metrics
	$(REMOTE) "kubectl logs vllm-0 | grep -E 'GPU KV cache size|Maximum concurrency|Available KV cache memory'" \
	  | tee metrics/kv-$(MODEL)-$(TOPO)-$(STAMP).log

gateway-image:
	@mkdir -p $(GWBUILD)
	docker buildx build --platform linux/amd64,linux/arm64 -t $(GW_IMAGE):$(GW_TAG) --metadata-file $(GWBUILD)/image.json --push gateway
	@d=$$(python3 -c "import json; print(json.load(open('$(GWBUILD)/image.json'))['containerimage.digest'])") && \
	  sed -i.bak -E "s|image: .*cluster-doctor-gateway[@:][^ ]*|image: $(GW_IMAGE)@$$d|" deploy/k8s/gateway.yaml && rm -f deploy/k8s/gateway.yaml.bak && \
	  echo "pinned $(GW_IMAGE)@$$d in deploy/k8s/gateway.yaml ($(GW_TAG)); commit it"

gateway:
	@grep -q "cluster-doctor-gateway@sha256:[0-9a-f]\{64\}" deploy/k8s/gateway.yaml || (echo "no pinned gateway image: run make gateway-image"; exit 1)
	@mkdir -p $(GWBUILD)
	$(PY) -m serving.warmup $(MODEL) > $(GWBUILD)/warm.json
	cp deploy/k8s/gateway.yaml $(GWBUILD)/
	lam push $(GWBUILD)/ '~/gateway/' --delete
	$(REMOTE) "kubectl create configmap gateway-warmup --from-file=warm.json=\$$HOME/gateway/warm.json --dry-run=client -o yaml | kubectl apply -f - && \
	  kubectl apply -f \$$HOME/gateway/gateway.yaml && \
	  kubectl set env deployment/gateway GW_POLICY=$(POLICY) GW_POOL=$(TOPO) && \
	  kubectl rollout restart deployment/gateway && kubectl rollout status deployment/gateway --timeout=5m"

tunnel:
	@eval "$$(lam env $(NAME))" && echo "localhost:$(PORT) → $(TUNNEL) via $$LAMBDA (Ctrl-C to close)" && \
	  ssh -i "$$LAMBDA_SSH_KEY" "$$LAMBDA" "fuser -k -n tcp $(PORT) >/dev/null 2>&1 ; true" && \
	  ssh -tt -i "$$LAMBDA_SSH_KEY" -o ExitOnForwardFailure=yes -L $(PORT):127.0.0.1:$(PORT) "$$LAMBDA" \
	  kubectl port-forward $(TUNNEL) $(PORT):8000

dashboards:
	lam push deploy/observability/dashboards/ '~/dashboards/' --delete
	$(REMOTE) "kubectl -n monitoring create configmap doctor-dashboards --from-file=\$$HOME/dashboards \
	  --dry-run=client -o yaml | kubectl apply -f - && \
	  kubectl -n monitoring label configmap doctor-dashboards grafana_dashboard=1 --overwrite"

grafana:
	@eval "$$(lam env $(NAME))" && echo "Grafana: http://localhost:3000  user admin  password:" && \
	  ssh -i "$$LAMBDA_SSH_KEY" "$$LAMBDA" "kubectl -n monitoring get secret grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo" && \
	  ssh -i "$$LAMBDA_SSH_KEY" "$$LAMBDA" "fuser -k -n tcp 3000 >/dev/null 2>&1 ; true" && \
	  ssh -tt -i "$$LAMBDA_SSH_KEY" -o ExitOnForwardFailure=yes -L 3000:127.0.0.1:3000 "$$LAMBDA" \
	    kubectl -n monitoring port-forward svc/grafana 3000:80

demo:
	CONC=$(if $(filter 1,$(CONC)),8,$(CONC)) REPEAT=$(or $(REPEAT),1) lab/demo.sh

golden:
	uv run -q python -m evals.run_golden --base-url $(BASE) --profile $(MODEL) --topology $(TOPO) --workers $(WORKERS) \
	  --tag $(TAG) --concurrency $(CONC) $(if $(ONLY),--only $(ONLY)) $(if $(REPEAT),--repeat $(REPEAT)) $(if $(NOHARNESS),--no-harness)

metrics:
	@mkdir -p metrics
	curl -sf http://127.0.0.1:$(PORT)/metrics > metrics/$(SCRAPE)-$(TAG)-$(STAMP).prom
	@if [ $(SCRAPE) = gateway ]; then for p in $(PODS); do \
	  curl -sf http://127.0.0.1:$(PORT)/debug/workers/$$p/metrics > metrics/$$p-$(TAG)-$(STAMP).prom || rm -f metrics/$$p-$(TAG)-$(STAMP).prom; \
	done; fi

down:
	lam rm $(NAME)
	rm -f .cache/node.env .cache/ready.json
	lam ls

preflight: lint test
	cd gateway && CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o /dev/null ./cmd/gateway   # what make gateway ships
	uv run -q python -m evals.build_golden && git diff --quiet evals/golden || (echo "golden set changed — commit it"; exit 1)
	$(PY) -m serving.fit $(MODEL) $(TOPO) --gate
	@lam config | grep -q "LAMBDA_API_KEY: *set (" || (echo "lam has no LAMBDA_API_KEY: run lam config init"; exit 1)
	@grep -q "max-model-len=24576" deploy/k8s/vllm.yaml && echo "preflight ok — next: make up && make deploy && make kv"

sweep:
	@mkdir -p metrics
	for c in $(LEVELS); do \
	  uv run -q python -m evals.run_golden --base-url $(BASE) --profile $(MODEL) --topology $(TOPO) --workers $(WORKERS) \
	    --tag sweep-$(MODEL)-c$$c --concurrency $$c --repeat $(REPEAT) || true; \
	  curl -sf http://127.0.0.1:$(PORT)/metrics > metrics/$(SCRAPE)-sweep-c$$c-$(STAMP).prom || echo "no /metrics at level $$c"; \
	  if [ $(SCRAPE) = gateway ]; then for p in $(PODS); do \
	    curl -sf http://127.0.0.1:$(PORT)/debug/workers/$$p/metrics > metrics/$$p-sweep-c$$c-$(STAMP).prom || rm -f metrics/$$p-sweep-c$$c-$(STAMP).prom; \
	  done; fi; \
	done

kubeconfig:
	@mkdir -p .cache
	$(REMOTE) cat /home/ubuntu/.kube/config | sed -e 's#server: https://.*:6443#server: https://127.0.0.1:6443#' \
	  -e 's#name: default#name: lambda#g' -e 's#cluster: default#cluster: lambda#' -e 's#user: default#user: lambda#' \
	  -e 's#current-context: default#current-context: lambda#' > $(KCFG)
	@chmod 600 $(KCFG) && echo "wrote $(KCFG) (context lambda); run make k8s-tunnel in another terminal"

k8s-tunnel:
	@eval "$$(lam env $(NAME))" && echo "localhost:6443 → k3s API on $$LAMBDA (Ctrl-C to close)" && \
	  ssh -i "$$LAMBDA_SSH_KEY" -o ExitOnForwardFailure=yes -N -L 6443:127.0.0.1:6443 "$$LAMBDA"

record-live:
	KUBECONFIG=$(KCFG) uv run -q python -m lab.record --context lambda --live $(if $(ONLY),--only $(ONLY))

watch:
	@mkdir -p metrics
	$(KENV) DOCTOR_KUBECTL=$(KUBECTL) uv run -q python -m doctor watch --context $(CTX) --base-url $(BASE) --profile $(MODEL) \
	  --exclude $(WATCH_EXCLUDE) --out metrics/watch-$(STAMP).jsonl $(WATCH_ARGS)

watch-metrics:
	@mkdir -p metrics
	curl -sf http://127.0.0.1:9109/metrics | tee metrics/watch-$(STAMP).prom | grep -v '^#'

inject:
	$(KENV) uv run -q python -m lab.inject --context $(CTX) --faults $(FAULTS) --stagger $(STAGGER)

heal:
	$(KENV) uv run -q python -m lab.inject --context $(CTX) --clear

prom:
	@eval "$$(lam env $(NAME))" && echo "Prometheus: http://localhost:9090 (Ctrl-C to close)" && \
	  ssh -i "$$LAMBDA_SSH_KEY" "$$LAMBDA" "fuser -k -n tcp 9090 >/dev/null 2>&1 ; true" && \
	  ssh -tt -i "$$LAMBDA_SSH_KEY" -o ExitOnForwardFailure=yes -L 9090:127.0.0.1:9090 "$$LAMBDA" \
	    kubectl -n monitoring port-forward svc/prometheus-server 9090:80

opencost:
	@eval "$$(lam env $(NAME))" && echo "OpenCost API: http://localhost:9003/allocation/compute?window=1d&aggregate=namespace (Ctrl-C to close)" && \
	  ssh -i "$$LAMBDA_SSH_KEY" "$$LAMBDA" "fuser -k -n tcp 9003 >/dev/null 2>&1 ; true" && \
	  ssh -tt -i "$$LAMBDA_SSH_KEY" -o ExitOnForwardFailure=yes -L 9003:127.0.0.1:9003 "$$LAMBDA" \
	    kubectl -n opencost port-forward svc/opencost 9003:9003

faults:
	@uv run -q python -m lab.inject --list
