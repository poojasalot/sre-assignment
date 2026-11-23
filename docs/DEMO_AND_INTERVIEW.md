# Live demo and SRE lead interview guide

## 1. Suggested 5-7 minute live demo

**1. Explain the service path (60 seconds).**

- Two services have clear ownership boundaries: FastAPI handles the REST interface and PostgreSQL permission lookup; Go owns token generation and Redis persistence.
- The Python service calls Go over gRPC. W3C Trace Context is propagated across HTTP, PostgreSQL, gRPC and Redis.
- PostgreSQL and Redis are dependencies; service readiness is tied to required dependencies, while liveness is process-only.

**2. Show the request works (60 seconds).**

```bash
curl -i -X POST http://localhost:8000/token \
  -H 'Content-Type: application/json' \
  -d '{"user_email":"alice@example.com"}'

curl -i -X POST http://localhost:8000/token \
  -H 'Content-Type: application/json' \
  -d '{"user_email":"unknown@example.com"}'
```

Explain 200 for a known user, 404 for an unknown user, 422 for invalid input, and 503/504 for dependency errors/timeouts.

**3. Show metrics (60-90 seconds).**

Open Grafana's SRE dashboard and point out request rate, 5xx ratio, p95 latency, pool saturation and Redis latency. Generate several requests so rate panels have samples. Prometheus histograms need observations before percentile graphs become meaningful.

**4. Show trace and correlated logs (90 seconds).**

- Run `./scripts/validate-observability.sh`. It sends a request with a trace ID it generated, then proves that
  Tempo has the full trace and that one Loki query returns logs from both services. Use the trace ID it prints.
- In Tempo, open the trace and follow HTTP → SQL → gRPC → Go → `redis.set` → `redis.xadd`, then the
  asynchronous `audit.process` → `redis.xack`. The queue consumer joins the same trace because the
  `traceparent` travels inside the stream message.
- Click **Logs for this span** to jump to Loki. The query
  `{service_name=~"python-token-service|go-token-service"} | trace_id="<trace-id>"` shows `token_issued`,
  `token_minted` and `token_audit_recorded` together. Use a log line's **TraceID** link to jump back to Tempo.
- Explain that token and email values are never written to logs, span attributes or queue messages.

**5. Show the audit queue (45 seconds).**

On the dashboard's audit-queue row, point out depth (lag + pending), oldest pending age and processing p95.
Explain that publishing is best-effort, so a queue problem never blocks token issuance, and that a live
consumer reclaims a crashed pod's unacknowledged messages after 30 seconds.

**6. Show the deployment/safety choices (60-90 seconds).**

Show `scripts/kind-e2e.sh`: it deploys the dev overlay to kind with default-deny NetworkPolicies enforced and
runs a Redis failover drill. Then show the Dockerfiles, probes and resources, ConfigMap and Secret separation,
HPA, PDB and NetworkPolicies. Dev uses the smallest resource profile, staging uses an intermediate profile,
and production retains the shared base sizing. PostgreSQL uses CloudNativePG and Redis uses Sentinel.
Production still needs backups, restore drills and a real notification route.

## 2. 90-second design explanation

> I kept the business flow small and focused the change on service reliability and operability. The Python service validates the request, looks up permissions through a bounded PostgreSQL pool, then calls the Go service over a reused gRPC channel with a deadline. The Go service generates a cryptographically random opaque token and stores it in Redis with an explicit TTL. Dependency failures are mapped to clear 503/504 responses or gRPC Unavailable rather than being treated as success.
>
> Each mint also publishes an audit event to a Redis Stream, which a consumer group processes and acknowledges asynchronously. That gives the platform a real queue with measurable depth, oldest-message age and processing time.
>
> For observability, I instrumented HTTP and gRPC RED metrics and the actual dependencies: PostgreSQL pool acquisition and utilisation, Redis operation latency and errors, and the queue. OpenTelemetry propagates W3C trace context from FastAPI through PostgreSQL and gRPC into Redis, and on through the queue. Both services emit JSON logs with trace and span IDs. OTLP logs and traces go through a Collector into Loki and Tempo, Prometheus scrapes metrics, and Grafana links logs to traces in both directions. A script proves this for one trace ID.
>
> On Kubernetes, the stateless services have probes, resource limits, HPAs, PDBs and default-deny network policies. PostgreSQL is a three-instance CloudNativePG cluster and Redis is a Sentinel topology. Kubernetes config is generated from the same files Compose uses, so the two cannot drift. A kind end-to-end test with a Redis failover drill is in CI. Remaining production work is backups and restore drills, external secrets, mandatory mTLS and a real Alertmanager route.

## 3. Good answers to likely follow-ups

### Why a readiness probe and a liveness probe?

Liveness answers “is this process irrecoverably unhealthy and should Kubernetes restart it?” Readiness answers “can this pod safely receive new traffic?” A database or Redis outage should generally fail readiness, not liveness, otherwise every pod can enter a restart loop while the dependency is already down.

### Why not add retries everywhere?

Retries amplify load during incidents and can duplicate non-idempotent operations. First establish finite deadlines, bounded retry budgets, exponential backoff with jitter, and idempotency semantics. For token minting, retry behavior needs a clear decision about duplicate token creation and caller response loss.

### Why not use a JWT?

Opaque tokens allow central revocation and policy changes, at the cost of Redis availability and a lookup on validation. JWTs reduce lookup latency but require strict issuer/audience/algorithm validation, short lifetimes, key rotation and a revocation strategy. The right answer follows the identity protocol and threat model, not a blanket preference.

### How would you size the PostgreSQL pool?

Budget `pool_max_per_pod × maximum_pod_replicas`, add migrations, monitoring and admin connections, then leave headroom under `max_connections`. Validate using pool wait, active sessions, query latency, CPU and lock metrics. A larger pool can worsen DB contention.

### How would you improve alert quality?

Page on user-impacting SLO burn and sustained symptoms; use dependency saturation and component-level thresholds for diagnosis. Group alerts by service/cluster, inhibit downstream symptoms when a root dependency is clearly down, add runbooks and test notification delivery. Avoid paging on a single transient spike.

### What would you do for multi-region resilience?

Define the RTO/RPO and consistency requirements first. Decide if token validity/revocation must be globally consistent. Design database and Redis replication/failover, regional traffic routing, key management, data residency, dependency locality and disaster recovery drills. A second app region without a data strategy is not real resilience.

## 4. Demo caveats to state proactively

- A trace/log may take a few seconds to appear because OTLP export is batched.
- Prometheus/Grafana latency percentile panels require sufficient request observations.
- The audit queue is a deliberate addition. The starter had no queue, so "queue depth" needed a real queue to measure. Publishing is best-effort, so a queue problem never blocks token issuance.
- Queue depth gauges are read from Redis at scrape time and are group-wide, so dashboards use `max()` across replicas, not `sum()`.
- Report the current results in [VALIDATION.md](VALIDATION.md); Compose and kind checks are runtime tests and
  should not be described as passing until they have actually completed.
- Kubernetes HPAs need metrics-server. NetworkPolicies need a CNI that enforces them (kind's does). Access is by port-forward until an Ingress is added.
- PostgreSQL and Redis HA is exercised by the kind end-to-end test. Backups and restore drills still need object storage and an agreed RPO/RTO.
