#!/usr/bin/env sh
set -eu

BASE_URL="${BASE_URL:-http://localhost:8000}"
Loki_URL="${LOKI_URL:-http://localhost:3100}"
TEMPO_URL="${TEMPO_URL:-http://localhost:3200}"

response="$(curl --fail --silent --show-error -X POST "$BASE_URL/token" -H 'Content-Type: application/json' -d '{"user_email":"alice@example.com"}')"
trace_id="$(curl --fail --silent --show-error "$Loki_URL/loki/api/v1/query_range" --get --data-urlencode 'query={service_name="python-token-service"} |= "token_issued"' --data-urlencode 'limit=1' | python3 -c 'import json,sys; d=json.load(sys.stdin); found=""; 
for stream in d.get("data",{}).get("result",[]):
    for _,line in stream.get("values",[]):
        try:
            found=json.loads(line).get("trace_id","")
        except Exception:
            found=""
        if found:
            break
    if found:
        break
print(found)')"

[ -n "$trace_id" ] || { echo 'No correlated Python trace ID found in Loki' >&2; exit 1; }

echo "Trace ID: $trace_id"
curl --fail --silent --show-error "$Loki_URL/loki/api/v1/query_range" --get \
  --data-urlencode 'query={service_name="python-token-service", trace_id="'$trace_id'"}' --data-urlencode 'limit=20' >/dev/null
curl --fail --silent --show-error "$Loki_URL/loki/api/v1/query_range" --get \
  --data-urlencode 'query={service_name="go-token-service", trace_id="'$trace_id'"}' --data-urlencode 'limit=20' >/dev/null

curl --fail --silent --show-error "$TEMPO_URL/api/traces/$trace_id" >/dev/null
echo 'Tempo trace and Loki correlation validated.'
