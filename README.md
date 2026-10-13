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

## Quick start (mock replicas, no GPU)

Requires Go 1.23+.

```sh
make test                          # unit tests, with the race detector
make run-mocks                     # 4 mock replicas on :8001-:8004
make run-router POLICY=prefix-aware   # router on :8090
make stop

make bench                         # compare all three policies (~10 min)
```

Send a request through the router:

```sh
curl -s localhost:8090/v1/chat/completions \
  -d '{"messages":[{"role":"user","content":"hello"}],"max_tokens":8}' -D - -o /dev/null \
  | grep -i x-routed-to
```

## Real vLLM on Apple Silicon

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

## Results so far

All single runs; p99 varied about 15% between identical runs, so treat these
as indicative. Percentage changes are relative to round-robin.

**Real vLLM**: vllm-metal 0.30.0, 2 replicas of Qwen3-0.6B-4bit on one M3 Pro
GPU, 4,480-token KV cache each. Workload: 6-turn conversations, 4 shared
~200-token system prompts, open-loop arrivals.

| conversations/s | policy | TTFT p50 | TTFT p99 | hit rate |
|---|---|---|---|---|
| 1.0 | round-robin | 96 ms | 503 ms | 68.8% |
| 1.0 | least-load | 93 ms | 336 ms | 71.3% |
| 1.0 | **prefix-aware** | **68 ms** | **217 ms (−57%)** | **85.0%** |
| 1.25 | round-robin | 113 ms | 449 ms | 70.4% |
| 1.25 | least-load | 129 ms | 527 ms | 68.6% |
| 1.25 | **prefix-aware** | **82 ms** | **268 ms (−40%)** | **83.9%** |

**Mock replicas**: 4 replicas, cache sized for an L4 running Llama-3.1-8B
(2048 blocks), 8-turn conversations with 8 shared system prompts.

| conversations/s | round-robin TTFT p99 / hit | prefix-aware TTFT p99 / hit |
|---|---|---|
| 1 | 293 ms / 63.5% | 219 ms (−25%) / 84.6% |
| 2 | 405 ms / 60.9% | 354 ms (−13%) / 83.6% |
| 3 (baselines saturated) | 2667 ms / 55.8% | 1399 ms (−48%) / 83.5% |

What the runs show:
- The gain in tail latency grows with load: saved prefill matters most when
  requests are queueing.
- Even round-robin hits the cache often here, because shared system prompts
  end up cached on every replica. Workloads with less shared prefix would
  start from a lower baseline.
- Under overload, vLLM's hit rate drops for every policy (46% at 2
  conversations/s on the Mac), because running requests' KV blocks evict
  cached history.
- `-load-slack 4` was the best setting in a sweep of 1, 2, 4 and 8; 8 let hot
  spots form and nearly doubled p99.

## Router flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8090` | Listen address |
| `-backends` | `http://localhost:8001,…:8004` | Comma-separated vLLM base URLs |
| `-policy` | `round-robin` | `round-robin`, `least-load` or `prefix-aware` |
| `-block-chars` | `64` | Characters per KV block (~16 tokens) |
| `-cache-blocks` | `2048` | Shadow index size per replica, in blocks |
| `-load-slack` | `4` | Extra in-flight requests accepted to get a cache hit |

## Known limitations

- Prompts using OpenAI's content-array format (`"content": [{"type": "text", …}]`)
  aren't parsed; those requests are routed on load alone.
- The shadow index can drift from a replica's real cache, for example after a
  restart. It corrects itself as traffic is recorded.
- With several router replicas, each sees only its own share of in-flight
  load and keeps its own shadow index.

## Roadmap

- Repeated benchmark runs, reported with a spread
- Router Prometheus metrics and a Grafana dashboard
- Container images and Kubernetes manifests
- Terraform for a GKE cluster with an L4 GPU node pool, deployed with Argo CD
- Benchmarks on vLLM with Llama-3.1-8B on L4 GPUs
