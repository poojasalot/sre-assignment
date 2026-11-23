# SRE operations guide: SLIs, SLOs, thresholds and go-live checklist

## 1. Proposed service objectives (initial hypotheses)

These are **starting targets for the assignment**, not values measured from load testing or agreed with GEANT. Set final objectives from user journeys, traffic, dependency contracts and an agreed error-budget policy.

| Service indicator | Initial target | Measurement / notes |
|---|---|---|
| Availability | 99.9% monthly for `POST /token` and Go `MintToken` | Count successful responses and expected client errors separately from server/dependency failures. Exclude malformed requests and unknown users from availability errors; include 5xx, timeouts and dependency-induced failures |
| Python API latency | p95 < 500 ms; p99 < 1 s | Measure end-to-end server duration. Establish a baseline with realistic concurrency and a warm database/cache |
| Go MintToken latency | p95 < 100 ms | Measure gRPC server duration; refine after Redis and network baseline tests |
| Token issuance failures | < 0.1% of valid issuance attempts | Use application-level outcomes; separate invalid/unknown user from internal failures |
| Dependency health | PostgreSQL pool below 80% sustained utilisation; Redis command p95 < 50 ms | Saturation/latency are diagnostic indicators; avoid paging on a single short spike |
| Telemetry pipeline | no persistent export loss; telemetry pipeline availability monitored | Alert on Collector queue/retry/drop metrics in production; demo Collector currently uses in-memory batching |

A 99.9% monthly availability target permits approximately 43.8 minutes of downtime in a 30.4-day month. Do not declare an SLO until ownership, eligible traffic, maintenance policy and the measurement window are agreed.

## 2. Dashboard panels

The provisioned dashboard is deliberately oriented around RED plus dependency saturation:

1. **Rate:** HTTP requests/sec grouped by route; gRPC calls/sec grouped by full method/status.
2. **Errors:** HTTP 5xx ratio; gRPC non-OK ratio and token mint errors. Break client input errors away from server errors before computing SLO availability.
3. **Duration:** HTTP p50/p95; Redis operation p95; PostgreSQL connection acquisition p95.
4. **Saturation:** DB connections checked out / max; Redis key count; pod CPU/memory and HPA replica count should be added from kube-state-metrics / cAdvisor in a production cluster.
5. **Dependencies:** database query failures, Redis operation error rate/latency, health/readiness state.
6. **Golden path:** a dashboard link to Tempo trace search and Loki queries filtered by service/trace ID.

## 3. Alert rules currently included

| Alert | Threshold | For | Severity | Rationale / first response |
|---|---:|---:|---|---|
| App target down | Prometheus `up == 0` | 2 min | Critical | Check pods/endpoints, restarts, deployment rollout, network policy and metrics endpoint |
| Python fast burn | 5m and 1h error ratios > 1.44% (14.4x burn for 99.9%) | 2 min | Critical | Check traces/logs by route and dependency; consider rollback or traffic shed |
| Python slow burn | 30m and 6h error ratios > 0.6% (6x burn) | 10 min | Warning | Find sustained dependency/performance regression before budget is exhausted |
| Go gRPC fast burn | 5m and 1h server-failure ratios > 1.44% | 2 min | Critical | Check Redis spans/errors, gRPC status, latency and resource pressure |
| Python 5xx ratio | > 2% | 5 min | Warning | Triage errors by endpoint and trace |
| Python p95 | > 500 ms | 10 min | Warning | Inspect DB pool acquire, SQL span, Go RPC and Redis command duration |
| DB pool saturation | > 80% | 5 min | Warning | Check concurrency, slow SQL and pool/database sizing; do not blindly increase pool size |
| DB pool acquire timeout | any increase in 5m | 1 min | Critical | Check pool in-use/max and PostgreSQL sessions; reduce load or correct leak/query latency |
| Redis p95 | > 50 ms | 10 min | Warning | Check Redis CPU/memory/network, evictions, persistence and connection pool |
| Redis pool timeouts | Any increase in 5m | 1 min | Warning | Check pool usage, `maxclients`, network latency and per-replica connection sizing |
| Token mint error ratio | > 1% | 5 min | Critical | Inspect Go gRPC error code and Redis command outcome |

