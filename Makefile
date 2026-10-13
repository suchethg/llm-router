.PHONY: build test run-mocks run-router stop bench monitoring-up monitoring-down images kind-up kind-down run-vllm-mac tf-up tf-down

build:
	go build -o bin/ ./cmd/...

test:
	go test -race ./...

# Four mock vLLM replicas on :8001-:8004, in the background.
run-mocks: build
	for i in 1 2 3 4; do ./bin/mockvllm -addr :800$$i -name mock-$$i & done

POLICY ?= round-robin

run-router: build
	./bin/router -policy $(POLICY)

stop:
	-pkill -f bin/mockvllm
	-pkill -f bin/router
	-pkill -f "vllm serve"

# Compare all policies on the same multi-turn workload (fresh mocks each).
bench:
	scripts/bench.sh

# Prometheus (:9090) and Grafana (:3000) in Docker, scraping the host.
monitoring-up:
	docker compose -f deploy/local/docker-compose.yml up -d

monitoring-down:
	docker compose -f deploy/local/docker-compose.yml down

# Container images for every program in cmd/.
images:
	for c in router mockvllm loadgen; do docker build --build-arg CMD=$$c -t llm-router/$$c:dev . ; done

# Local Kubernetes: a dedicated kind cluster running the router and 4 mocks.
# It gets its own kubeconfig, so ~/.kube/config and your current context are
# never touched.
KUBECONFIG_KIND = bin/kubeconfig
KUBECTL_KIND = kubectl --kubeconfig $(KUBECONFIG_KIND) -n llm-router

# OVERLAY=local runs 4 mocks in the cluster; OVERLAY=mac-gpu points the router
# at vllm-metal replicas on this Mac (start them with `make run-vllm-mac`).
OVERLAY ?= local

kind-up: images
	kind get clusters | grep -qx llm-router || kind create cluster --name llm-router --kubeconfig $(KUBECONFIG_KIND)
	kind load docker-image --name llm-router llm-router/router:dev llm-router/mockvllm:dev
	$(KUBECTL_KIND) apply -k deploy/k8s/overlays/$(OVERLAY)
	$(KUBECTL_KIND) rollout restart deployment/llm-router
	$(KUBECTL_KIND) rollout status deployment/llm-router --timeout=120s
	# Only the local overlay has in-cluster replicas to wait for.
	if $(KUBECTL_KIND) get statefulset/vllm >/dev/null 2>&1; then $(KUBECTL_KIND) rollout status statefulset/vllm --timeout=120s; fi

kind-down:
	kind delete cluster --name llm-router --kubeconfig $(KUBECONFIG_KIND)

# vLLM replicas on the Mac GPU (vllm-metal), ports 8001-8002. `make stop` ends them.
run-vllm-mac:
	scripts/vllm-mac.sh 2

# On-prem via Terraform: kind cluster + Argo CD + root Application, then the
# locally built images (Terraform doesn't build images). Argo CD deploys the
# rest from git. Separate from kind-up: both use the cluster name llm-router.
TF = terraform -chdir=deploy/terraform/onprem

tf-up: images
	$(TF) init -input=false
	$(TF) apply -input=false
	kind load docker-image --name llm-router llm-router/router:dev llm-router/mockvllm:dev

tf-down:
	$(TF) destroy -input=false
