.PHONY: build test run-mocks run-router stop bench

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
