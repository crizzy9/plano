#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Isolate this disposable project from a developer's provider settings and .env.
for key in "${!PLANO_@}"; do
  unset "$key"
done
export PLANO_DEMO=true PLANO_BIND_IP=127.0.0.1 PLANO_PORT=0
export PLANO_DOMAIN=plano.example.test COMPOSE_PARALLEL_LIMIT=1

scratch=$(mktemp -d "${TMPDIR:-/tmp}/plano-verify.XXXXXXXX")
suffix=${scratch##*.}
compose=(docker compose --env-file /dev/null --project-name "plano-verify-${suffix,,}" -f compose.yaml)
cleanup() {
  local status=$?
  if (( status != 0 )); then
    "${compose[@]}" logs --tail 50 plano >&2 || true
  fi
  "${compose[@]}" down --volumes || status=1
  rmdir "$scratch" || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"${compose[@]}" -f compose.https.yaml config --format json | jq -e '
  (.services.plano.ports // [] | length == 0) and
  (.services.plano.environment.PLANO_REQUIRE_ENROLLMENT == "true") and
  (.services.plano.environment.PLANO_PUBLIC_URL == "https://plano.example.test") and
  (.services.caddy.depends_on.plano.condition == "service_healthy") and
  (.services.caddy.ports | map(.target) | sort == [80, 443])
' >/dev/null

check_container() {
  local address
  address=$("${compose[@]}" port plano 8080)
  [[ $address =~ ^127\.0\.0\.1:[1-9][0-9]*$ ]]
  origin="http://$address"
  test "$("${compose[@]}" exec -T plano id -u)" = 10001
  request "$origin/healthz" | jq -e '.status == "ok" and .mode == "demo"' >/dev/null
}

request() {
  curl -q --noproxy '*' --fail --silent --show-error --max-time 10 "$@"
}

enroll() {
  request -H 'Content-Type: application/json' -d '{}' "$origin/v1/installations" |
    jq -er '.token | strings | select(length > 0)'
}

"${compose[@]}" up --build --wait --wait-timeout 90
check_container
initial_container=$("${compose[@]}" ps -q plano)
test -n "$initial_container"
owner=$(enroll)
other=$(enroll)
test "$owner" != "$other"
flight_id=$(request --get -H "Authorization: Bearer $owner" \
  --data-urlencode number=UA123 --data-urlencode date=2020-01-01 "$origin/v1/flights" |
  jq -er '.flights[0] | select(.number == "UA123") | .id | strings | select(length > 0)')
request -H "Authorization: Bearer $owner" -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg id "$flight_id" '{flightId: $id}')" "$origin/v1/tracked" >/dev/null

"${compose[@]}" up --no-build --force-recreate --wait --wait-timeout 90
new_container=$("${compose[@]}" ps -q plano)
test -n "$new_container"
test "$new_container" != "$initial_container"
check_container
request -H "Authorization: Bearer $owner" "$origin/v1/tracked" |
  jq -e --arg id "$flight_id" '[.flights[].id] == [$id]' >/dev/null
request -H "Authorization: Bearer $other" "$origin/v1/tracked" |
  jq -e '.flights | length == 0' >/dev/null
printf '%s\n' 'Compose demo health, non-root execution, persistence, and installation isolation passed.'
