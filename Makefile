# cluster-doctor — local fault lab (kind), offline evaluation, and the Lambda A100 lifecycle.
#
#   make tools                      fetch pinned kind + kubectl into .bin/ (checksums verified)
#   make test / lint                offline: unit tests over recorded snapshots, ruff
#   make lab-up / lab-record / lab-down   kind cluster, inject faults, record fixtures/  (lab only mutates kind)
#   make golden-build               rebuild evals/golden from faults/ + fixtures/ (references must pass)
#   make up / deploy / kv / tunnel / grafana / dashboards / down   Lambda A100 via the `lam` CLI (see SPEC §7)
#   make golden TAG=… [BASE=…]      run the golden set against a model endpoint (vLLM or the gateway)
NAME   ?= cluster-doctor
TAG    ?= baseline
N      ?= 2
PORT   ?= 8000
MODEL  ?= Qwen/Qwen3-8B-AWQ
BASE   ?= http://127.0.0.1:$(PORT)/v1
CONC   ?= 1
STAMP  := $(shell date +%Y%m%d-%H%M%S)
KUBECTL := $(CURDIR)/.bin/kubectl
KIND    := $(CURDIR)/.bin/kind
REMOTE  = lam ssh $(NAME) --
RUFF    = uvx -q ruff@0.13.2

.PHONY: tools test lint golden-build lab-up lab-record lab-down up status deploy scale logs kv tunnel dashboards grafana golden metrics down

tools:
	bash lab/get-tools.sh

test:
	uv run -q --with pytest python -m pytest

lint:
	$(RUFF) check doctor evals lab tests

golden-build:
	python3 -m evals.build_golden

lab-up:
	$(KIND) create cluster --config lab/kind-config.yaml --wait 180s
	$(KUBECTL) --context kind-doctor-lab apply -f lab/metrics-server-v0.9.0.yaml
	$(KUBECTL) --context kind-doctor-lab -n kube-system patch deployment metrics-server --type=json \
	  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'

lab-record:
	python3 -m lab.record --context kind-doctor-lab

lab-down:
	$(KIND) delete cluster --name doctor-lab

up:
	lam launch -c deploy/cloud-init.yaml --name $(NAME) --retry 30m
	$(REMOTE) cat /var/lib/bootstrap/ready.json

status:
	lam ls --uptime
	$(REMOTE) kubectl get pods -A -o wide

deploy:
	lam push deploy/k8s/ '~/k8s/' --delete
	$(REMOTE) kubectl apply -f '~/k8s/'
	$(REMOTE) kubectl rollout status statefulset/vllm --timeout=15m

scale:
	$(REMOTE) kubectl scale statefulset/vllm --replicas=$(N)
	$(REMOTE) kubectl rollout status statefulset/vllm --timeout=15m

logs:
	$(REMOTE) kubectl logs vllm-0 --tail=100

kv:
	$(REMOTE) "kubectl logs vllm-0 | grep -E 'GPU KV cache size|Maximum concurrency|Available KV cache memory'"

tunnel:
	@eval "$$(lam env $(NAME))" && echo "localhost:$(PORT) → vllm-0 via $$LAMBDA (Ctrl-C to close)" && \
	  ssh -i "$$LAMBDA_SSH_KEY" "$$LAMBDA" "fuser -k -n tcp $(PORT) >/dev/null 2>&1 ; true" && \
	  ssh -tt -i "$$LAMBDA_SSH_KEY" -o ExitOnForwardFailure=yes -L $(PORT):127.0.0.1:$(PORT) "$$LAMBDA" \
	  kubectl port-forward pod/vllm-0 $(PORT):8000

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

golden:
	python3 -m evals.run_golden --base-url $(BASE) --model $(MODEL) --tag $(TAG) --concurrency $(CONC) \
	  $(if $(ONLY),--only $(ONLY)) $(if $(REPEAT),--repeat $(REPEAT)) $(if $(NOHARNESS),--no-harness)

metrics:
	@mkdir -p metrics
	curl -sf http://127.0.0.1:$(PORT)/metrics > metrics/vllm-$(TAG)-$(STAMP).prom

down:
	lam rm $(NAME)
	lam ls
