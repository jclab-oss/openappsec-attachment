#!/usr/bin/env bash
# Full e2e test: traefik with the open-appsec attachment AND the open-appsec
# agent in declarative (standalone) mode with a prevent-mode policy.
# Benign traffic must pass, a clear attack must be blocked (403).
set -euo pipefail
cd "$(dirname "$0")"

BASE_URL=${BASE_URL:-http://127.0.0.1:8080}
ATTACK_URL="$BASE_URL/?arg=%27%20OR%201%3D1%20UNION%20SELECT%20username%2C%20password%20FROM%20users--"
TIMEOUT_SEC=${TIMEOUT_SEC:-420}

compose() {
    if docker compose version >/dev/null 2>&1; then
        docker compose -f docker-compose.yml -f docker-compose.agent.yml "$@"
    else
        docker-compose -f docker-compose.yml -f docker-compose.agent.yml "$@"
    fi
}

cleanup() {
    echo "--- traefik logs (tail) ---"
    compose logs traefik | tail -80 || true
    echo "--- agent logs (tail) ---"
    compose logs appsec-agent | tail -40 || true
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

echo "Waiting for the agent registration and policy load (attack must be blocked)..."
start=$(date +%s)
blocked=0
while [ $(( $(date +%s) - start )) -lt "$TIMEOUT_SEC" ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$ATTACK_URL" || echo 000)
    if [ "$code" = "403" ]; then
        blocked=1
        break
    fi
    sleep 5
done

if [ "$blocked" != "1" ]; then
    echo "FAIL: attack request was never blocked (last code: $code)"
    exit 1
fi
echo "Attack request blocked with 403."

echo "Benign request must still pass..."
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/hello")
[ "$code" = "200" ] || { echo "FAIL: benign request returned $code"; exit 1; }

echo "Benign POST must still pass..."
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d 'name=john&city=seoul' "$BASE_URL/form")
[ "$code" = "200" ] || { echo "FAIL: benign POST returned $code"; exit 1; }

echo "Attack in the request body must be blocked..."
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -d "comment=' OR 1=1 UNION SELECT username, password FROM users--" "$BASE_URL/form")
[ "$code" = "403" ] || { echo "FAIL: body attack returned $code"; exit 1; }

echo "E2E TEST PASSED"
