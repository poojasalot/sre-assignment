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
- Shutdown closes the gRPC server gracefully, stops the HTTP health and metrics server, drains the audit consumer, closes Redis and flushes telemetry within a bounded timeout.
- gRPC TLS fails closed. If any `GRPC_TLS_*` certificate setting is present but cannot be loaded (bad path or invalid CA), the Go service exits with `grpc_tls_config_invalid` instead of silently serving plaintext. Plaintext is used only when no TLS settings are given, which is the local demo, and the service logs this at startup (`grpc_transport=plaintext_demo`).

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

## 4. Redis audit queue

The starter used Redis only as a key/value store, so it had no queue whose depth could be measured. The
assignment asks for queue depth and processing time, so the Go service now has a real asynchronous workload:
every minted token publishes a `token_minted` audit event (permission count, TTL, `traceparent`; no token or
email) to the `token:events` Redis Stream. A consumer group, `audit`, runs in every Go replica and processes
the events.

| Decision | Reason | Alternative |
|---|---|---|
| A Stream rather than a List | Consumer groups, per-message acknowledgement, a pending-entries list and replay give at-least-once delivery and make *lag* and *pending* measurable | A List with `BRPOPLPUSH` needs hand-written acknowledgement, retry and visibility tracking |
| Publishing is best-effort | An audit outage must not fail token issuance. Failures are logged (`audit_event_publish_failed`) and counted by `redis_operations_total{operation="xadd",status="error"}` | A transactional outbox in PostgreSQL would guarantee delivery, at the cost of an extra write on the hot path |
| `XAUTOCLAIM` after 30 s idle | Consumer names are pod hostnames, so a crashed pod's pending messages would otherwise never be acknowledged | Fixed consumer names on a StatefulSet; more coupling, no benefit |
| `MAXLEN ~ 10000` | Bounds Redis memory. Acknowledged history is not needed after processing | Time-based trimming with `MINID` |
| Trace context in the message | The consumer span `audit.process` joins the request's trace, so asynchronous work appears in the same Tempo trace | Span links, which keep separate traces |

Depth and age metrics are read at scrape time from Redis (`XLEN`, `XINFO GROUPS`, `XPENDING`), so every
replica reports the same group-wide numbers. Dashboards and alerts therefore aggregate with `max()`, not
`sum()`. The total backlog is `lag + pending`. Processing time is the histogram
`queue_message_processing_duration_seconds`, measured per message from receipt to `XACK`.

The original Redis client metrics remain: `redis_operations_total`, `redis_operation_duration_seconds`,
`redis_pool_*` and `redis_db_keys`. Server-side metrics such as `redis_up`, role, memory, clients and
replication come from `redis_exporter`.

## 5. Token storage alternatives

Current design uses opaque random tokens stored server-side in Redis with TTL. It supports immediate revocation by deleting a key and avoids embedding claims in a client-readable token, but every validation depends on Redis availability and adds a network lookup.

Alternative options:

1. **Signed JWT access token + short expiry:** stateless verification at services, lower lookup load; revocation is harder and signing key rotation / audience / issuer / algorithm validation become critical.
2. **Opaque access token + introspection/cache:** central revocation and policy control; introspection/cache introduces availability and latency trade-offs.
3. **Redis HA / managed cache:** preserves current model while improving availability; still requires TTL policy, memory sizing, eviction strategy and backup/recovery design.

For a real identity service, do not use the demo's random hex token format as a full protocol. Use a reviewed OAuth2/OIDC design, key management, audience/scope checks, rate limits, abuse controls and security review.

## 6. Kubernetes choices

- **Kustomize over Helm.** The rendered resources stay inspectable for a small platform. Helm fits reusable,
  versioned packaging better.
- **One source of truth for config.** `k8s/platform` generates its ConfigMaps directly from
  `observability/` and `database/`, the same files Compose mounts. This removes the class of bug where a
  copied Kubernetes config drifts from the tested one. The cost is that rendering needs
  `--load-restrictor LoadRestrictionsNone`, which must also be set in Argo CD's `argocd-cm`. Generated
  ConfigMaps carry content hashes, so a change rolls out the pods that use it. The schema ConfigMap keeps a
  stable name, because Kustomize cannot rewrite the reference inside the CNPG `Cluster` custom resource,
  and CNPG reads it only at bootstrap anyway.
- **Separate roots** for the platform and each app allow independent reconciliation. Overlays compose them
  per environment.
- **Stateful dependencies through proven patterns.** PostgreSQL runs on CloudNativePG, which handles
  failover, the `-rw` Service, generated credentials and metrics. Redis runs as a StatefulSet with AOF on
  PVCs plus three Sentinels using hostname announcements. A Redis pod discovers the current primary from
  Sentinel before starting, so a restarted former primary rejoins as a replica instead of creating a
  split-brain.
- **Replica counts belong to the HPA.** Deployments omit `replicas`, so re-applying manifests never fights
  the autoscaler. HPAs scale on CPU as an initial signal. Production should add request rate or queue lag.
- **Default deny in both directions.** Every flow, including DNS, the Kubernetes API (for Prometheus
  discovery and the CNPG instance manager) and the CNPG operator, has an explicit policy. kind's CNI enforces
  them, so the e2e run exercises these policies.
- **Prometheus RBAC is a namespaced Role.** Service discovery is limited to its own namespace
  (`own_namespace: true`), so environments sharing a cluster cannot see each other.
- **Environment overlays own sizing.** Dev has the smallest CPU/memory profile and an app HPA ceiling of 3;
  staging has an intermediate profile and ceiling of 5; production keeps the shared base sizing and raises
  the app HPA floor to 3. Dev also relaxes required anti-affinity and runs one PostgreSQL instance; staging
  and production keep the HA topology.

## 7. Bonus scope

**Implemented:**

- CI with config, schema, image and end-to-end checks.
- HPAs and PDBs.
- Multi-window burn-rate SLO alerts with rule unit tests.
- Default-deny NetworkPolicies.
- Argo CD Applications per environment.

**Not implemented, intentionally:** Istio or Linkerd. They add sidecars, certificates and policy overhead
without a traffic-management requirement here. The Go service supports application-level mTLS and refuses to
start if TLS is configured incorrectly.
