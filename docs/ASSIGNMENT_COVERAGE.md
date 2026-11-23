# Assignment coverage matrix

This matrix maps each requirement in the GÉANT SRE assignment to the code that implements it. Paths are
relative to the repository root. The "Verified by" column names the automated check that covers each item;
[VALIDATION.md](VALIDATION.md) records which checks have actually run.

## Required items

| Requirement | Implementation | Verified by |
|---|---|---|
| Dockerfile for the Python service | `python-service/Dockerfile`: multi-stage, venv copied into a slim runtime, UID 10001, `HEALTHCHECK` | CI `containers` (hadolint, build, Trivy) |
| Dockerfile for the Go service | `golang-service/Dockerfile`: multi-stage, static stripped binary, distroless `nonroot` runtime | CI `containers` |
| Multi-stage builds, security best practices | Both Dockerfiles. Kubernetes pods add `runAsNonRoot`, `seccompProfile: RuntimeDefault`, dropped capabilities, no privilege escalation, and a read-only root filesystem for the apps, Redis, Prometheus and the Collector | CI `containers`, `k8s-validation` |
| Local Docker Compose | `docker-compose.yml`: both apps, PostgreSQL, Redis, redis_exporter, the Collector, Prometheus, Alertmanager, Loki, Tempo and Grafana, with health checks, named volumes and an internal backend network | CI `integration` |
| Kubernetes app deployments | `k8s/apps/python-service`, `k8s/apps/go-service`: Deployment, Service, HPA, PDB, topology spread | `make k8s-validate`, `scripts/kind-e2e.sh` |
| PostgreSQL and Redis in Kubernetes | `k8s/platform/postgres.yaml` (CloudNativePG, 3 instances, least-privilege `app` owner, schema from `database/init.sql`); `k8s/platform/redis.yaml` (3 Redis pods with AOF on PVCs, 3 Sentinels, redis_exporter sidecar) | kubeconform with the CNPG CRD schema; the kind e2e waits for the CNPG cluster and runs a Sentinel failover drill |
| Resource requests and limits | Workload containers in the shared bases; dev and staging override CPU/memory requests and limits in `k8s/overlays/{dev,staging}/resources.yaml`, while production keeps the base profile | `make k8s-validate` renders and schema-validates every overlay; compare `rendered/overlays-{dev,staging,production}.yaml` to inspect the effective profiles (schema validation alone does not assert resource values) |
| Liveness and readiness probes | Python `/health/live`, `/health/ready` (PostgreSQL and gRPC); Go `/health/live`, `/health/ready` (Redis); startup probes; probes on every platform component | Python and Go unit tests; kind rollout waits |
| Service discovery | Kubernetes DNS Services: `postgres-rw`, `go-service:50051`, Sentinel `redis-sentinel:26379`, `otel-collector:4317` | kind e2e |
| ConfigMaps and Secrets | `platform-config` (non-sensitive settings); generated ConfigMaps for every observability and DB file; `platform-secrets` from `scripts/create-k8s-secrets.sh`; DB credentials from the CNPG-generated `postgres-app` Secret | kubeconform; kind e2e |
| Structured JSON logs (Python and Go) | Python `JsonFormatter`; Go `slog` JSON handler. Raw tokens and emails are never logged | Unit tests |
| Trace context in logs | Python `TraceContextFilter`; the Go OTel slog bridge. `trace_id` and `span_id` appear on every line and become Loki structured metadata | `scripts/validate-observability.sh` |
| Log aggregation | OTLP logs → Collector → Loki. Grafana's Loki datasource has a `TraceID` derived field linking to Tempo | `validate-observability.sh` |
| Query one request across both services | `{service_name=~"python-token-service\|go-token-service"} \| trace_id="<id>"` | `validate-observability.sh` asserts that this one query returns both services |
| HTTP RED metrics | `http_server_requests_total`, `http_server_request_duration_seconds` (bounded labels); Go gRPC equivalents | Unit tests; dashboard and recording rules |
| Database connection pool metrics | `db_pool_connections_in_use`, `db_pool_connections_max`, `db_pool_acquire_duration_seconds`, `db_pool_acquire_errors_total`, `db_query_errors_total` | Unit tests; alert rules |
| Redis queue depth and processing time | `golang-service/audit_queue.go`: every mint publishes to the `token:events` Redis Stream, and the `audit` consumer group processes, acknowledges and reclaims messages. Depth: `redis_stream_length`, `redis_stream_lag_messages`, `redis_stream_pending_messages`, `redis_stream_oldest_pending_age_seconds`. Processing: `queue_message_processing_duration_seconds`, `queue_messages_processed_total` | `audit_queue_test.go` (consume, trace continuation, dead-consumer reclaim, depth collector); `AuditQueueStalled` rule test |
| Prometheus collection | `observability/prometheus/prometheus-compose.yml` (static targets); `prometheus-k8s.yml` (endpoint and pod discovery in the namespace, through a namespaced Role) | promtool; the kind e2e asserts every target is up |
| Grafana dashboard | `observability/grafana/dashboards/sre-token-platform.json`: HTTP and gRPC RED, availability SLI, DB pool, Redis client and server, audit queue, and Explore links | JSON parse; all 29 PromQL expressions pass promtool |
| OpenTelemetry instrumentation | FastAPI, psycopg2 and gRPC client instrumentation in Python; gRPC server stats handler plus sanitised Redis client spans in Go; the queue producer and consumer propagate context | Go span and trace-continuation tests |
| Trace propagation Python → PostgreSQL → Go → Redis | W3C Trace Context end to end, including through the Redis Stream | `validate-observability.sh` requires `redis.set`, `redis.xadd` and `audit.process` in the same Tempo trace |
| Tracing backend | Tempo via the Collector; Grafana `tracesToLogsV2` returns logs for both services | `validate-observability.sh` |
| End-to-end trace demo | README section 4; `docs/DEMO_AND_INTERVIEW.md` | `validate-observability.sh` |
| Documentation | `README.md` (architecture, setup, observability, decisions); `docs/DESIGN_DECISIONS.md`; `docs/OPERATIONS.md` | n/a |

