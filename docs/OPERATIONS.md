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
| Audit queue freshness | 99% of audit events acknowledged within 60 s | `queue_message_processing_duration_seconds` plus oldest pending age; `AuditQueueStalled` fires at 300 s |
| Telemetry pipeline | No sustained export loss | `OtelCollectorExportFailures` alerts on failed span and log exports. The demo Collector batches in memory |

A 99.9% monthly availability target permits approximately 43.8 minutes of downtime in a 30.4-day month. Do not declare an SLO until ownership, eligible traffic, maintenance policy and the measurement window are agreed.

## 2. Dashboard panels

The provisioned dashboard is deliberately oriented around RED plus dependency saturation:

1. **SLI:** 1-hour availability for the Python API and Go `MintToken` against the 99.9% target.
2. **Rate:** HTTP requests per second by route; gRPC calls per second by method and status.
3. **Errors:** HTTP 5xx ratio; gRPC server-error ratio; token mint errors. Client errors (4xx, `InvalidArgument`, `NotFound`) are excluded from the SLI.
4. **Duration:** HTTP and gRPC p50/p95; Redis operation p95; PostgreSQL connection acquisition p95.
5. **Saturation:** DB connections checked out against the maximum; Redis pool timeouts; Redis keys.
6. **Audit queue:** depth (lag + pending), oldest pending age, processing p95, processed per second by status.
7. **Redis server:** per-pod `redis_up`, current primary, connected clients and replicas (from `redis_exporter`).
8. **Golden path:** header links to Explore. Loki log lines link to their Tempo trace, and Tempo spans link back to Loki.

In a production cluster, add pod CPU and memory, HPA replica counts and node metrics from kube-state-metrics and cAdvisor.

## 3. Alert rules

All ratios come from recording rules (`job:http_server_error_ratio:rate{5m,30m,1h,6h}` and
`job:grpc_server_error_ratio:rate…`), so dashboards and alerts share one definition. The numerator counts
only server failures: HTTP 5xx, and the gRPC codes `Internal`, `Unavailable`, `DeadlineExceeded`, `Unknown`
and `ResourceExhausted`. Client errors such as `InvalidArgument` and `NotFound` never burn budget.
`observability/prometheus/alert-rules.test.yml` unit-tests the key behaviours: a 5% error rate fires the fast
burn, 404s never burn budget, missing targets fire `absent()` alerts, and a stalled queue fires.

| Alert | Condition | For | Severity | First response |
|---|---|---:|---|---|
| `PythonAvailabilityFastBurn` / `GoGrpcAvailabilityFastBurn` | 1h **and** 5m error ratio > 14.4 × 0.1% (2% of the monthly budget gone in 1 h) | 2m | critical | Check the deploy timeline and roll back if correlated; open traces for failing requests |
| `PythonAvailabilitySlowBurn` / `GoGrpcAvailabilitySlowBurn` | 6h **and** 30m error ratio > 6 × 0.1% | 10m | warning | Find the sustained regression before the budget is exhausted |
| `PythonHighP95Latency` | p95 > 500 ms | 10m | warning | Compare pool acquire p95, SQL span, gRPC and Redis span durations |
| `GoGrpcHighP95Latency` | p95 > 100 ms | 10m | warning | Check Redis p95, Redis CPU, and pool timeouts |
| `PythonServiceTargetDown` / `GoServiceTargetDown` | `up == 0` | 2m | critical | Check pods, restarts, rollout, the NetworkPolicy and the metrics port |
| `PythonServiceAbsent` / `GoServiceAbsent` | No healthy target exists at all (`absent(up == 1)`) | 5m | critical | The Deployment was deleted, scaled to zero, or discovery is broken. A plain `up == 0` alert is silent in this case |
| `DatabasePoolNearExhaustion` | in use / max > 80% | 5m | warning | Check concurrency and slow SQL. Do not just raise the pool size (replicas × pool must stay under `max_connections`) |
| `DatabasePoolAcquireFailures` | any acquire error in 5m | 1m | critical | Check PostgreSQL sessions, locks and the CNPG primary |
| `PostgresExporterDown` | `cnpg_collector_up == 0` | 2m | critical | Check the CNPG instance pods and the operator |
| `RedisDown` | `redis_up == 0` on any pod | 1m | critical | Check the Redis pod. If it is the primary, confirm Sentinel promoted a replica |
| `RedisOperationP95High` | client-side p95 > 50 ms | 10m | warning | Check Redis CPU, memory, evictions, AOF rewrite and the network |
| `RedisPoolAcquisitionTimeouts` | any increase in 5m | 1m | warning | Check pool size against `maxclients` and per-replica connections |
| `TokenMintFailureSpike` | mint error ratio > 1% | 5m | critical | Look at gRPC status codes and Redis command outcomes |
| `AuditQueueBacklog` | max(lag) + max(pending) > 1000 | 10m | warning | Consumers are not keeping up: check processing p95 and consumer errors |
| `AuditQueueStalled` | oldest pending message > 300 s | 5m | warning | Dead consumers are reclaimed after 30 s, so *no* consumer is acknowledging: check the Go pods and `audit_ack_failed` logs |
| `OtelCollectorExportFailures` | span or log export failures | 10m | warning | Check Loki and Tempo health and the Collector logs. Telemetry is being dropped |

