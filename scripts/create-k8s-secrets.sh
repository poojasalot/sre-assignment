#!/usr/bin/env sh
# Creates the platform Secret with random values in the given namespace (idempotent).
# PostgreSQL credentials are not here: CloudNativePG generates the postgres-app Secret.
# Production should source these from a secret manager (e.g. External Secrets Operator).
set -eu
namespace="${1:?usage: $0 <namespace>}"

kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
if kubectl -n "$namespace" get secret platform-secrets >/dev/null 2>&1; then
  echo "platform-secrets already exists in $namespace"
  exit 0
fi
kubectl -n "$namespace" create secret generic platform-secrets \
  --from-literal=REDIS_PASSWORD="$(openssl rand -hex 32)" \
  --from-literal=GRAFANA_ADMIN_PASSWORD="$(openssl rand -hex 16)"
echo "Grafana admin password: kubectl -n $namespace get secret platform-secrets -o jsonpath='{.data.GRAFANA_ADMIN_PASSWORD}' | base64 -d"