**Important:** Prometheus evaluates these rules and exposes firing alerts in its Alerts page. The assignment ZIP does not configure a real PagerDuty/Slack/email receiver because credentials and GEANT's on-call routing were not provided. Before go-live, add Alertmanager (or your platform's alert router), route critical alerts to the 24/7 on-call, warnings to the service channel, configure inhibition/grouping, and test delivery end to end.

The initial 99.9% burn-rate rule is an illustrative multi-window pattern. Before production, use recording rules and validate alert calculations with test traffic; distinguish valid 4xx responses (e.g. user not found) from server failures in the SLI numerator/denominator.

## 4. Example investigation runbooks

### Elevated HTTP 5xx or high p95

1. Identify affected route and pod; compare the start time with deployment events.
2. Select a representative trace in Tempo. Follow Python → PostgreSQL → gRPC → Go → Redis spans.
3. In Loki, search Python and Go logs using the same trace ID. Avoid searching/logging raw token or PII.
4. Check `db_pool_connections_in_use / db_pool_connections_max`, pool acquire p95, DB query errors and Redis p95.
5. If a recent release correlates with regression, stop rollout/rollback. Otherwise mitigate the saturated dependency, apply traffic shaping and preserve enough capacity for recovery.

### PostgreSQL unavailable / pool exhausted

- Check PostgreSQL readiness, connection count, locks, slow queries, storage and failover events.
- Compare application replicas × pool max with database `max_connections`, reserving slots for migrations, monitoring and admin access.
- Reduce request concurrency or scale database capacity based on evidence. Increasing app pool size can make database overload worse.
- Verify reconnect and failover behavior; test stale/broken connections and pool recovery.

### Redis unavailable / slow

- Go readiness becomes false when Redis Ping fails; liveness remains healthy so Kubernetes does not restart healthy processes solely because Redis is down.
- Check managed Redis failover, node CPU/memory, connections, evictions, persistence and network.
- Validate retry/backoff budget and gRPC caller deadlines. Retries must be bounded and idempotent; do not blindly retry token mint calls without understanding duplicate issuance semantics.

### Telemetry backend/Collector failure

- App request handling should not depend synchronously on Loki/Tempo availability; exporters batch telemetry asynchronously.
- Check Collector memory limiter, export failures/retries, receiver saturation and backend health.
- Set explicit queue limits, retry policy and persistent sending queue in production. Monitor dropped spans/logs and telemetry cost/cardinality.

## 5. Production go-live checklist (not all implemented in this assignment)

### Platform and resilience

- [ ] Run PostgreSQL and Redis as managed HA services or supported multi-zone clusters; configure failover and tested point-in-time restore / backup retention.
- [ ] Set PostgreSQL `max_connections`, statement timeout, slow-query logging, vacuum/maintenance policy and connection budget across all app replicas.
- [ ] Define Redis memory/eviction policy, key TTL distribution, capacity alarms, replication/failover, persistence expectations and token revocation semantics.
- [ ] Spread replicas across zones/nodes with topology spread constraints and anti-affinity; ensure HPA scale-out has spare cluster capacity and Cluster Autoscaler / node autoscaling is configured.
- [ ] Configure ingress/gateway with TLS, request size/time limits, rate limits, authentication/authorization, WAF and DDoS protections.
- [ ] Use internal TLS or service-mesh mTLS for Python→Go and app→data connections; enforce certificate rotation and identity policy.
- [ ] Add pod security admission / policy enforcement, signed images, SBOM, vulnerability scanning, provenance and admission policy.
- [ ] Use external secrets manager, encryption at rest, least-privilege IAM/service accounts, credential rotation and audit trails.
- [ ] Add backups and restore tests for Grafana/Prometheus/Loki/Tempo state; configure object storage and appropriate retention in production.
- [ ] Run failure-injection/game days for DB outage, Redis failover, Collector outage, pod/node loss, DNS problems and zone evacuation.

### Application and API

