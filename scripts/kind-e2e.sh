#!/usr/bin/env bash
# End-to-end test of the dev overlay on a local kind cluster. kind's default CNI (kindnet, kind >= 0.24)
# enforces NetworkPolicy, so this also proves the default-deny allowlists.
#   1. cluster + images + CloudNativePG + secrets + dev overlay
#   2. wait for PostgreSQL, Redis/Sentinel, observability stack and both services
#   3. smoke test, trace/log correlation, Prometheus targets and Alertmanager wiring
#   4. Redis failover: hang the primary, Sentinel promotes a replica, tokens still mint, old primary rejoins
# Requires Docker with at least 6 GiB and 4 CPUs. Existing kind clusters are refused by default.
# Usage: scripts/kind-e2e.sh            (KEEP_CLUSTER=1 to leave the cluster running afterwards)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="${KIND_CLUSTER:-sre-token}"
NS="sre-platform-dev"
PY_IMAGE="ghcr.io/your-org/sre-python-service:dev"
GO_IMAGE="ghcr.io/your-org/sre-go-service:dev"
cd "$ROOT"

log() { printf '\n==> %s\n' "$*"; }
ORIGINAL_CONTEXT=""
if kubectl config current-context >/dev/null 2>&1; then
  ORIGINAL_CONTEXT="$(kubectl config current-context)"
fi

existing_clusters="$(kind get clusters)"
if printf '%s\n' "$existing_clusters" | grep -Fxq "$CLUSTER"; then
  echo "kind cluster '$CLUSTER' already exists; choose a different KIND_CLUSTER. Existing clusters are never reused or deleted." >&2
  exit 1
fi
if [ -n "$existing_clusters" ] && [ "${ALLOW_PARALLEL_KIND_CLUSTER:-0}" != "1" ]; then
  printf 'Existing kind cluster(s) detected:\n%s\n' "$existing_clusters" >&2
  echo "Stop them before this resource-intensive test, or set ALLOW_PARALLEL_KIND_CLUSTER=1 if Docker has enough capacity." >&2
  exit 1
fi

docker_memory_bytes="$(docker info --format '{{.MemTotal}}')"
if [[ ! "$docker_memory_bytes" =~ ^[0-9]+$ ]]; then
  echo "Could not determine Docker Engine memory; refusing to start kind test." >&2
  exit 1
fi
minimum_memory_bytes=$((6 * 1024 * 1024 * 1024))
if (( docker_memory_bytes < minimum_memory_bytes )); then
  printf 'Docker Engine has %s bytes of memory; kind-e2e requires at least 6 GiB for the full stack.\n' "$docker_memory_bytes" >&2
  exit 1
fi
docker_cpu_count="$(docker info --format '{{.NCPU}}')"
if [[ ! "$docker_cpu_count" =~ ^[0-9]+$ ]] || (( docker_cpu_count < 4 )); then
  printf 'Docker Engine reports %s CPUs; kind-e2e requires at least 4 CPUs for the full stack.\n' "$docker_cpu_count" >&2
  exit 1
fi

pids=()
created_cluster=0
cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  if [ "$created_cluster" = "1" ] && [ "${KEEP_CLUSTER:-0}" != "1" ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    if [ -n "$ORIGINAL_CONTEXT" ] && kubectl config get-contexts "$ORIGINAL_CONTEXT" >/dev/null 2>&1; then
      if ! kubectl config use-context "$ORIGINAL_CONTEXT" >/dev/null; then
        echo "Could not restore kubectl context '$ORIGINAL_CONTEXT'." >&2
      fi
    fi
  fi
}
on_error() {
  echo "e2e failed; recent state:" >&2
  kubectl -n "$NS" get pods -o wide >&2 || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -30 >&2 || true
}
trap on_error ERR
trap cleanup EXIT

log "Create kind cluster $CLUSTER"
created_cluster=1
kind create cluster --name "$CLUSTER" --wait 120s

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
