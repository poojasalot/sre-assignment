# Validation record

This records what has actually been run, what only runs in CI, and what needs real infrastructure. Nothing
is marked as passing unless it was executed.

## 1. Executed locally (no Docker available)

Tools were pinned under `.tools/` (gitignored): kubectl 1.37.2 (Kustomize 5.8.1), kubeconform 0.8.0,
promtool 3.16.0, amtool 0.35.0, otelcol-contrib 0.165.0 and hadolint 2.14.0, with Go 1.27 and Python 3.13.

| Check | Command | Result |
|---|---|---|
| Go formatting, vet, race tests | `make go-test` | Pass: 10 tests. They cover token mint and validate, the TLS fail-closed paths, audit consume with trace continuation, dead-consumer reclaim, the depth collector and oldest-age calculation. Queue tests run against miniredis. |
| `go.mod` tidy | `go mod tidy -diff` | Pass |
| Python unit tests | `make python-test` | Pass: 6 tests |
| Prometheus configs | `promtool check config --syntax-only` (Compose and Kubernetes) | Pass |
| Alert and recording rules | `promtool check rules` | Pass: 30 rules |
| Alert rule unit tests | `promtool test rules` | Pass: a 5% error rate fires the fast burn, 404s do not burn budget, `absent()` alerts fire, and the stalled queue fires |
| Alertmanager config | `amtool check-config` | Pass |
| OTel Collector config | `otelcol-contrib validate` | Pass |
| Grafana dashboard | JSON parse, plus all 29 panel expressions through `promtool check rules` | Pass |
| Kustomize rendering | `make k8s-render`: platform, both apps, dev, staging and production | Pass. Dev renders namespace `sre-platform-dev` throughout, including the RoleBinding subject. It has 1 CNPG instance, `preferred` anti-affinity for Redis and Sentinel, HPA `minReplicas: 1`, and no `replicas` on the app Deployments. |
| Kubernetes schemas | `make k8s-validate`: `kubeconform -strict` with the Kubernetes, CloudNativePG and Argo CD CRD schemas | Pass: 240 resources valid, 0 invalid, 0 skipped. This includes `k8s/argocd-application.example.yaml`. |
| Dockerfile lint | `hadolint --failure-threshold error` | Pass. One pip-pinning warning remains, intentionally not gated. |
| Shell scripts | `sh -n` and `bash -n` on every script | Pass |

## 2. Wired into CI but not yet run

These need Docker. They run in `.github/workflows/ci.yml` and have not run anywhere yet, so treat their first
run as the real validation.

| Check | Where | What it proves |
|---|---|---|
| Compose stack and smoke test | `integration` job | All services start healthy; token for alice, 404 for an unknown user |
| Trace and log correlation | `integration` job, `scripts/validate-observability.sh` | One generated trace ID returns a Tempo trace with both services and the `redis.set`, `redis.xadd` and `audit.process` spans, and one Loki query returns logs from both services |
| Image build, lint and scan | `containers` job | hadolint, build and a Trivy scan of both images |
| kind end-to-end | `kind-e2e` job, `scripts/kind-e2e.sh` | See below |
| Config validation with the deployed images | `observability-config` job | Same checks as section 1, using the exact Prometheus, Alertmanager and Collector images |

What `scripts/kind-e2e.sh` asserts, in order:

1. The dev overlay applies, the CloudNativePG cluster is Ready, and every workload rolls out. This exercises the
   Sentinel writable volume, Redis bootstrap, Alertmanager storage and the generated ConfigMaps.
2. The smoke test and the trace/log correlation pass inside the cluster.
3. Every expected Prometheus job is up (`python-token-service`, `go-token-service`, `otel-collector`,
   `redis-exporter`, `cnpg`, `prometheus`) and an Alertmanager is discovered.
4. A Redis failover: the primary is paused for 20 s, Sentinel promotes a replica, tokens still mint, and the old
   primary rejoins as a replica.

Because kind's default CNI enforces NetworkPolicy, step 1 to 3 passing also validates the default-deny allowlists.

### Known risks for the first run

Static checks cannot confirm these, so check them first if the run fails:

- **Probes under default-deny.** Kubelet probes come from the node, which most CNIs exempt. If kind's does not,
  pods will never become Ready.
- **CNPG instance manager.** Its egress to the Kubernetes API (ports 443 and 6443) must be allowed. The policy
  allows both.
- **Loki and Tempo ownership.** Their volumes are written by the image's own user, and their root
  filesystems are left writable. Confirm they start under the pod security context.
- **Resource sizing.** The full stack in a small Docker Desktop VM may not fit; give kind 6 GB or more.
- **Tempo search.** `validate-observability.sh` fetches the trace by ID, which does not depend on search indexing.

## 3. Not validated, needs real infrastructure

| Item | Needed |
|---|---|
| Notification delivery | A real Alertmanager receiver. The checked-in webhook is `https://example.invalid/...`. |
| Backups and restore | Object storage for CloudNativePG, plus a restore drill. |
| Multi-node and multi-zone behaviour | Required anti-affinity, PDBs and topology spread need a multi-node cluster. kind runs the single-node dev overlay. |
| HPA scaling | metrics-server and load. |
| mTLS | Workload certificates for the `GRPC_TLS_*` settings. Only the fail-closed error path is unit-tested. |
| Argo CD sync | An Argo CD instance with `kustomize.buildOptions: --load-restrictor LoadRestrictionsNone`. Only schema validity is checked. |
| Load and capacity | Load tests to set pool sizes, HPA thresholds, resource limits and the audit stream cap. |

## 4. Reproduce

```bash
# Static checks (no Docker)
make test
make validate-config      # needs promtool, amtool, otelcol-contrib on PATH
make k8s-validate         # needs kubectl and kubeconform on PATH

# Runtime checks (Docker)
cp .env.example .env      # replace every change-me-local-only value
docker compose up --build -d --wait
./scripts/smoke-test.sh
./scripts/validate-observability.sh
docker compose down --volumes --remove-orphans

# Kubernetes (Docker + kind 0.24+)
./scripts/kind-e2e.sh
```
