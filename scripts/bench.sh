#!/usr/bin/env bash
# Compare routing policies on the same workload. Each policy gets freshly
# started replicas, so every run begins with cold caches.
#
#   scripts/bench.sh                         # all three policies, 4 mocks
#   scripts/bench.sh prefix-aware            # just one
#   LOADGEN_ARGS="-rate 3" ROUTER_ARGS="-load-slack 2" scripts/bench.sh
#   BACKEND=vllm REPLICAS=2 scripts/bench.sh # real vLLM (vllm-metal on a Mac)
#
# With BACKEND=vllm, ROUTER_ARGS should include -cache-blocks matching the
# replicas' real KV cache (vLLM logs "KV cache size: N tokens"; blocks are
# N/16), or the router assumes replicas remember more than they do.
set -euo pipefail
cd "$(dirname "$0")/.."

[ $# -gt 0 ] || set -- round-robin least-load prefix-aware
BACKEND=${BACKEND:-mock}
REPLICAS=${REPLICAS:-4}
MODEL=${MODEL:-mlx-community/Qwen3-0.6B-4bit}
# Settings that fit 2 replicas of the 4-bit 0.6B model on an 18 GB Mac.
VLLM_ARGS=${VLLM_ARGS:---enable-prefix-caching --gpu-memory-utilization 0.15 --max-num-seqs 8 --max-num-batched-tokens 1024 --max-model-len 4096}
OUT=${OUT:-bench-results}
mkdir -p "$OUT" "$OUT/logs"
go build -o bin/ ./cmd/...

ports=$(seq 8001 $((8000 + REPLICAS)))
join() { local sep=$1; shift; local IFS=$sep; echo "$*"; }
backends=$(join , $(for p in $ports; do echo "http://127.0.0.1:$p"; done))
metrics=$(join , $(for p in $ports; do echo "http://127.0.0.1:$p/metrics"; done))

pids=()
stop() { kill ${pids[@]+"${pids[@]}"} 2>/dev/null || true; wait 2>/dev/null || true; pids=(); }
trap stop EXIT

# wait_up URL PID SECONDS: poll until URL answers, failing early if PID dies.
wait_up() {
  local deadline=$((SECONDS + $3))
  until curl -sf -o /dev/null "$1"; do
    kill -0 "$2" 2>/dev/null || { echo "process for $1 exited; see $OUT/logs" >&2; exit 1; }
    [ $SECONDS -lt $deadline ] || { echo "timed out waiting for $1" >&2; exit 1; }
    sleep 0.5
  done
}

start_replicas() {
  for p in $ports; do
    if [ "$BACKEND" = vllm ]; then
      vllm serve "$MODEL" --host 127.0.0.1 --port "$p" $VLLM_ARGS > "$OUT/logs/vllm-$p.log" 2>&1 &
    else
      ./bin/mockvllm -addr "127.0.0.1:$p" -name "mock-$p" ${MOCK_ARGS:-} 2>/dev/null &
    fi
    pids+=($!)
    # vLLM replicas start one at a time: loading several models at once
    # spikes memory past what a laptop has free.
    [ "$BACKEND" = vllm ] && wait_up "http://127.0.0.1:$p/health" $! 300
  done
  for i in "${!pids[@]}"; do
    wait_up "http://127.0.0.1:$((8001 + i))/health" "${pids[$i]}" 30
  done
}

for policy in "$@"; do
  start_replicas
  ./bin/router -addr 127.0.0.1:8090 -backends "$backends" -policy "$policy" ${ROUTER_ARGS:-} 2>/dev/null &
  pids+=($!)
  wait_up http://127.0.0.1:8090/healthz $! 10

  echo "--- $policy ($BACKEND x$REPLICAS)"
  model=()
  [ "$BACKEND" = vllm ] && model=(-model "$MODEL")
  ./bin/loadgen -target http://127.0.0.1:8090 -label "$policy" -json "$OUT/$policy.json" \
    -metrics "$metrics" ${model[@]+"${model[@]}"} ${LOADGEN_ARGS:-}
  stop
done

echo
./bin/loadgen report $(printf "$OUT/%s.json " "$@")
