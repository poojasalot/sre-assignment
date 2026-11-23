#!/usr/bin/env sh
set -eu
BASE_URL="${BASE_URL:-http://localhost:8000}"

printf 'Checking Python liveness...\n'
curl --fail --silent --show-error "$BASE_URL/health/live"
printf '\nChecking Python readiness...\n'
curl --fail --silent --show-error "$BASE_URL/health/ready"
printf '\nRequesting token for seeded user...\n'
curl --fail --silent --show-error -X POST "$BASE_URL/token" \
  -H 'Content-Type: application/json' \
  -d '{"user_email":"alice@example.com"}'
printf '\nChecking unknown user returns 404...\n'
status="$(curl --silent --output /dev/null --write-out '%{http_code}' -X POST "$BASE_URL/token" \
  -H 'Content-Type: application/json' \
  -d '{"user_email":"nobody@example.com"}')"
[ "$status" = "404" ] || { echo "expected 404, got $status" >&2; exit 1; }
echo 'Smoke test passed.'