Alertmanager groups alerts by `alertname`, `service` and `namespace` and routes them to a single placeholder webhook
(`https://example.invalid/...`). Before go-live: route critical alerts to the 24/7 on-call, route warnings to
the service channel, add inhibition (for example `RedisDown` suppressing `TokenMintFailureSpike`), and test
delivery end to end.

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

- Go readiness becomes false when Redis Ping fails. Liveness stays healthy, so Kubernetes does not restart healthy processes just because Redis is down.
- Ask Sentinel who the primary is: `kubectl exec redis-sentinel-0 -- redis-cli -p 26379 sentinel get-master-addr-by-name mymaster`. Compare that with `redis_instance_info{role="master"}` on the dashboard. A failover completes within about 5 s (down-after) plus the election time. The Go client follows it automatically.
- A restarted pod asks Sentinel for the primary before starting, so a former primary rejoins as a replica. If no Sentinel answers, it falls back to `redis-0`. Check the Sentinel pods first if roles look wrong.
- Then check CPU and memory, connections against `maxclients`, evictions, AOF rewrite and the network.
- Check the retry and backoff budget and the gRPC caller deadlines. Retries must be bounded. Do not blindly retry token mints without deciding how duplicate issuance should behave.

### Audit queue backlog or stalled

- Backlog = `max(redis_stream_lag_messages) + max(redis_stream_pending_messages)`. Lag that grows while pending stays flat means consumers are not reading: check that the Go pods are running and look for `audit_consumer_failed` logs. Pending that grows means messages are read but not acknowledged: look for `audit_ack_failed`.
- Messages a crashed pod leaves pending are reclaimed by a live consumer after 30 s (`XAUTOCLAIM`). An oldest pending age above 300 s therefore means no healthy consumer exists.
- Inspect directly: `XINFO GROUPS token:events` and `XPENDING token:events audit`. The stream is capped at about 10,000 entries, so a long consumer outage drops the oldest *unread* events. Restore consumers before the lag approaches that cap.
- Token issuance is unaffected because publishing is best-effort. `redis_operations_total{operation="xadd",status="error"}` shows events lost at publish time.

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

- [x] Define dev/staging/production Kustomize overlays (`k8s/overlays/`).
- [ ] Define the promotion workflow; pin immutable image digests.
- [x] GitOps reconciliation with drift correction (Argo CD `selfHeal` for dev and staging; manual sync for production).
- [ ] Production approvals and rollback procedure on top of the Argo CD Applications.
- [ ] Add canary/blue-green rollout, automated smoke tests, rollback gates and database backward-compatible migration strategy.
- [ ] Define ownership, on-call, escalation, incident severity, postmortem and operational readiness review.

## 6. Capacity planning notes

- The Python pool max is 10 by default in Compose and 8 in Kubernetes. At 10 Python replicas, that can mean up to 80 PostgreSQL connections before other clients. Confirm DB capacity and set pool limits with headroom.
- Go Redis client uses a pool size of 50 per Go process; with 10 replicas, potential client connections can approach 500. Validate against Redis `maxclients` and connection usage.
- HPA CPU 70% is an initial scale trigger, not a performance SLO. Test burst, steady-state and failure-mode load. Scale on useful demand signals, but avoid autoscaling on a noisy latency metric that can create positive feedback.
- Resource limits are starting values. Benchmark actual RSS/CPU and adjust after representative workload tests; OOMKills and throttling are operational signals.
- The audit stream holds at most about 10,000 entries. At sustained peak mint rate R per second, that covers roughly 10,000 / R seconds of consumer outage before unread events are trimmed. Size the cap from the measured peak rate.
