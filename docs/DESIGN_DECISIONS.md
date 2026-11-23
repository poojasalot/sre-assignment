# Design decisions, trade-offs and alternatives

This document records the reasoning behind the implementation as an SRE lead would present it at design review.

## 1. Containerization

| Choice | Why | Alternative and trade-off |
|---|---|---|
| Python multi-stage image, slim runtime | Dependencies are installed in a builder layer; runtime excludes build tools, runs as UID 10001 and contains only app files and venv | `python:alpine` can reduce size but native Python wheels / musl compatibility can be harder; distroless Python is smaller but needs more work for certificates, debugging and native libs |
| Go multi-stage image, distroless runtime | Compile static binary in Go 1.27.4 builder; final image has no shell/package manager and runs non-root | Alpine final image is easier to debug and offers shell tools but increases attack surface; distroless debug variants can be used for incident-only troubleshooting |
| Compose for local development | Reproducible local dependency and observability topology | Tilt / Skaffold is useful for iterative Kubernetes development but is unnecessary for a 4-6 hour exercise |

## 2. Application reliability

- PostgreSQL connections are pooled and bounded. Pool acquisition has a timeout so requests fail clearly under saturation instead of waiting indefinitely. The pool maximum must be coordinated with replica count and database `max_connections`.
- A single gRPC channel is reused by the Python process. Per-request channel creation wastes sockets and connection establishment time. A per-RPC deadline limits resource occupancy when Go or Redis is degraded.
- The Go service distinguishes invalid input, missing token, and dependency outage. A missing Redis key means an invalid/expired token; a Redis network error is a service failure and returns gRPC `Unavailable`.
- Liveness checks whether a process can serve health traffic; readiness checks mandatory dependencies. Dependency failure should remove the pod from Service endpoints rather than cause repeated liveness restarts.
- Shutdown closes the gRPC server gracefully, stops HTTP health/metrics server, closes Redis and flushes telemetry within a bounded timeout.

**Alternative:** use async SQLAlchemy + asyncpg with FastAPI async handlers if the service is redesigned around async I/O. The current synchronous psycopg2 handler is valid because FastAPI runs synchronous endpoints in its thread pool; do not mix blocking psycopg2 calls directly into `async def` endpoints.

## 3. Telemetry architecture

| Concern | Current implementation | Alternative |
|---|---|---|
| Metrics | Prometheus client metrics with explicit low-cardinality RED and dependency metrics | OpenTelemetry Metrics SDK + Collector Prometheus exporter can give a single vendor-neutral instrumentation API, but is more setup for this small starter |
| Logs | JSON stdout plus OTLP log export with trace context | Fluent Bit / Grafana Alloy / Vector tailing container logs is a good fit when applications should only write stdout; direct OTLP avoids runtime log scraping configuration here |
| Traces | OpenTelemetry SDK/instrumentations → Collector → Tempo; custom Redis client spans | Jaeger is equally valid and was explicitly offered in the assignment; Tempo integrates naturally with Grafana and supports object storage in production. Custom Redis spans record only operation/system attributes, not keys or values |
| Log backend | Loki | OpenSearch/EFK provides richer search/analytics but has materially higher operational overhead; CloudWatch is attractive for AWS-hosted workloads |
| Collector topology | One Collector for the small demo | In production, separate gateway Collectors (central export, tail sampling) and agent/DaemonSet Collectors (node/pod collection) for scale, isolation and resilience |

Trace propagation uses W3C Trace Context. Trace and log resource metadata include service name, service version and deployment environment. Redis spans are intentionally manual/sanitised: automatic command instrumentation can capture key/value arguments such as opaque tokens or user identifiers, so only the Redis operation name is recorded. Metric labels avoid user email, token, raw URL path, trace ID and other unbounded/high-cardinality values.

## 4. Redis queue metric decision

The starter's Redis usage is `SET` with TTL and `GET`; there is no queue, list, stream or worker. A queue-depth metric is not semantically valid. The implementation exposes:

- `redis_operations_total{operation,status}`
- `redis_operation_duration_seconds{operation}`
- `redis_db_keys` (Redis DB 0 key count; approximate overall datastore occupancy, not token-only count)

If a Redis Stream is added, use `XINFO GROUPS` / pending-entry metrics and oldest pending age. If a Redis List queue is added, use `LLEN`, dequeue/ack throughput, retry/dead-letter counts and processing-duration histograms. The choice depends on delivery guarantees: Streams provide consumer groups, pending-entry tracking and replay; Lists are simpler but require more custom acknowledgement/retry handling.

## 5. Token storage alternatives

Current design uses opaque random tokens stored server-side in Redis with TTL. It supports immediate revocation by deleting a key and avoids embedding claims in a client-readable token, but every validation depends on Redis availability and adds a network lookup.

Alternative options:

1. **Signed JWT access token + short expiry:** stateless verification at services, lower lookup load; revocation is harder and signing key rotation / audience / issuer / algorithm validation become critical.
2. **Opaque access token + introspection/cache:** central revocation and policy control; introspection/cache introduces availability and latency trade-offs.
3. **Redis HA / managed cache:** preserves current model while improving availability; still requires TTL policy, memory sizing, eviction strategy and backup/recovery design.

For a real identity service, do not use the demo's random hex token format as a full protocol. Use a reviewed OAuth2/OIDC design, key management, audience/scope checks, rate limits, abuse controls and security review.

## 6. Kubernetes choices

- Kustomize was selected over Helm to keep a small assignment's rendered resources inspectable and avoid chart templating overhead. Helm is a better fit for reusable multi-environment packaging and versioned dependencies.
- Deployments run the stateless Python and Go services with independent replicas. Kubernetes PostgreSQL uses CloudNativePG and Redis uses StatefulSets plus Sentinel. Backup, restore, failover and capacity evidence remain required before production.
- Requests and limits are explicit; HPAs use CPU as an initial scale signal. Production autoscaling should consider request rate, in-flight work, queue depth (if a real queue exists), p95 latency and downstream saturation as well.
- NetworkPolicies restrict ingress paths. They only work when the cluster CNI enforces them. The sample assumes an `ingress-nginx` namespace for inbound HTTP and a metrics-server for HPA.
- PDBs preserve at least one pod during voluntary disruptions, but do not replace multi-zone placement, capacity planning or graceful node failure design.

## 7. Bonus scope: what was and was not implemented

**Implemented:** CI test/build workflow, HPA, PDB, basic SLO/error-budget alert expressions, and NetworkPolicies.

**Not implemented intentionally:** Istio/Linkerd. They add sidecars, certificates and policy overhead without a concrete traffic-management or mesh requirement in this assignment. Argo CD examples and dev/staging/production overlays are implemented, but repository credentials, promotion approval and drift reconciliation remain platform integration work.
