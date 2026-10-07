# cluster-doctor: the local fault lab (kind), offline evaluation, and the Lambda GPU lifecycle (A100, H100 or GH200).
#
#   make tools                      fetch pinned kind, kubectl, promtool, golangci-lint, gitleaks into .bin/ (checksums verified)
#   make test / lint                offline: unit tests over recorded snapshots, gateway vet + race tests, alert rule tests;
#                                   ruff and golangci-lint (gateway/.golangci.yml)
#   make hooks                      use .githooks/: secrets + gateway fmt/vet/build before commit, lint + race tests before push (D-47)
#   make secrets / vulncheck        gitleaks over the whole history / govulncheck on the gateway (needs the network)
#   make lab-up / lab-record / lab-down   kind cluster, inject faults, record fixtures/  (lab only mutates kind)
#   make golden-build               rebuild evals/golden from faults/ + fixtures/ (references must pass)
#   make gateway-image              build + push the gateway image (Docker Hub, public), pin its digest
#   make up / deploy / kv / tunnel / grafana / dashboards / down   Lambda GPU node via the `lam` CLI: up takes the first of TYPES with capacity (lab/up.sh)
#   make scale [N=2]                workers on the topology's slices (at most its max_replicas)
#   make autoscale / autoscale-off  KEDA scales the workers 1..max_replicas on gateway demand (D-49); off keeps the count
#   make gateway [POLICY=least_loaded]   apply the pinned gateway image + warm-up body on the node and roll it out (D-42)
#   make deploy HOP=1 && make gateway HOP=1   workers run vLLM's MooncakeConnector and the gateway copies a moved
#                                   run's KV instead of recomputing it (gateway/internal/hop). node.env turns it on
#                                   for an H100 node; HOP=0 on the make line turns it off
#   make tunnel [TUNNEL=pod/vllm-0]      forward localhost:8000 to the gateway (default) or to one vLLM pod
#   make demo                       laptop only: two fake vLLM workers behind the gateway, golden set at concurrency 8
#   make models / fit [MODEL=… TOPO=…] / fit-all   model profiles (deploy/models) and whether each fits each topology (D-40)
#   make deploy MODEL=… TOPO=…      render, fetch weights, serve that model on that topology (fit gate first)
#   make golden TAG=… [BASE=…]      run the golden set against a model endpoint (vLLM or the gateway); v1 + v2 scores
#   make matrix                     design/model-matrix.md: fit + results + ranking from deploy/ and metrics/
#   make report                     report/report.ipynb + design/figures/: the results notebook, rebuilt from metrics/
#   make sweep [LEVELS="1 4 8 16 32"] REPEAT=2   golden set at each concurrency + a /metrics scrape per level (gateway + pods)
#   make preflight                  everything that must be true before paying for a GPU
#   make kubeconfig / k8s-tunnel / record-live ONLY=gpu-unavailable   live-only faults on the Lambda k3s cluster
#   make watch [WATCH_ARGS=…]       the doctor, autonomous, against CTX (default lambda); /metrics on :9109
#   make inject FAULTS=… [STAGGER=60] / heal   break the lab cluster on purpose / remove what inject created
#   make faults                     list injectable faults by tier
#   make prom / opencost            port-forward Prometheus (:9090) / the OpenCost API (:9003) from the Lambda node
# ---- playbook: a GPU session in order (make help prints this) ------------------------------------------------
#   make preflight && make up       lint + tests + fit gate, then the first GPU with capacity; prints the node it got
#   make resume                     if make up timed out while Lambda was still booting the node, wait for it and finish
#   make bringup                    deploy → scale to the topology's workers → kv → gateway → dashboards
#   make tunnel                     terminal 2: the gateway at localhost:8000
#   make grafana                    terminal 3: Grafana at localhost:3000
#   make check                      pods, both workers Ready through the gateway, KV hop on or off
#   make bench TAG=…                golden set + concurrency sweep + metrics + queue probes + export, through the gateway
#                                   (PROBES=0 skips the probes; make export alone re-pulls the time series, D-48)
#   make gateway POLICY=least_loaded && make bench TAG=…-ll   the control arm of the stickiness A/B
#   git add metrics && git commit   before make down. The node's Prometheus keeps nothing (make down warns if the
#                                   last bench was never exported)
#   make down                       billing stops
# `make up` writes the node it got (GPU, ARCH, MODEL, TOPO, HOP) here; anything on the make line still wins.
-include .cache/node.env
NAME   ?= cluster-doctor
TAG    ?= baseline
N      ?= 2
PORT   ?= 8000
MODEL  ?= qwen3-8b-awq
TOPO   ?= sliced
ARCH   ?= amd64
TYPES  ?= gpu_1x_h100_pcie gpu_1x_h100_sxm5 gpu_1x_gh200 gpu_1x_a100_sxm4
WORKERS ?= 1
BASE   ?= http://127.0.0.1:$(PORT)/v1
CONC   ?= 1
LEVELS ?= 1 4 8 16 32
REPEAT ?= 2
CTX    ?= lambda
TUNNEL ?= svc/gateway
POLICY ?= prefix_then_load
PROBES ?= 1
HOP    ?= 0
MAXREP  = $(shell $(PY) -c "import json; print(json.load(open('deploy/serving.json'))['topologies']['$(TOPO)']['max_replicas'])")
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
# The in-flight cap and the KV hop's rates for the deployed model and topology, from the fit (D-43). The fit uses
# measured KV once make kv has run.
GWENV   = $(shell $(PY) -m serving.fit $(MODEL) $(TOPO) --gateway-env)
GW_IMAGE ?= docker.io/cdugga/cluster-doctor-gateway
GW_TAG  = $(shell git rev-parse --short HEAD)$(shell git diff --quiet HEAD -- gateway || echo -dirty)
SCRAPE  = $(if $(filter svc/gateway,$(TUNNEL)),gateway,vllm)
PODS    = vllm-0 vllm-1

