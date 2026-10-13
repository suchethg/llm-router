# llm-router

A KV-cache-aware load balancer for [vLLM](https://github.com/vllm-project/vllm) replicas, written in Go.

Multi-turn chat resends the whole conversation on every turn. vLLM's automatic
prefix caching can skip recomputing that history, but only on the replica that
already holds it. A cache-blind load balancer such as round-robin scatters a
conversation's turns across replicas, so most turns pay full prefill again.
This router sends each request to the replica most likely to have its prompt
prefix cached, as long as that replica isn't overloaded.

## How it works

```
client ──► router ──► replica 1 (vLLM)
             │   ├──► replica 2
             │   └──► ...
             └─ per replica: shadow index of KV blocks sent there + in-flight count
```

1. **Prefix keys like vLLM's.** The prompt is split into fixed-size blocks, and
   each block is keyed by a hash chained over every block before it, the same
   scheme vLLM uses for automatic prefix caching. Block *i* is only reusable
   when the whole prompt up to block *i* matches (`internal/prefix`).
2. **Shadow index.** The router can't see replica caches, so it keeps an LRU
   per replica of the block hashes it has sent there, sized like the replica's
   real KV cache.
3. **Routing rule** (`internal/policy`, `prefix-aware`): only replicas within
   `-load-slack` in-flight requests of the least-loaded one are eligible.
   Among those, pick the one with the longest cached prefix; ties go to lower
   load. With no overlap anywhere, this falls back to least-load, which
   spreads out new conversations.
4. **Load** is the router's own count of in-flight requests (held until the
   response finishes streaming), not vLLM's scraped metrics, which are
   seconds stale and cause herding.

The router has no tokenizer, so blocks are measured in characters (64 chars ≈
16 tokens). Its estimate of what a replica has cached is approximate. It only
needs to rank replicas correctly; reported hit rates always come from vLLM's
own metrics.

## Components

| Path | What it is |
|---|---|
| `cmd/router` | The router: OpenAI-compatible `POST /v1/chat/completions` proxy with streaming, `GET /healthz` |
| `internal/policy` | `round-robin` and `least-load` (baselines), `prefix-aware` |
| `internal/prefix` | Chat rendering, chained block hashing, LRU |
| `internal/backend` | Replica list, per-replica reverse proxy, in-flight counter |
| `cmd/mockvllm` | CPU-only vLLM stand-in: same API and metric names, LRU prefix cache, prefill cost only for uncached blocks, `--max-num-seqs` queueing |
| `cmd/loadgen` | Open-loop multi-turn chat load generator; reports TTFT percentiles and prefix-cache hit rate from replica metrics |
| `scripts/bench.sh` | Runs each policy on freshly started replicas (cold caches) with the same workload and prints a comparison |
| `deploy/k8s` | Kustomize manifests: router base; overlays for in-cluster mocks (`local`), Mac GPU replicas (`mac-gpu`) and NVIDIA GPU nodes (`gpu`) |
| `deploy/terraform/onprem` | Terraform: kind cluster, Argo CD and the root Application |
| `deploy/argocd` | Argo CD Applications: router, Prometheus, Grafana, dashboards |
| `deploy/local` | Prometheus and Grafana in Docker Compose, for running without Kubernetes |

## Setup

### Mock replicas (no GPU)

Requires Go 1.23+.

```sh
make test                          # unit tests, with the race detector
make run-mocks                     # 4 mock replicas on :8001-:8004
make run-router POLICY=prefix-aware   # router on :8090
make stop

make bench                         # benchmark all three policies (~10 min)
```

Send a request through the router:

```sh
curl -s localhost:8090/v1/chat/completions \
  -d '{"messages":[{"role":"user","content":"hello"}],"max_tokens":8}' -D - -o /dev/null \
  | grep -i x-routed-to
```

### Real vLLM on Apple Silicon

