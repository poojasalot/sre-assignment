#!/usr/bin/env bash
# End-to-end test of the dev overlay on a local kind cluster. kind's default CNI (kindnet, kind >= 0.24)
# enforces NetworkPolicy, so this also proves the default-deny allowlists.
#   1. cluster + images + CloudNativePG + secrets + dev overlay
#   2. wait for PostgreSQL, Redis/Sentinel, observability stack and both services
#   3. smoke test, trace/log correlation, Prometheus targets and Alertmanager wiring
#   4. Redis failover: hang the primary, Sentinel promotes a replica, tokens still mint, old primary rejoins
# Usage: scripts/kind-e2e.sh            (KEEP_CLUSTER=1 to leave the cluster running afterwards)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="${KIND_CLUSTER:-sre-token}"
NS="sre-platform-dev"
PY_IMAGE="ghcr.io/your-org/sre-python-service:dev"
GO_IMAGE="ghcr.io/your-org/sre-go-service:dev"
cd "$ROOT"

log() { printf '\n==> %s\n' "$*"; }
pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  if [ "${KEEP_CLUSTER:-0}" != "1" ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
}
on_error() {
  echo "e2e failed; recent state:" >&2
  kubectl -n "$NS" get pods -o wide >&2 || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -30 >&2 || true
}
trap on_error ERR
trap cleanup EXIT

log "Create kind cluster $CLUSTER"
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s

log "Build and load images"
docker build -t "$PY_IMAGE" python-service
docker build -t "$GO_IMAGE" golang-service
kind load docker-image "$PY_IMAGE" "$GO_IMAGE" --name "$CLUSTER"

log "Install CloudNativePG and create secrets"
./scripts/install-cnpg.sh
./scripts/create-k8s-secrets.sh "$NS"

log "Apply the dev overlay"
kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/overlays/dev | kubectl apply --server-side -f -

log "Wait for the platform"
kubectl -n "$NS" wait cluster.postgresql.cnpg.io/postgres --for=condition=Ready --timeout=10m
for workload in statefulset/redis statefulset/redis-sentinel deployment/otel-collector deployment/loki \
  deployment/tempo deployment/prometheus deployment/alertmanager deployment/grafana \
  deployment/go-token-service deployment/python-token-service; do
  kubectl -n "$NS" rollout status "$workload" --timeout=5m
done

forward() { kubectl -n "$NS" port-forward "svc/$1" "$2" >/dev/null 2>&1 & pids+=($!); }
forward python-service 8000:8000
forward loki 3100:3100
forward tempo 3200:3200
forward prometheus 9091:9090
sleep 3

log "Smoke test and observability correlation"
./scripts/smoke-test.sh
./scripts/validate-observability.sh

log "Prometheus: every target up and Alertmanager discovered"
prometheus_ok() {
  curl -fsS http://localhost:9091/api/v1/targets | python3 -c '
import json, sys
targets = json.load(sys.stdin)["data"]["activeTargets"]
jobs = {t["labels"]["job"] for t in targets}
down = [t["scrapeUrl"] for t in targets if t["health"] != "up"]
expected = {"python-token-service", "go-token-service", "otel-collector", "redis-exporter", "cnpg", "prometheus"}
sys.exit(0 if expected <= jobs and not down else 1)' &&
  curl -fsS http://localhost:9091/api/v1/alertmanagers | python3 -c '
import json, sys
sys.exit(0 if json.load(sys.stdin)["data"]["activeAlertmanagers"] else 1)'
}
for _ in $(seq 1 30); do prometheus_ok && break; sleep 5; done
prometheus_ok || { echo "Prometheus targets down or Alertmanager not discovered" >&2; exit 1; }
echo "OK: all scrape targets up, Alertmanager wired"

# A deleted pod restarts from its AOF volume faster than Sentinel's 5s down-after window, so it correctly
# stays primary. To force a failover we hang the primary for longer than that window instead.
log "Redis failover via Sentinel (primary hung for 20s)"
sentinel() { kubectl -n "$NS" exec redis-sentinel-0 -- redis-cli -p 26379 --raw "$@"; }
redis_cli() { kubectl -n "$NS" exec "$1" -c redis -- sh -c "REDISCLI_AUTH=\"\$REDIS_PASSWORD\" redis-cli $2"; }
old_primary="$(sentinel sentinel get-master-addr-by-name mymaster | head -n 1)"
echo "current primary: $old_primary"
redis_cli "${old_primary%%.*}" "CLIENT PAUSE 20000 ALL"
new_primary="$old_primary"
for _ in $(seq 1 30); do
  new_primary="$(sentinel sentinel get-master-addr-by-name mymaster | head -n 1)"
  [ "$new_primary" != "$old_primary" ] && break
  sleep 2
done
[ "$new_primary" != "$old_primary" ] || { echo "Sentinel did not promote a new primary" >&2; exit 1; }
echo "promoted primary: $new_primary"
for _ in $(seq 1 30); do ./scripts/smoke-test.sh >/dev/null 2>&1 && break; sleep 2; done
./scripts/smoke-test.sh
for _ in $(seq 1 30); do redis_cli "${old_primary%%.*}" "INFO replication" | grep -q '^role:slave' && break; sleep 2; done
redis_cli "${old_primary%%.*}" "INFO replication" | grep -q '^role:slave' ||
  { echo "old primary did not rejoin as a replica" >&2; exit 1; }
echo "OK: old primary rejoined as a replica of $new_primary"

log "kind end-to-end test passed"
