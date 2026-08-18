#!/usr/bin/env bash
# Benchmarks traefik with and without the open-appsec attachment.
#
# Both measurements hit the same traefik instance and the same backend; the
# only difference is the middleware, so the delta is the cost of inspection.
# The result is written as a markdown table to stdout and, when running in
# GitHub Actions, appended to the job summary.
set -euo pipefail
cd "$(dirname "$0")"

APPSEC_URL=${APPSEC_URL:-http://127.0.0.1:8080}
BASELINE_URL=${BASELINE_URL:-http://127.0.0.1:8081}
REQUESTS=${BENCH_REQUESTS:-2000}
WARMUP_REQUESTS=${BENCH_WARMUP_REQUESTS:-200}
OHA_IMAGE=${OHA_IMAGE:-ghcr.io/hatoo/oha:latest}
READY_TIMEOUT_SEC=${READY_TIMEOUT_SEC:-420}

# Concurrency is derived from the CPU count so the benchmark measures the
# per-request cost of inspection instead of the queueing delay of an overloaded
# agent. The load generator, traefik, the agent and the backend all share these
# CPUs, and inspection is the expensive part, so a fraction of the cores keeps
# the agent below saturation: a quarter of the cores measured cleanly on a
# 20-core host where driving it at one connection per core pushed p99 from
# 16 ms to 3 s. Set BENCH_CONCURRENCY to pin it instead.
CPUS=$(nproc)
CONCURRENCY_DIVISOR=${BENCH_CONCURRENCY_DIVISOR:-4}
CONCURRENCY=${BENCH_CONCURRENCY:-}
if [ -z "$CONCURRENCY" ]; then
    CONCURRENCY=$(( CPUS / CONCURRENCY_DIVISOR ))
    if [ "$CONCURRENCY" -lt 2 ]; then
        CONCURRENCY=2
    fi
fi

RESULT_DIR=$(mktemp -d)

compose() {
    if docker compose version >/dev/null 2>&1; then
        docker compose -f docker-compose.bench.yml "$@"
    else
        docker-compose -f docker-compose.bench.yml "$@"
    fi
}

cleanup() {
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$RESULT_DIR"
}
trap cleanup EXIT

echo "Starting the benchmark stack..."
compose up -d

echo "Waiting for both entrypoints to answer..."
for i in $(seq 1 30); do
    if curl -fsS -o /dev/null "$BASELINE_URL/" && curl -fsS -o /dev/null "$APPSEC_URL/"; then
        break
    fi
    if [ "$i" = 30 ]; then
        echo "FAIL: traefik did not start"
        exit 1
    fi
    sleep 2
done

# The "after" numbers are only meaningful once the agent actually inspects
# traffic. Until registration and policy load complete the plugin fails open,
# which would measure nothing but the daemon round-trip.
echo "Waiting for the attachment to inspect traffic (attack must be blocked)..."
start=$(date +%s)
inspecting=0
while [ $(( $(date +%s) - start )) -lt "$READY_TIMEOUT_SEC" ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$APPSEC_URL/?file=../../../../etc/passwd" || echo 000)
    if [ "$code" = "403" ]; then
        inspecting=1
        break
    fi
    sleep 5
done
if [ "$inspecting" != "1" ]; then
    echo "FAIL: the attachment never started inspecting; benchmark would be meaningless"
    exit 1
fi
echo "Attachment is inspecting."

POST_BODY="name=john&city=seoul&comment=hello+from+the+benchmark"

# oha <requests> <concurrency> <url> <method> [body] -- writes JSON to stdout
oha() {
    local requests=$1 concurrency=$2 url=$3 method=$4 body=${5:-}
    local args=(--no-tui -c "$concurrency" -m "$method" --disable-keepalive)
    if [ -n "$body" ]; then
        args+=(-d "$body" -H "Content-Type: application/x-www-form-urlencoded")
    fi
    docker run --rm --network host "$OHA_IMAGE" \
        --output-format json -n "$requests" "${args[@]}" "$url"
}

# run_case <output-name> <url> <method> [body]
run_case() {
    local name=$1 url=$2 method=$3 body=${4:-}
    # Warm up so plugin/daemon start-up costs do not skew the measurement.
    oha "$WARMUP_REQUESTS" "$CONCURRENCY" "$url" "$method" "$body" >/dev/null 2>&1 || true
    oha "$REQUESTS" "$CONCURRENCY" "$url" "$method" "$body" > "$RESULT_DIR/$name.json"
}

echo "Running benchmark: ${REQUESTS} requests, concurrency ${CONCURRENCY} (${CPUS} CPUs)"
for scenario in get post; do
    case "$scenario" in
        get)
            method=GET; body=""; path="/" ;;
        post)
            method=POST; body="$POST_BODY"; path="/form" ;;
    esac
    echo "  - $scenario baseline"
    run_case "${scenario}-baseline" "$BASELINE_URL$path" "$method" "$body"
    echo "  - $scenario appsec"
    run_case "${scenario}-appsec" "$APPSEC_URL$path" "$method" "$body"
done

echo "Building the report..."
# Inspection throughput scales with the number of attachment workers (one IPC
# channel each), so record the sizing the numbers were produced with.
workers=$(compose logs traefik 2>&1 |
    grep -o 'starting with [0-9]* attachment workers' | head -1 |
    grep -o '[0-9]*' || true)
export BENCH_CPUS="$CPUS"
export BENCH_WORKERS="${workers:-unknown}"
report=$(python3 report_benchmark.py "$RESULT_DIR" "$REQUESTS" "$CONCURRENCY")
echo "$report"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "$report" >> "$GITHUB_STEP_SUMMARY"
fi
