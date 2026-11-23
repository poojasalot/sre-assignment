#!/usr/bin/env sh
set -eu

CNPG_VERSION="${CNPG_VERSION:-1.27.1}"

kubectl apply --server-side -f "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/v${CNPG_VERSION}/releases/cnpg-${CNPG_VERSION}.yaml"
kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=5m
kubectl get deployment cnpg-controller-manager -n cnpg-system
