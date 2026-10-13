#!/usr/bin/env bash
# Compare routing policies on the same workload. Each policy gets freshly
# started mock replicas, so every run begins with cold caches.
#
#   scripts/bench.sh                         # all three policies
#   scripts/bench.sh prefix-aware            # just one
#   LOADGEN_ARGS="-rate 3" ROUTER_ARGS="-load-slack 2" scripts/bench.sh
set -euo pipefail
cd "$(dirname "$0")/.."

[ $# -gt 0 ] || set -- round-robin least-load prefix-aware
OUT=${OUT:-bench-results}
mkdir -p "$OUT"
go build -o bin/ ./cmd/...

pids=()
stop() { kill ${pids[@]+"${pids[@]}"} 2>/dev/null || true; wait 2>/dev/null || true; pids=(); }
trap stop EXIT
wait_up() { curl -s -o /dev/null --retry 50 --retry-connrefused --retry-delay 0 --retry-max-time 10 "$1"; }

for p in "$@"; do
  for i in 1 2 3 4; do
    ./bin/mockvllm -addr "127.0.0.1:800$i" -name "mock-$i" ${MOCK_ARGS:-} 2>/dev/null & pids+=($!)
  done
  ./bin/router -addr 127.0.0.1:8090 -policy "$p" ${ROUTER_ARGS:-} 2>/dev/null & pids+=($!)
  for i in 1 2 3 4; do wait_up "http://127.0.0.1:800$i/health"; done
  wait_up http://127.0.0.1:8090/healthz

  echo "--- $p"
  ./bin/loadgen -target http://127.0.0.1:8090 -label "$p" -json "$OUT/$p.json" \
    -metrics "$(printf 'http://127.0.0.1:800%s/metrics,' 1 2 3 4)" ${LOADGEN_ARGS:-}
  stop
done

echo
./bin/loadgen report $(printf "$OUT/%s.json " "$@")