.PHONY: help bringup check bench resume alerts-test report demo gateway gateway-image models fit fit-all gate render prefetch matrix tools preflight sweep kubeconfig k8s-tunnel record-live watch watch-metrics inject heal prom opencost faults test lint go-lint hooks secrets vulncheck golden-build lab-up lab-record lab-down up status deploy scale autoscale autoscale-off logs kv tunnel dashboards grafana golden metrics down probes export

.DEFAULT_GOAL := help

help:
	@sed -n '/^# ---- playbook/,/^# .make up. writes/p' Makefile | sed '$$d' | sed 's/^# \{0,1\}//'
	@echo "Node: GPU=$(GPU) MODEL=$(MODEL) TOPO=$(TOPO) HOP=$(HOP)   (.cache/node.env, or the defaults)"

# One worker per slice of the topology (2 on sliced and the halves, 1 on a whole card), so the gateway, the A/B and
# the KV hop always have workers to route between. kv needs vllm-0 up, which scale's rollout status waits for.
bringup:
	$(MAKE) deploy
	@if [ "$(MAXREP)" -gt 1 ]; then $(MAKE) scale N=$(MAXREP); else echo "TOPO=$(TOPO) has one worker; not scaling"; fi
	$(MAKE) kv
	$(MAKE) gateway
	$(MAKE) dashboards

check:
	$(REMOTE) "kubectl get pods -o wide; kubectl logs deploy/gateway 2>/dev/null | grep -m1 -o 'kv hop on' || echo 'kv hop: off'"
	@curl -sf http://127.0.0.1:$(PORT)/debug/workers | python3 -c 'import json,sys; [print(w["pod"], w["phase"]) for w in json.load(sys.stdin)]' \
	  || echo "no gateway at localhost:$(PORT): start make tunnel in another terminal"

bench:
	@test "$(TAG)" != baseline || (echo "set TAG=… to name this run"; exit 1)
	@mkdir -p .cache && printf '{"tag": "%s", "start": %s}\n' "$(TAG)" "$$(date +%s)" > .cache/bench.json
	$(MAKE) golden WORKERS=$(MAXREP) CONC=4 REPEAT=2 TAG=$(TAG)
	$(MAKE) sweep WORKERS=$(MAXREP)
	$(MAKE) metrics TAG=$(TAG)
	$(if $(filter 1,$(PROBES)),$(MAKE) probes TAG=$(TAG))
	$(MAKE) export

# Part 5 of the brief, measured (D-48): a golden run at concurrency 4 as background load, with three ~14k-token batch
# prompts, three clients that leave mid-request, and one worker deleted and left to return. Each probe's time goes to
# metrics/events-*.jsonl; make export pairs them with the node's time series.
# The load command lives in a variable: a recipe line that names $(MAKE) directly runs even under make -n, which would
# start real probes (and delete a worker) during a dry run.
PROBE_LOAD = $(MAKE) golden WORKERS=$(MAXREP) CONC=4 REPEAT=2 TAG=$(TAG)-probes
probes:
	$(PY) -m lab.probes session --tag $(TAG)-probes --profile $(MODEL) --base-url $(BASE) --node $(NAME) \
	  --load-cmd "$(PROBE_LOAD)"

