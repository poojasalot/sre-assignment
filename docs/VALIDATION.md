# Validation record and remaining checks

## Checks performed in the packaging environment

| Check | Result | Notes |
|---|---|---|
| Original assignment reviewed | Pass | Requirements mapped to code/config/docs |
| Original source tree inspected | Pass | FastAPI, Go gRPC, proto files, dependency manifests and SQL reviewed |
| Python syntax compilation | Pass | `python3 -m compileall -q python-service` |
| Go source formatting/parser | Pass | `gofmt -d` reports no formatting changes for Go application and test files |
| YAML parsing | Pass | 30 YAML/YML files parsed with PyYAML; 2 JSON files parsed; all 19 Kustomize resource/config input references exist |
| Python unit tests | Pass | `pytest.ini` provides a repository-level Python path; all tests passed locally: 6 passed. |
| Docker image builds / Compose configuration | Pass | Both application images built successfully; `docker-compose config` passed. |
| Go unit tests/vet | Pass | `go test ./...` and `go vet ./...` passed. |
| Kustomize rendering | Pass | Base, platform, application and dev/staging/production roots rendered successfully. |
| Kubernetes live deployment / failover | Pending | Requires a real Kubernetes cluster, CNPG operator and Redis/Sentinel runtime. |

## Commands to run before submission

```bash
# Python tests
python3 -m pip install -r python-service/requirements-dev.txt
(cd python-service && python3 -m pytest -q)

# Go tests with Go 1.27.4+
(cd golang-service && go test ./... && go vet ./...)

# Build images and exercise the integrated stack
docker compose config
docker compose up --build -d
# Legacy alternative:
# docker-compose config
# docker-compose up --build -d
./scripts/smoke-test.sh

# Docker validation
docker-compose build
docker-compose up -d
./scripts/smoke-test.sh
docker-compose down --volumes --remove-orphans

# Check Kubernetes resources locally with Kustomize and on a real cluster
kubectl kustomize k8s/base > /tmp/sre-platform-rendered.yaml
kubectl apply --dry-run=client -k k8s/base
kubectl apply -f k8s/secrets.yaml
kubectl apply -k k8s/base

# Render all Kustomize roots
make k8s-all-render
```

The generated manifests should also be validated with a Kubernetes schema validator such as kubeconform against the exact target cluster version. Then verify PVC binding, HPA metrics, ingress/network policy behavior, alert evaluation and OTLP export from inside the cluster.

## Other advanced follow-up changes

The following hardening items have been added to the repository:

- Independent Kustomize roots for Python, Go and shared platform resources.
- Hash-suffix generation for generated ConfigMaps.
- Preferred zone/node topology spreading and pod anti-affinity for application Pods.
- Default-deny ingress and egress policies with explicit application dependencies and DNS rules.
- Redis exporter Deployment/Service and Compose service with Prometheus scrape configuration.
- Optional Python/Go gRPC TLS/mTLS settings while retaining the local plaintext demo default.
- CI rendering and kubeconform Kubernetes schema validation.

## Known follow-up fixes / validation focus

1. Confirm exact dependency and container tag compatibility in CI; pin by digest for production deployments.
2. Run integration tests against disposable PostgreSQL and Redis rather than relying only on mocked Python/Go unit tests.
3. Run `docker compose logs` and verify OTLP logs appear in Loki with `service_name`, `trace_id` and `span_id` structured fields.
4. Verify a single Tempo trace includes the SQL and Redis child spans. Instrumentation versions/semantics can vary; treat this as a release gate.
5. Check Prometheus rule syntax and alert state with `promtool check rules` / `promtool test rules`; alert thresholds are provisional.
6. Verify Kubernetes container UIDs and writable-volume ownership for PostgreSQL, Redis, Loki, Tempo, Prometheus and Grafana against the pinned images.
7. Apply manifests on the actual CNI and test NetworkPolicies; probe traffic and ingress controller namespace labels vary by cluster.
8. Connect Alertmanager to GEANT's real notification path and test a synthetic page.
9. Verify independent Kustomize roots render and reconcile independently; confirm the platform root is applied before application roots.
10. Exercise generated ConfigMap rollout hashing and verify only affected application Pods restart.
11. Validate zone/node topology spreading on a cluster with representative failure-domain labels and capacity.
12. Test default-deny egress from inside every workload; validate DNS, PostgreSQL, Go, Redis and OTLP allowlists with the target CNI.
13. Scrape Redis exporter metrics in Compose and Kubernetes and validate memory, evictions, connections and command metrics.
14. Generate a CA and workload certificates; verify Python/Go mutual TLS, certificate rejection and certificate rotation behavior.

## HA, GitOps and P0/P1 validation

1. Install CloudNativePG and verify a three-instance cluster reaches `Cluster in Healthy state`.
2. Create `k8s/secrets.yaml` with CNPG `username`/`password` keys and application `POSTGRES_PASSWORD`/`REDIS_PASSWORD` keys.
3. Apply `k8s/platform`, verify the migration Job completes, and verify PostgreSQL read/write routing through `postgres-rw`.
4. Validate Redis Sentinel quorum, trigger a controlled primary failure and verify the Go service reconnects to the promoted primary.
5. Render and validate dev, staging and production overlays independently.
6. In staging, apply the Argo CD example and verify environment-specific namespaces and image tags.
7. Run `scripts/validate-observability.sh` and require a Loki-correlated Tempo trace.
8. Configure and test a real Alertmanager notification route; the checked-in webhook is a placeholder and must not be used as a production receiver.
