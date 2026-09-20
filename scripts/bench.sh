#!/usr/bin/env bash
# bench.sh runs the benchmark protocol from PLAN.md §8: start the mocker, run
# every available leg N times in one driver invocation, and print the medians
# and the gateway overhead delta.
#
# The gateway leg is included automatically as soon as cmd/gateway builds
# (M3). Until then this produces the direct-to-mocker baseline only.
#
# Overridable:
#   RPS DURATION WARMUP COOLDOWN RUNS CONCURRENCY STREAM
#   LATENCY JITTER TOKENS TPS MODEL MOCKER_PORT GATEWAY_PORT
#   BENCH_FLAGS   extra flags passed through to cmd/bench
set -euo pipefail

cd "$(dirname "$0")/.."

RPS=${RPS:-1000}
DURATION=${DURATION:-30s}
WARMUP=${WARMUP:-3s}
COOLDOWN=${COOLDOWN:-2s}
RUNS=${RUNS:-3}
CONCURRENCY=${CONCURRENCY:-256}
STREAM=${STREAM:-0}

LATENCY=${LATENCY:-50ms}
JITTER=${JITTER:-0}
TOKENS=${TOKENS:-200}
# PLAN.md §M2: pacing exists so `curl -N` is legible by eye. Leaving it on
# injects one timer wait per token into both legs and buries the microseconds
# being measured, so every overhead measurement runs unpaced.
TPS=${TPS:-0}
MODEL=${MODEL:-mock-small}

MOCKER_PORT=${MOCKER_PORT:-8081}
GATEWAY_PORT=${GATEWAY_PORT:-8080}

OUT=bench-out
mkdir -p "$OUT"

pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# rss prints a process's resident set size in MB.
rss() { ps -o rss= -p "$1" 2>/dev/null | awk '{printf "%.1f", $1/1024}'; }

# wait_healthy polls a URL until it answers or the budget runs out.
wait_healthy() {
  local url=$1 name=$2
  for _ in $(seq 1 100); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  echo "bench.sh: $name never became healthy at $url" >&2
  return 1
}

echo "bench.sh: building"
go build -o "$OUT/mocker" ./cmd/mocker
go build -o "$OUT/bench" ./cmd/bench

echo "bench.sh: starting mocker on :$MOCKER_PORT (latency=$LATENCY jitter=$JITTER tokens=$TOKENS tps=$TPS)"
"$OUT/mocker" -port "$MOCKER_PORT" -latency "$LATENCY" -jitter "$JITTER" \
  -tokens "$TOKENS" -tps "$TPS" >"$OUT/mocker.log" 2>&1 &
mocker_pid=$!
pids+=("$mocker_pid")
wait_healthy "http://127.0.0.1:$MOCKER_PORT/healthz" mocker

bench_args=(
  -target "http://127.0.0.1:$MOCKER_PORT"
  -rps "$RPS" -duration "$DURATION" -warmup "$WARMUP" -cooldown "$COOLDOWN"
  -runs "$RUNS" -concurrency "$CONCURRENCY" -model "$MODEL"
)
[ "$STREAM" = "1" ] && bench_args+=(-stream)

# The gateway leg joins the run the moment cmd/gateway has a build (M3).
gateway_pid=""
if compgen -G "cmd/gateway/*.go" >/dev/null; then
  echo "bench.sh: starting gateway on :$GATEWAY_PORT"
  go build -o "$OUT/gateway" ./cmd/gateway
  "$OUT/gateway" -port "$GATEWAY_PORT" -upstream "http://127.0.0.1:$MOCKER_PORT" \
    >"$OUT/gateway.log" 2>&1 &
  gateway_pid=$!
  pids+=("$gateway_pid")
  wait_healthy "http://127.0.0.1:$GATEWAY_PORT/healthz" gateway
  bench_args+=(-gateway "http://127.0.0.1:$GATEWAY_PORT")
else
  echo "bench.sh: gateway leg skipped — cmd/gateway has no build yet (arrives in M3)"
fi

# shellcheck disable=SC2086
"$OUT/bench" "${bench_args[@]}" ${BENCH_FLAGS:-} | tee "$OUT/last-run.txt"

echo
echo "server memory after the run (RSS):"
echo "  mocker   $(rss "$mocker_pid") MB"
[ -n "$gateway_pid" ] && echo "  gateway  $(rss "$gateway_pid") MB"
echo
echo "bench.sh: full output in $OUT/last-run.txt"