# The node's Prometheus history over the last bench window (.cache/bench.json) as metrics/ts-*.json, before make down
# destroys it. SINCE=<unix seconds> sets the window by hand; PROM=http://127.0.0.1:9090 reads through make prom.
export:
	$(PY) -m lab.export --node $(NAME) $(if $(SINCE),--start $(SINCE)) $(if $(PROM),--prom-url $(PROM))

tools:
	bash lab/get-tools.sh

test: alerts-test
	uv run -q python -m pytest
	cd gateway && go vet ./... && go test -race ./...

# The results notebook and its charts (D-46), rebuilt from metrics/: report/report.ipynb, design/figures/*.png.
report:
	uv run -q --group report python -m report.build

# The production alert rules (D-45): valid, and each fires on its condition and stays quiet below it.
alerts-test: .bin/promtool
	.bin/promtool check rules deploy/observability/alerts.yaml
	cd deploy/observability && ../../.bin/promtool test rules alerts_test.yaml

.bin/promtool:
	bash lab/get-tools.sh promtool

lint: go-lint
	$(RUFF) check doctor evals lab serving tests report

# Code-quality gates (D-47). The hooks call the same tools; CI runs them too, so --no-verify only delays a failure.
GOVULNCHECK_VERSION = v1.8.0
go-lint: .bin/golangci-lint
	cd gateway && ../.bin/golangci-lint run ./...

hooks: .bin/golangci-lint .bin/gitleaks
	git config core.hooksPath .githooks
	@echo "hooks on: .githooks/pre-commit, .githooks/pre-push"

secrets: .bin/gitleaks
	.bin/gitleaks git --no-banner --redact --config .gitleaks.toml

vulncheck:
	cd gateway && go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.bin/golangci-lint .bin/gitleaks:
	bash lab/get-tools.sh $(notdir $@)

models:
	$(PY) -m serving.profiles list

fit:
	$(PY) -m serving.fit $(MODEL) $(TOPO)

fit-all:
	$(PY) -m serving.fit all

render:
	$(PY) -m serving.profiles render $(MODEL) $(TOPO) --out $(RENDER) $(if $(filter 1,$(HOP)),--hop)

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

resume:
	NAME=$(NAME) lab/up.sh --resume

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
	@$(REMOTE) "! kubectl get scaledobject vllm >/dev/null 2>&1" || (echo "KEDA owns the replica count: make autoscale-off first"; exit 1)
	@$(PY) -c "import json,sys; t=json.load(open('deploy/serving.json'))['topologies']['$(TOPO)']; 	  sys.exit(0 if $(N) <= t['max_replicas'] else f'TOPO=$(TOPO) allows at most {t[\"max_replicas\"]} replicas')"
	$(REMOTE) kubectl scale statefulset/vllm --replicas=$(N)
	$(REMOTE) kubectl rollout status statefulset/vllm --timeout=15m

# KEDA scaling of the vLLM workers (D-49): demand / per-worker cap, plus capacity sheds. The cap is the gateway's
# GW_MAX_INFLIGHT for this model and topology, so one formula drives admission and scaling.
WORKER_CAP = $(patsubst GW_MAX_INFLIGHT=%,%,$(filter GW_MAX_INFLIGHT=%,$(GWENV)))
autoscale:
	@[ "$(MAXREP)" -gt 1 ] || (echo "TOPO=$(TOPO) has one worker slot; nothing to scale"; exit 1)
	@mkdir -p .cache/autoscale
	sed -e 's/__MAX_REPLICAS__/$(MAXREP)/' -e 's/__WORKER_CAP__/$(WORKER_CAP)/' deploy/autoscale/keda-vllm.yaml > .cache/autoscale/keda-vllm.yaml
	lam push .cache/autoscale/ '~/autoscale/' --delete
	$(REMOTE) "kubectl -n keda rollout status deployment/keda-operator --timeout=2m && kubectl apply -f \$$HOME/autoscale/keda-vllm.yaml && \
	  sleep 20 && kubectl get scaledobject vllm && kubectl get hpa keda-hpa-vllm"

autoscale-off:
	$(REMOTE) "kubectl delete scaledobject vllm --ignore-not-found && kubectl get statefulset vllm"

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
	  kubectl set env deployment/gateway GW_POLICY=$(POLICY) GW_POOL=$(TOPO) GW_HOP=$(if $(filter 1,$(HOP)),true,false) $(GWENV) && \
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
	@if [ -f .cache/bench.json ] && ! grep -q '"exported"' .cache/bench.json; then \
	  echo "WARNING: the last make bench was never exported, and its time series die with the node. Run make export first."; \
	  echo "Continuing in 15 s (Ctrl-C to stop)."; sleep 15; fi
	lam rm $(NAME)
	rm -f .cache/node.env .cache/ready.json .cache/bench.json
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
