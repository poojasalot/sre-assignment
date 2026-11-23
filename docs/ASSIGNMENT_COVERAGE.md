# Assignment coverage matrix

This matrix maps each line of the GÉANT assignment to the implementation and calls out deliberate scope boundaries. File paths are relative to the repository root.

## Required items

| Assignment requirement | Implementation | Status / qualification |
|---|---|---|
| Dockerfile for Python service | `python-service/Dockerfile` | Multi-stage Python 3.12 build, isolated virtualenv, slim runtime, UID/GID 10001, no root runtime. |
| Dockerfile for Go service | `golang-service/Dockerfile` | Multi-stage build, Go 1.27.4 builder, stripped static binary, distroless non-root runtime. |
| Multi-stage builds | Both Dockerfiles | Build tools and intermediate layers are excluded from runtime images. |
| Container security | Both Dockerfiles; Kubernetes pod/container security contexts | Non-root, no privilege escalation, dropped Linux capabilities, read-only app root filesystem where supported. Further image scanning/signing is a go-live action. |
| Local Docker Compose | `docker-compose.yml`, `.env.example` | PostgreSQL, Redis, both apps, OTel Collector, Prometheus, Loki, Tempo, Grafana, named volumes and health checks. |
| Kubernetes app deployments | `k8s/base/python-service.yaml`, `k8s/base/go-service.yaml` | Deployments, Services, resource requests/limits, startup/readiness/liveness probes. |
| PostgreSQL and Redis in Kubernetes | `k8s/base/postgres.yaml`, `k8s/base/redis.yaml` | PostgreSQL uses CloudNativePG with three instances. Redis uses three data Pods and three Sentinel Pods. Install CNPG, test failover, and configure managed backups before production. |
| Resource requests/limits | All app and observability pod specs in `k8s/base/` | Initial capacity values only; benchmark and right-size against real workload. |
| Liveness/readiness | Python `/health/live`, `/health/ready`; Go `/health/live`, `/health/ready`; K8s probes | Liveness checks process health; readiness checks required dependencies. Startup probes protect slow starts. |
| Service discovery | Kubernetes Services + DNS | Python uses `postgres:5432` and `go-service:50051`; Go uses `redis:6379`; apps send OTLP to `otel-collector:4317`. |
| ConfigMaps and Secrets | `k8s/base/config.yaml`, ConfigMap generators in `k8s/base/kustomization.yaml`, `k8s/secrets.example.yaml` | Non-sensitive settings in ConfigMaps; database, Redis and Grafana credentials in Secret references. Example Secret has placeholders and must be replaced. |
| Structured JSON logs (Python + Go) | `python-service/main.py`, `golang-service/main.go` | JSON stdout logs with service/environment and trace/span identifiers. Avoids logging raw token/email. |
| Trace context in logs | Python `TraceContextFilter`; Go `logArgsWithTrace` / OTel slog bridge | W3C trace context propagated; logs can be correlated by trace ID. |
| Log aggregation | `observability/otel/collector.yaml`, `observability/loki/loki.yaml`, Grafana Loki datasource | Apps export OTLP logs to Collector; Collector exports to Loki; Grafana is pre-provisioned. |
| Query one request across services | `README.md` → “Logs and request correlation”; `docs/DEMO_AND_INTERVIEW.md` | Query `service_name` in Loki, copy trace ID, filter the Python and Go records by that ID. |
| HTTP RED metrics | `python-service/main.py` | Request counter and latency histogram use bounded method/route/status labels. Go exposes gRPC RED because the Go API is gRPC, not HTTP. |
| Database connection pool metrics | `python-service/main.py` | In-use/max connection gauges, acquisition latency histogram, query/acquisition failure counters. |
| Redis queue depth and processing time | `golang-service/main.go` | **Not applicable to the supplied starter:** Redis is used as expiring token key/value storage, not as a queue and has no worker. Real Redis operation latency/rate/errors, pool stats and key count are instrumented. Queue metrics are documented as a future extension, not fabricated. |
| Prometheus collection | `observability/prometheus/prometheus-compose.yml`, `observability/prometheus/prometheus-k8s.yml` | Local and Kubernetes scrape configurations. Kubernetes Prometheus has scoped read-only discovery RBAC. |
| Grafana dashboard | `observability/grafana/dashboards/sre-token-platform.json` | Provisioned dashboard includes request rate/errors/latency, gRPC, database pool and Redis indicators. |
| OpenTelemetry instrumentation | Python and Go application code; `observability/otel/collector.yaml` | FastAPI, psycopg2 and gRPC client/server instrumentation plus custom sanitised Redis client spans. |
| Trace propagation Python → PostgreSQL → Go → Redis | Python FastAPI/psycopg2/gRPC client instrumentation; Go gRPC server/custom Redis client spans | W3C Trace Context; expected spans appear when request and Collector are running. Must be confirmed in the actual runtime before submission. |
| Tracing backend | `observability/tempo/tempo.yaml`, Grafana Tempo datasource | Tempo receives OTLP through the Collector. |
| End-to-end trace demo | `README.md` → “End-to-end trace”; `docs/DEMO_AND_INTERVIEW.md` | Trace search and expected span sequence are documented. Live trace not verified in this build environment. |
| Architecture / setup / decisions / observability docs | `README.md`, `docs/DESIGN_DECISIONS.md`, `docs/OPERATIONS.md` | Includes flow diagram, Compose/Kubernetes steps, trade-offs, observability access and readiness guidance. |

