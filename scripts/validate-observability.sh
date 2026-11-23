#!/usr/bin/env sh
# Proves end-to-end correlation for one request: we choose the trace ID ourselves (W3C traceparent),
# then require (1) a Tempo trace containing the Python, Go, Redis and audit-queue spans and
# (2) Loki log lines from BOTH services carrying that trace_id, found with a single LogQL query.
set -eu

BASE_URL="${BASE_URL:-http://localhost:8000}"
LOKI_URL="${LOKI_URL:-http://localhost:3100}"
TEMPO_URL="${TEMPO_URL:-http://localhost:3200}"
ATTEMPTS="${ATTEMPTS:-30}"

hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
trace_id="$(hex 16)"
traceparent="00-${trace_id}-$(hex 8)-01"

curl --fail --silent --show-error -o /dev/null -X POST "$BASE_URL/token" \
  -H "traceparent: $traceparent" -H 'Content-Type: application/json' \
  -d '{"user_email":"alice@example.com"}'
echo "Issued token with trace ID $trace_id"

# Telemetry is batched (~5s), so poll until it arrives.
retry() {
  description="$1"; shift
  i=1
  while [ "$i" -le "$ATTEMPTS" ]; do
    if "$@"; then echo "OK: $description"; return 0; fi
    sleep 2; i=$((i + 1))
  done
  echo "FAILED: $description" >&2
  return 1
}

logql="{service_name=~\"python-token-service|go-token-service\"} | trace_id=\"$trace_id\""

logs_from_both_services() {
  curl --fail --silent --get "$LOKI_URL/loki/api/v1/query_range" \
    --data-urlencode "query=$logql" --data-urlencode 'limit=100' | python3 -c '
import json, sys
streams = json.load(sys.stdin)["data"]["result"]
services = {s["stream"].get("service_name") for s in streams}
sys.exit(0 if {"python-token-service", "go-token-service"} <= services else 1)'
}

complete_trace() {
  curl --fail --silent "$TEMPO_URL/api/traces/$trace_id" | python3 -c '
import json, sys

def walk(node, services, spans):
    if isinstance(node, dict):
        if node.get("key") == "service.name":
            services.add(node.get("value", {}).get("stringValue"))
        if "spanId" in node and "name" in node:
            spans.add(node["name"])
        for value in node.values():
            walk(value, services, spans)
    elif isinstance(node, list):
        for value in node:
            walk(value, services, spans)

services, spans = set(), set()
walk(json.load(sys.stdin), services, spans)
required_spans = {"redis.set", "redis.xadd", "audit.process"}
ok = {"python-token-service", "go-token-service"} <= services and required_spans <= spans
if ok:
    print("  spans:", ", ".join(sorted(spans)))
sys.exit(0 if ok else 1)'
}

retry "Tempo trace spans Python -> PostgreSQL -> Go -> Redis -> audit queue" complete_trace
retry "Loki returns logs from both services for one trace ID" logs_from_both_services
echo "LogQL used: $logql"