## Optional and bonus items

| Bonus item | Implementation | Notes |
|---|---|---|
| Service mesh | Not included | A deliberate choice: sidecars and a control plane cost more than they return for two services. Application-level gRPC mTLS is supported, and the Go service fails closed on invalid TLS configuration. |
| GitOps | `k8s/argocd-application.example.yaml`, `k8s/overlays/{dev,staging,production}` | AppProject-scoped Applications. Dev and staging sync automatically; production syncs manually. |
| Automated tests in CI | `.github/workflows/ci.yml` | Unit and race tests, config validation with the deployed images, rule unit tests, strict Kubernetes schema validation, hadolint, Trivy (image and filesystem), Compose integration with trace/log correlation, and the kind e2e with failover. |
| SLI/SLO alerting | `observability/prometheus/alert-rules.yml` + `alert-rules.test.yml`, `observability/alertmanager/alertmanager.yml` | Recording rules, multi-window burn rates (14.4× and 6×), `absent()` alerts, dependency, queue and pipeline alerts. The receiver is a placeholder `example.invalid` webhook. |
| Horizontal Pod Autoscaling | `k8s/apps/*/hpa.yaml` | CPU-based with conservative scale-down. HPAs own the replica count. |
| Network policies | `k8s/platform/network-policies.yaml` | Default-deny ingress **and** egress, with a per-workload allowlist. Enforced in the kind e2e. |
| Disruption budgets | `k8s/apps/*/hpa.yaml`, `k8s/platform/redis.yaml` | App PDBs, plus Redis and Sentinel PDBs with `maxUnavailable: 1`. CNPG manages its own. |

## Scope boundaries

1. Compose runs single PostgreSQL and Redis instances for local use. High availability is a Kubernetes concern.
2. Alerts are routed to Alertmanager, but there is no real receiver, because GÉANT's on-call integration was not provided.
3. Backups (CNPG to object storage), ingress/TLS, external secrets and image signing are environment
   integrations. They are listed in the README section 8 and the OPERATIONS go-live checklist.