## Optional / bonus items

| Bonus item | Implementation | Status / qualification |
|---|---|---|
| Service mesh (Istio/Linkerd) | Not included | Deliberately omitted: adds operational complexity and does not improve this small two-service demo enough to justify it. Add only for an established mesh use case (mTLS policy, traffic management, multi-team platform standard). |
| GitOps (ArgoCD/Flux) | `k8s/argocd-application.example.yaml`, `k8s/overlays/` | Separate dev, staging and production Argo CD Applications are provided. Repository promotion, credentials and approval policy remain environment/platform configuration. |
| Automated tests in CI/CD | `.github/workflows/ci.yml`; Python and Go unit tests | Unit tests, Go vet and Docker image builds are configured. Full integration/load/security pipelines are go-live improvements. |
| SLI/SLO alert rules | `observability/prometheus/alert-rules.yml`, `observability/alertmanager/alertmanager.yml` | Prometheus rules and Alertmanager routing configuration are present. Replace the placeholder webhook and test real notification delivery. |
| Horizontal Pod Autoscaling | `k8s/base/autoscaling.yaml` | CPU-based HPA with conservative scale-down behavior. Requires metrics-server; validate with load tests. |
| Network policies | `k8s/base/network-policies.yaml` | Default-deny ingress plus narrow service-to-service and observability ingress rules. Requires a CNI that enforces NetworkPolicy. |
| Disruption budgets | `k8s/base/autoscaling.yaml` | PDBs for the two app Deployments; ensure replicas and topology meet the real availability requirement. |

## Intentional scope boundaries and next steps

1. The starter's Redis usage is a token key/value store. To meet a queue-specific interpretation of “queue depth and processing time”, introduce a Redis Stream/list plus worker and instrument `XLEN`/`LLEN`, pending/consumer-group lag, oldest-message age, enqueue/dequeue rate, retries/dead-letter count and processing-duration histogram.
2. The Docker Compose stack uses local singleton PostgreSQL/Redis instances. Kubernetes uses CloudNativePG and Redis Sentinel HA, with operator, backup and failover drills still required.
3. Prometheus rules are loaded, but notifications are not delivered until Alertmanager (or a managed alerting integration) is connected, routed, and tested.
4. Kustomize assumes a default StorageClass, `metrics-server`, a NetworkPolicy-capable CNI and (if public access is required) an ingress controller. An HTTPS ingress/gateway is intentionally left environment-specific.
5. The code and manifests are a runnable target design, but this packaging environment lacked Docker, kubectl and a downloadable Go 1.24+ toolchain. Follow `docs/VALIDATION.md` to build, test, render and exercise it on a workstation/CI runner before presenting it as executed.
