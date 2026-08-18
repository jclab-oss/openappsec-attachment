#!/usr/bin/env bash
# Smoke test: traefik with the open-appsec attachment but WITHOUT an agent.
# The attachment must fail open and forward all traffic.
set -euo pipefail
cd "$(dirname "$0")"

BASE_URL=${BASE_URL:-http://127.0.0.1:8080}

compose() {
    if docker compose version >/dev/null 2>&1; then
        docker compose "$@"
    else
        docker-compose "$@"
    fi
}

cleanup() {
    compose logs traefik | tail -50 || true
    compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

compose up -d

echo "Waiting for traefik to answer..."
for i in $(seq 1 30); do
    if curl -fsS -o /dev/null "$BASE_URL/"; then
        break
    fi
    if [ "$i" = 30 ]; then
        echo "FAIL: traefik did not start"
        exit 1
    fi
    sleep 2
done

traefik_logs=$(compose logs traefik 2>&1)

echo "Checking the plugin was loaded..."
if ! echo "$traefik_logs" | grep -qi "Plugins loaded"; then
    echo "FAIL: plugin was not loaded"
    exit 1
fi

echo "Checking the daemon is running..."
if ! echo "$traefik_logs" | grep -q "openappsec-traefik-daemon"; then
    echo "FAIL: daemon did not start"
    exit 1
fi

echo "GET must be forwarded (fail-open)..."
body=$(curl -fsS "$BASE_URL/")
echo "$body" | grep -q "Hostname" || { echo "FAIL: unexpected body: $body"; exit 1; }

echo "POST with body must be forwarded (fail-open)..."
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d 'some=payload' "$BASE_URL/post")
[ "$code" = "200" ] || { echo "FAIL: POST returned $code"; exit 1; }

echo "Suspicious request must also be forwarded without an agent (fail-open)..."
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/?a=%3Cscript%3Ealert(1)%3C/script%3E")
[ "$code" = "200" ] || { echo "FAIL: fail-open violated, got $code"; exit 1; }

echo "SMOKE TEST PASSED"