- [ ] Add API authentication/authorization and rate limiting; enforce request/response schemas and maximum request sizes.
- [ ] Decide whether email identity is case-insensitive and normalise consistently with DB constraints.
- [ ] Add idempotency semantics for retried token issuance; clarify whether multiple active tokens per user are allowed.
- [ ] Add token revocation/rotation, audience/scope semantics, secure storage and threat modelling. Avoid treating the demo random opaque token as a production OAuth/OIDC implementation.
- [ ] Validate permissions against policy tests and add negative/tenant-isolation tests.
- [ ] Add SQL migration tooling (e.g. Alembic/Flyway) rather than only first-run init SQL.
- [ ] Set server-side max request concurrency, graceful drain, gRPC max message sizes and explicit timeout budgets end to end.
- [ ] Add integration tests with real PostgreSQL/Redis (Testcontainers), contract tests for protobuf compatibility and load tests (k6/Locust/ghz).

### Observability and incident management

- [ ] Agree SLOs with service owners and users; define SLIs, exclusions, monthly window and error-budget policy.
- [ ] Configure Alertmanager / PagerDuty / Opsgenie / Slack with tested routes, escalation, deduplication, inhibition and runbooks.
- [ ] Add Kubernetes/node/pod metrics, black-box synthetic checks, database/cache native metrics and Collector self-observability.
- [ ] Add trace sampling strategy (head sampling for baseline, tail sampling for errors/slow traces where justified) and retention/cost controls.
- [ ] Establish log retention, PII/token redaction, access control and audit trails.
- [ ] Set cardinality budgets and review every metric label; never label metrics by email, token, request ID or trace ID.
- [ ] Create dashboards for deployment health, saturation, HPA, data stores, SLO/error budget and telemetry pipeline health.

### Delivery and ownership

- [ ] Define dev/stage/prod Kustomize overlays and promotion workflow; pin immutable image digests.
- [ ] Use GitOps (ArgoCD/Flux) or controlled deployment pipeline with approvals, drift detection and rollback.
- [ ] Add canary/blue-green rollout, automated smoke tests, rollback gates and database backward-compatible migration strategy.
- [ ] Define ownership, on-call, escalation, incident severity, postmortem and operational readiness review.

## 6. Capacity planning notes

- The Python pool max is 10 by default in Compose and 8 in Kubernetes. At 10 Python replicas, that can mean up to 80 PostgreSQL connections before other clients. Confirm DB capacity and set pool limits with headroom.
- Go Redis client uses a pool size of 50 per Go process; with 10 replicas, potential client connections can approach 500. Validate against Redis `maxclients` and connection usage.
- HPA CPU 70% is an initial scale trigger, not a performance SLO. Test burst, steady-state and failure-mode load. Scale on useful demand signals, but avoid autoscaling on a noisy latency metric that can create positive feedback.
- Resource limits are starting values. Benchmark actual RSS/CPU and adjust after representative workload tests; OOMKills and throttling are operational signals.

## Review follow-up status

The repository has implemented these principal-review improvements:

- Separate Kustomize roots for Python, Go and shared platform resources.
- ConfigMap hash suffixes instead of `disableNameSuffixHash: true`.
- Preferred topology spreading and pod anti-affinity for Python and Go.
- Default-deny ingress and egress with explicit dependency and observability allowlists.
- Redis exporter metrics for Compose and Kubernetes.
- Optional application-managed gRPC mTLS with a plaintext local-demo fallback.
- CI Kubernetes render/schema validation.

The following items remain production prerequisites: managed HA data services, tested restore/failover, external secret management, certificate rotation, actual on-call routing, integration/load tests, database migrations and service/protobuf contract governance.

## HA and GitOps implementation status

- PostgreSQL is deployed through CloudNativePG as a three-instance cluster. Install and monitor the operator before applying the platform root.
- Redis runs as three data replicas with three Sentinel processes. Validate automatic promotion and update application connection settings during failover drills.
- Dev, staging and production overlays isolate namespaces and environment image/replica settings.
- Argo CD can reconcile each environment independently; production automation should require approval and image-digest promotion.
- Database initialization is executed by a versioned migration Job; migration changes must be backward compatible before application rollout.
- Alertmanager receives Prometheus notifications, but the placeholder webhook must be replaced with an approved GEANT notification route.
