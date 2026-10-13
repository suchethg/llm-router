#!/usr/bin/env bash
# Start N vllm-metal replicas on the Mac GPU, on ports 8001.., one at a time
# (loading several models at once spikes memory). Logs go to bin/vllm-<port>.log.
#   scripts/vllm-mac.sh [N]     default 2, which is what fits in 18 GB
set -euo pipefail
cd "$(dirname "$0")/.."
N=${1:-2}
MODEL=${MODEL:-mlx-community/Qwen3-0.6B-4bit}
VLLM_ARGS=${VLLM_ARGS:---enable-prefix-caching --gpu-memory-utilization 0.15 --max-num-seqs 8 --max-num-batched-tokens 1024 --max-model-len 4096}
mkdir -p bin
for port in $(seq 8001 $((8000 + N))); do
  # Loopback only: Docker Desktop forwards host.docker.internal from kind pods
  # to it, so there's no need to expose vLLM to the network.
  nohup vllm serve "$MODEL" --host 127.0.0.1 --port "$port" $VLLM_ARGS > "bin/vllm-$port.log" 2>&1 &
  pid=$!
  until curl -sf -o /dev/null "http://127.0.0.1:$port/health"; do
    kill -0 $pid 2>/dev/null || { echo "replica on :$port exited; see bin/vllm-$port.log" >&2; exit 1; }
    sleep 1
  done
  echo "vllm replica ready on :$port"
done
