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

- Open Tempo search for `python-token-service`.
- Open one successful trace and follow HTTP → SQL → gRPC → Go → Redis.
- Copy the trace ID. Search Loki with `{service_name="python-token-service"} | trace_id="<trace-id>"` and `{service_name="go-token-service"} | trace_id="<trace-id>"`, then show both log records carry the same trace ID.
- Explain that token/email values are not written to logs.

**5. Show the deployment/safety choices (60-90 seconds).**

Show the Dockerfiles, Kubernetes probes/resources, ConfigMap/Secret separation, HPA, PDB and NetworkPolicies. Explain that PostgreSQL uses CloudNativePG and Redis uses Sentinel; production still requires operator operation, tested backups, failover drills and notification routing.

## 2. 90-second design explanation

> I kept the business flow small and focused the change on service reliability and operability. The Python service validates the request, looks up permissions through a bounded PostgreSQL pool, then calls the Go service over a reused gRPC channel with a deadline. The Go service generates a cryptographically random opaque token and stores it in Redis with an explicit TTL. Dependency failures are mapped to clear 503/504 responses or gRPC Unavailable rather than being treated as success.
>
> For observability, I instrumented HTTP and gRPC RED metrics and the actual dependencies: PostgreSQL pool acquisition and utilisation, Redis operation latency/errors and pool/key statistics. OpenTelemetry propagates W3C trace context from FastAPI through PostgreSQL and gRPC into Redis. Both services emit JSON logs with trace and span IDs; OTLP logs and traces go through a Collector into Loki and Tempo, while Prometheus scrapes metrics and Grafana provisions the dashboard.
>
> On Kubernetes, I added separate liveness/readiness probes, resource requests and limits, ConfigMaps and Secrets, two replicas for stateless services, HPAs, PDBs and network policies. PostgreSQL is represented as a three-instance CloudNativePG cluster and Redis as a Sentinel topology. Remaining production work includes operator/backup ownership, live failover evidence, external secrets, TLS/mTLS and real Alertmanager routing.

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
- The assignment has no queue, so Redis queue depth and worker processing time are not implemented; real command duration/error metrics are present.
- Kubernetes HPA requires metrics-server; NetworkPolicies require a policy-enforcing CNI; inbound app/Grafana access assumes an `ingress-nginx` controller or port-forward.
- PostgreSQL and Redis are HA-capable Kubernetes designs through CloudNativePG and Sentinel. Operator setup, PVC storage, backup/restore, failover and live trace verification should be stated as cluster-dependent rather than assumed from static manifests.
