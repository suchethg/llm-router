.PHONY: build test run-mocks run-router stop bench monitoring-up monitoring-down images kind-up kind-down

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

kind-up: images
	kind get clusters | grep -qx llm-router || kind create cluster --name llm-router --kubeconfig $(KUBECONFIG_KIND)
	kind load docker-image --name llm-router llm-router/router:dev llm-router/mockvllm:dev
	$(KUBECTL_KIND) apply -k deploy/k8s/overlays/local
	$(KUBECTL_KIND) rollout restart deployment/llm-router statefulset/vllm
	$(KUBECTL_KIND) rollout status statefulset/vllm --timeout=120s
	$(KUBECTL_KIND) rollout status deployment/llm-router --timeout=120s

kind-down:
	kind delete cluster --name llm-router --kubeconfig $(KUBECONFIG_KIND)
