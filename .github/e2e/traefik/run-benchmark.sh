#!/usr/bin/env bash
# Benchmarks three ways of serving the same traffic:
#
#   1. no attachment            traefik straight to the backend
#   2. Yaegi plugin             the middleware interpreted by traefik's plugin runtime
#   3. built-in middleware      the same middleware compiled into traefik
#
# Every measurement hits the same backend through the same traefik routing with
# the same middleware configuration, so the differences are the cost of
# inspection and the cost of interpreting it.
#
# The two traefik variants are measured one at a time: their attachments would
# otherwise collide in the shared-memory namespace, and running only one at a
# time also keeps the idle variant from stealing CPU from the measured one.
#
# The result is written as a markdown table to stdout and, when running in
# GitHub Actions, appended to the job summary.
set -euo pipefail
cd "$(dirname "$0")"

APPSEC_URL=${APPSEC_URL:-http://127.0.0.1:8080}
BASELINE_URL=${BASELINE_URL:-http://127.0.0.1:8081}
REQUESTS=${BENCH_REQUESTS:-2000}
CONCURRENCY=${BENCH_CONCURRENCY:-20}
WARMUP_REQUESTS=${BENCH_WARMUP_REQUESTS:-200}
OHA_IMAGE=${OHA_IMAGE:-ghcr.io/hatoo/oha:latest}
READY_TIMEOUT_SEC=${READY_TIMEOUT_SEC:-420}
POST_BODY="name=john&city=seoul&comment=hello+from+the+benchmark"
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

# oha <output-name> <url> <method> [body]
run_case() {
    local name=$1 url=$2 method=$3 body=${4:-}
    local args=(--no-tui -c "$CONCURRENCY" -m "$method" --disable-keepalive)
    if [ -n "$body" ]; then
        args+=(-d "$body" -H "Content-Type: application/x-www-form-urlencoded")
    fi

    # Warm up so start-up costs do not skew the measurement. Yaegi in
    # particular is slowest on the first calls through a handler.
    docker run --rm --network host "$OHA_IMAGE" \
        --output-format quiet -n "$WARMUP_REQUESTS" "${args[@]}" "$url" >/dev/null 2>&1 || true

    docker run --rm --network host "$OHA_IMAGE" \
        --output-format json -n "$REQUESTS" "${args[@]}" "$url" > "$RESULT_DIR/$name.json"
}

wait_for_traefik() {
    local i
    for i in $(seq 1 30); do
        if curl -fsS -o /dev/null "$BASELINE_URL/" && curl -fsS -o /dev/null "$APPSEC_URL/"; then
            return 0
        fi
        sleep 2
    done
    echo "FAIL: traefik did not start"
    return 1
}

is_inspecting() {
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' "$APPSEC_URL/?file=../../../../etc/passwd" || echo 000)
    [ "$code" = "403" ]
}

# The inspected numbers only mean something once the agent actually inspects;
# until registration and policy load finish the middleware fails open, which
# would measure nothing but the daemon round-trip.
wait_for_inspection() {
    local start
    start=$(date +%s)
    while [ $(( $(date +%s) - start )) -lt "$READY_TIMEOUT_SEC" ]; do
        if is_inspecting; then
            return 0
        fi
        sleep 5
    done
    echo "FAIL: the attachment never started inspecting; benchmark would be meaningless"
    return 1
}

# Load itself can switch inspection off — the agent sheds it, or the middleware
# fails open when the daemon cannot keep up. Checking only before the run would
# report the cost of not inspecting as the cost of inspecting, so the state
# after the run is recorded and surfaced in the report.
record_inspection_state() {
    local name=$1
    if is_inspecting; then
        echo yes > "$RESULT_DIR/$name.inspecting"
    else
        echo no > "$RESULT_DIR/$name.inspecting"
        echo "    NOTE: inspection was no longer active after this run"
    fi
}

# One failed daemon call opens the fail-open window for errorBackoffMs, which
# is long enough to swallow a whole measurement: at a few thousand requests per
# second a 2 s window covers far more requests than the run itself. Counting
# the failures around each run is what makes a contaminated measurement
# impossible to mistake for a fast one.
daemon_failures() {
    compose logs "$1" 2>&1 | grep -c "daemon communication failure" || true
}

# How much inspection the agent actually asked for. A transaction it finalizes
# on the first call costs one round trip; one it keeps inspecting costs a call
# per body chunk and per response stage. That decision is the agent's, it
# changes as the agent learns, and it dwarfs the difference between the two
# execution modes — so the counts have to be reported next to the timings for
# the comparison to mean anything.
# Prints the counter, or nothing if it could not be read. A failed read must
# not come back as a number: subtracting one from a reading taken earlier
# produces a negative "count" that looks like data.
daemon_stat() {
    local service=$1 field=$2 attempt value
    for attempt in 1 2 3; do
        value=$(compose exec -T "$service" wget -q -O- http://127.0.0.1:8579/healthz 2>/dev/null |
            python3 -c "import json,sys; print(json.load(sys.stdin)['$field'])" 2>/dev/null || true)
        if [ -n "$value" ]; then
            echo "$value"
            return 0
        fi
        sleep 1
    done
}

# The two variants are configured identically, so confirm from the logs that
# each one really ran the implementation it is being credited with; otherwise a
# misbuilt image would quietly benchmark the same code twice.
assert_implementation() {
    local variant=$1 service=$2 logs
    logs=$(compose logs "$service" 2>&1)

    case "$variant" in
        native)
            if ! echo "$logs" | grep -q "compiled in, not interpreted"; then
                echo "FAIL: $service did not use the compiled-in middleware"
                return 1
            fi
            ;;
        yaegi)
            if echo "$logs" | grep -q "compiled in, not interpreted"; then
                echo "FAIL: $service used the compiled-in middleware, not the interpreter"
                return 1
            fi
            ;;
    esac
}

