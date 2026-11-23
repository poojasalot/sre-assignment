# Implementation summary: original starter → SRE assignment

This is the concrete change log for the submitted code. For the complete requirement-by-requirement checklist see `ASSIGNMENT_COVERAGE.md`; for trade-offs and alternatives see `DESIGN_DECISIONS.md`.

## Application changes

| Area / file | What changed | Why | Alternative considered |
|---|---|---|---|
| `python-service/main.py` | Reused one gRPC channel per process; added bounded PostgreSQL `ThreadedConnectionPool`, pool acquisition guard/timeout and pool metrics; added RPC deadline; separated liveness/readiness; added HTTP RED and dependency metrics; added W3C trace propagation, FastAPI/psycopg2/gRPC instrumentation, JSON logs and OTLP export; input validation and safe 404/503/504 mapping; graceful cleanup | Avoid connection churn, unbounded waits, false-positive liveness restarts, opaque failures and uncorrelated incidents | Async SQLAlchemy/asyncpg for a fully async rewrite; OpenTelemetry Metrics SDK instead of Prometheus client; managed DB pooler such as PgBouncer when connection fan-out warrants it |
| `golang-service/main.go` | Added gRPC RED, Redis command rate/error/latency, Redis key-count and pool metrics; added gRPC and sanitised Redis OTel spans plus OTLP logs; strict token TTL/input bounds; cryptographic 256-bit token generation; health/metrics HTTP server; Redis pool/timeouts; graceful gRPC/HTTP/Redis shutdown; structured trace-aware logging | Make the gRPC dependency observable and resilient; expose accurate dependency signals; prevent unsafe TTL/input values; shut down without dropping in-flight calls | JWTs for stateless validation (with key rotation/revocation trade-offs); Redis Streams if a durable asynchronous queue is genuinely required |
| `golang-service/main_test.go` | Added unit tests for token creation, TTL validation, missing token and Redis failure mapping | Protect critical service behavior and error contracts | Add Testcontainers integration tests with real Redis and gRPC client contract tests in CI |
| `python-service/tests/test_api.py` | Added API tests for health, invalid input, unknown user, dependency failure and successful issuance | Validate user-visible HTTP contracts | Add Testcontainers-backed PostgreSQL/Redis tests and property/fuzz tests |
| `database/init.sql` | Made table/seed setup idempotent and added unique/lookup constraints/indexes | Reproducible local/Kubernetes demo setup and efficient joins | Use Alembic/Flyway or another migration system for versioned production schema evolution |

## Runtime, build and delivery

| Area / file | What changed | Why | Alternative considered |
|---|---|---|---|
| `python-service/Dockerfile` | Multi-stage Python 3.12 slim image; runtime-only dependencies; non-root UID 10001; health check | Smaller runtime image and reduced container privileges | Distroless Python for a smaller attack surface, at the cost of harder debugging/native library management |
| `golang-service/Dockerfile` | Multi-stage Go 1.27.4 build; stripped static binary; distroless non-root image | Removes build toolchain and shell from the runtime image | Alpine runtime is easier to debug but carries more packages/surface area |
| `docker-compose.yml`, `.env.example` | Local stack with PostgreSQL, Redis, both apps, Collector, Prometheus, Loki, Tempo and Grafana; named volumes, isolated networks, health checks | Reproducible end-to-end local demonstration | Tilt/Skaffold for Kubernetes-native development; not necessary for this exercise |
| `k8s/base/*.yaml` | Independent app Deployments/Services, CloudNativePG, Redis/Sentinel, ConfigMaps/Secret refs, probes, resource bounds, HPA, PDB and NetworkPolicies | Demonstrates scheduling, service discovery, dependency health and least-privilege network access | Helm is an alternative for reusable multi-environment packaging; managed PostgreSQL/Redis may be preferable in production |
| `k8s/argocd-application.example.yaml` | Added dev, staging and production Argo CD Application examples plus matching overlays | Demonstrates environment promotion and deployment independence | Flux is an equivalent alternative; repository credentials and approval policy remain required |
| `.github/workflows/ci.yml` | Python tests, Go tests/vet, Docker builds, Compose smoke tests, Kustomize/kubeconform validation and Trivy scanning | Catches application, container and manifest regressions before merge | Add live cluster tests, SBOM/provenance, image signing and registry publication for release pipelines |

## Observability changes

| Area / file | What changed | Why | Alternative considered |
|---|---|---|---|
| `observability/otel/collector.yaml` | OTLP receivers, memory limiter, batching, Tempo trace export and Loki OTLP log export | Centralises telemetry routing and applies basic backpressure protection | Grafana Alloy/Fluent Bit for log collection, or vendor-specific agents in an existing platform |
| `observability/prometheus/*` | Compose/Kubernetes scrape configs plus availability/error/latency/saturation alerts | Exposes RED/dependency metrics and gives actionable example thresholds | Prometheus Operator/ServiceMonitors and recording rules for a larger cluster |
| `observability/grafana/*` | Provisioned Prometheus/Loki/Tempo datasources and 13-panel dashboard | Makes the live demo repeatable and gives a shared operational view | Managed Grafana/CloudWatch dashboards if GEANT has a standard platform |
| `observability/loki/loki.yaml`, `observability/tempo/tempo.yaml` | Loki log backend and Tempo trace backend configured with demo retention/storage | Meets the log aggregation and tracing requirements with Grafana integration | EFK/OpenSearch, Jaeger, or managed cloud observability |

## Important deviations / deliberate omissions

1. **No Redis queue existed in the provided code.** The original Go service used Redis as an expiring key/value store (`SET`/`GET`), not a queue. We measured real Redis command latency, errors, key count and pool saturation instead of inventing queue depth. If the requirement is interpreted literally as requiring queue telemetry, introduce a genuine Redis Stream/list and worker; then instrument queue length, pending/consumer lag, oldest-message age and message-processing duration. Do not label token-key count as queue depth.
2. **Data stores are demo and HA-capable variants.** The Docker Compose stack uses a single local PostgreSQL and Redis instance for reproducibility. Kubernetes uses CloudNativePG and Redis Sentinel for HA, while backup, restore and failover remain operational prerequisites.
3. **Alert delivery is now scaffolded.** Prometheus evaluates rules and can send alerts to Alertmanager. Replace the placeholder webhook and test real notification delivery.
4. **No Service Mesh.** It is intentionally not included because it adds operational overhead to a two-service exercise. Add Istio/Linkerd only for concrete mTLS, traffic policy or platform standard requirements.
5. **GitOps environment overlays are included.** Argo CD examples target dev, staging and production overlays. Repository credentials, promotion approval and image policy remain deployment configuration.
6. **No public Ingress/TLS certificate.** The manifests expose internal Services. Configure the cluster's ingress/gateway, TLS, authentication/rate limits and security policies before public access.

## Validation boundary

Static validation performed for this archive: Python compilation, Go formatting/parser, 30 YAML/YML parses, 2 JSON parses and 19 Kustomize input/resource path checks. This environment did not have Docker, kubectl, PostgreSQL/Redis runtime dependencies, the Python DB/OTel packages or Go 1.24+ locally. As a result, image builds, tests requiring dependencies, `go test`/`go vet`, live Compose/Kubernetes startup, actual traces/log correlation and alert evaluation are **not claimed as executed**. See `VALIDATION.md` for the exact commands to run before the interview submission.