[vllm-metal](https://docs.vllm.ai/projects/vllm-metal) runs vLLM on a Mac's GPU
through MLX/Metal, with the same OpenAI API, prefix caching and Prometheus
metrics, so the router and load generator work unchanged.

```sh
brew tap vllm-project/vllm-metal https://github.com/vllm-project/vllm-metal
brew install vllm-project/vllm-metal/vllm-metal

BACKEND=vllm REPLICAS=2 ROUTER_ARGS="-cache-blocks 280" \
LOADGEN_ARGS="-rate 1 -sys-prompts 4 -sys-chars 800 -turns 6 -user-chars 200 -max-tokens 48 -think 1s" \
  scripts/bench.sh
```

Defaults start `mlx-community/Qwen3-0.6B-4bit` with settings that fit two
replicas on an 18 GB Mac (each replica uses about 3.7 GB). Set
`-cache-blocks` to the replica's KV cache size in tokens ÷ 16; vLLM logs it
at startup as `KV cache size: N tokens`.

### On-prem Kubernetes (Terraform + Argo CD)

The router runs in a local [kind](https://kind.sigs.k8s.io) cluster and is
deployed by Argo CD from this repository; inference runs on the Mac's GPU.
Containers on a Mac can't use the Metal GPU, so the vLLM replicas run
natively and the cluster reaches them through `host.docker.internal`.

```
Terraform ──► kind cluster ──► Argo CD ──(syncs deploy/argocd from GitHub)──► router, Prometheus, Grafana
                                                       router ──► vllm-metal replicas on the Mac GPU
```

Requires Docker, kind, Terraform and vllm-metal (see above).

```sh
make run-vllm-mac   # 2 vLLM replicas on :8001-:8002
make tf-up          # kind cluster + Argo CD + root Application (asks for confirmation)
```

Argo CD then deploys everything in `deploy/argocd/` and keeps it in sync
with `main`. The cluster has its own kubeconfig, so your default `kubectl`
context is left alone:

```sh
export KUBECONFIG=$PWD/bin/kubeconfig
kubectl -n argocd get applications                             # all Synced/Healthy
kubectl -n llm-router port-forward svc/llm-router 18090:8090   # the router
kubectl -n monitoring port-forward svc/grafana 13000:80        # dashboard: http://localhost:13000
kubectl -n argocd port-forward svc/argocd-server 8443:80       # Argo CD UI: http://localhost:8443
```

Tear down with `make tf-down` and `make stop`.

`deploy/argocd/router.yaml` chooses where inference runs by its `path`:

| path | inference |
|---|---|
| `deploy/k8s/overlays/mac-gpu` (default) | vllm-metal replicas on the Mac GPU |
| `deploy/k8s/overlays/local` | 4 mock replicas inside the cluster; no GPU needed |
| `deploy/k8s/overlays/gpu` | 4 vLLM replicas of Qwen3-8B on Kubernetes nodes with NVIDIA GPUs |

### Monitoring without Kubernetes

The router exports Prometheus metrics on `/metrics`. When the router runs
directly on the machine (not in the cluster), view them with the replicas'
vLLM metrics in Grafana (requires Docker):

```sh
make monitoring-up     # Grafana http://localhost:3000, Prometheus http://localhost:9090
make monitoring-down
```

The `llm-router` dashboard opens without a login. It shows requests per
replica, routing reasons, the router's predicted prefix-cache hit rate next to
vLLM's actual one, time to first token, per-replica load and KV cache usage,
and the router's own overhead.

### Router flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8090` | Listen address |
| `-backends` | `http://localhost:8001,…:8004` | Comma-separated vLLM base URLs |
| `-policy` | `round-robin` | `round-robin`, `least-load` or `prefix-aware` |
| `-block-chars` | `64` | Characters per KV block (~16 tokens) |
| `-cache-blocks` | `2048` | Shadow index size per replica, in blocks |
| `-load-slack` | `4` | Extra in-flight requests accepted to get a cache hit |