# measure_variant <variant> <service>
# Records the inspected traffic for the variant plus that binary's own
# uninspected baseline, so a slower traefik build cannot masquerade as
# inspection overhead.
measure_variant() {
    local variant=$1 service=$2

    echo "=== $variant ==="
    # A fresh agent per variant. An attachment's shared-memory identity is its
    # worker index, so a second attachment would inherit the registrations of
    # the one measured before it; starting clean also gives both variants the
    # same warm-up state.
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    compose up -d appsec-agent whoami
    compose up -d "$service"
    wait_for_traefik
    wait_for_inspection
    assert_implementation "$variant" "$service"
    echo "Attachment is inspecting."

    local scenario method body path
    for scenario in get post; do
        case "$scenario" in
            get)  method=GET;  body="";           path="/" ;;
            post) method=POST; body="$POST_BODY"; path="/form" ;;
        esac
        echo "  - $scenario baseline"
        run_case "${scenario}-baseline-${variant}" "$BASELINE_URL$path" "$method" "$body"

        echo "  - $scenario $variant"
        local failures_before failures_after chunks_before chunks_after
        failures_before=$(daemon_failures "$service")
        chunks_before=$(daemon_stat "$service" chunksSent)
        run_case "${scenario}-${variant}" "$APPSEC_URL$path" "$method" "$body"
        chunks_after=$(daemon_stat "$service" chunksSent)
        failures_after=$(daemon_failures "$service")
        record_inspection_state "${scenario}-${variant}"

        if [ -n "$chunks_before" ] && [ -n "$chunks_after" ]; then
            echo "$(( chunks_after - chunks_before ))" > "$RESULT_DIR/${scenario}-${variant}.chunks"
        else
            echo "    NOTE: could not read the daemon's inspection counters for this run"
        fi
        echo "$(( failures_after - failures_before ))" > "$RESULT_DIR/${scenario}-${variant}.failures"
    done

    # Record the sizing the numbers were produced with; inspection throughput
    # scales with the number of attachment workers (one IPC channel each).
    compose logs "$service" 2>&1 |
        grep -o 'starting with [0-9]* attachment workers' | head -1 |
        grep -o '[0-9]*' > "$RESULT_DIR/$variant.workers" || true
}

echo "Running benchmark: ${REQUESTS} requests, concurrency ${CONCURRENCY}"
measure_variant yaegi traefik-yaegi
measure_variant native traefik-native

echo "Building the report..."
export BENCH_CPUS="$(nproc)"
export BENCH_WORKERS="$(cat "$RESULT_DIR/yaegi.workers" 2>/dev/null || echo unknown)"
report=$(python3 report_benchmark.py "$RESULT_DIR" "$REQUESTS" "$CONCURRENCY")
echo "$report"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "$report" >> "$GITHUB_STEP_SUMMARY"
fi
