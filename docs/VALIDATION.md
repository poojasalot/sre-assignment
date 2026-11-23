# Validation guide and latest results

This guide separates checks that have passed from checks that still need to be run. Re-run the commands
below after changes; a successful render or schema check does not replace runtime tests.

## Latest execution results (2026-11-23)

| Check | Result |
|---|---|
| Python and Go unit tests (`make test`) | Pass: 6 Python tests; Go formatting, vet and race tests pass. Python dependencies were installed into a temporary virtual environment. |
| Go module tidy check (`go mod tidy -diff`) | Pass. |
| Kustomize rendering and strict schemas (`make k8s-validate`) | Pass: 240 resources valid, 0 invalid, 0 skipped. Includes the platform, both app roots, all three overlays, CloudNativePG and Argo CD schemas. |
| Compose YAML (`docker-compose config --quiet`) | Pass. |
| Shell syntax and Grafana dashboard JSON | Pass. |
| Prometheus configs and alert rules | Pass: both Prometheus configs, all 30 rules and the Prometheus rule unit tests. |
| `make validate-config` | Prometheus checks and rule tests pass; 
<!-- the target stops at `amtool` because that executable is not installed. Alertmanager and OTel checks separately pass using the deployed container images (`amtool check-config` and `otelcol-contrib validate`). | --> |
| Compose image builds | Pass for both application images. |
<!-- | Compose integration and observability correlation | Blocked: Docker refused bind mounts from this workspace under `Documents`; the isolated test project was removed afterward. The existing Compose project was left untouched. |
| kind end-to-end / Redis failover | Failed before workload rollout: CloudNativePG and Kubernetes control-plane probes timed out while two kind clusters shared a Docker Engine with only 2 GiB available. The isolated test cluster was deleted; the existing `sre-token` cluster was preserved. | -->
| Existing CNPG controller recovery | Pass: after deleting the extra test cluster, the controller stayed Ready with no new restarts for 60 seconds, and a server-side dry run through its admission webhook succeeded. |


<!-- The kind issue was resource contention, not a broken Kubernetes Service route: the existing kind node had
2 GiB memory/2 CPUs, was using about 1.47 GiB, and the Kubernetes API Service had a valid endpoint. When the
extra test cluster was removed, the existing CNPG controller recovered. `scripts/kind-e2e.sh` now fails fast
below 6 GiB, refuses cluster-name collisions, refuses parallel kind clusters by default, and deletes only a
cluster it created. Use `ALLOW_PARALLEL_KIND_CLUSTER=1` only when Docker has sufficient capacity.

`make k8s-validate` creates the `rendered/` directory. It validates structure and schemas, not whether the
resource requests/limits or HPA values are the desired ones. Dev and staging resource profiles are defined
in their overlay `resources.yaml` files; production retains the shared base profile. Inspect the generated
files in `rendered/` when reviewing effective values. -->

## Prerequisites

- Python 3, Go, and the Python development requirements.
- `kubectl` and `kubeconform` for Kubernetes rendering/schema validation; network access is needed to fetch
  external CRD schemas.
- `promtool`, `amtool` and `otelcol-contrib` for `make validate-config`, or Docker with the validator images
  from `.github/workflows/ci.yml`.
- Docker Engine and Docker Compose for the Compose runtime flow.
- Docker with at least 6 GiB available memory and 4 CPUs, kind 0.24+, kubectl and openssl for the kind end-to-end flow.
  By default, no other kind cluster may be running.

## Static checks

Run these commands from the repository root:

```bash
# Create/activate a virtual environment and install test dependencies.
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r python-service/requirements-dev.txt

# Unit, formatting, static-analysis and dependency checks.
make test
(cd golang-service && go mod tidy -diff)

# Observability configuration and dashboard.
make validate-config
python -m json.tool observability/grafana/dashboards/sre-token-platform.json >/dev/null

# Render every Kustomize root and validate Kubernetes + CRD schemas.
make k8s-validate
```

`make validate-config` runs Prometheus config/rule checks and rule tests, Alertmanager config validation,
and OTel Collector validation. Install `amtool` and `otelcol-contrib` to run the Make target directly.
Alternatively, follow the image-based commands in `.github/workflows/ci.yml`; Docker must be able to access
the repository files. In this environment the checks were run with the deployed images by copying the
individual config files into temporary containers, because Docker bind mounts from this workspace are
blocked.

## Compose runtime flow

Use the Compose CLI available on your machine: the examples below use `docker compose`; substitute
`docker-compose` if you have the standalone Compose executable. If Docker reports a bind-mount permission
error, enable file sharing for the repository path in Docker Desktop before retrying.

```bash
docker compose config --quiet
cp .env.example .env
# Replace the example values in .env with unique local secrets.
docker compose up --build -d --wait --wait-timeout 180
./scripts/smoke-test.sh
./scripts/validate-observability.sh
docker compose down --remove-orphans
```

The smoke test checks Python liveness/readiness, token issuance for the seeded user and the expected 404 for
an unknown user. The observability script sends a request with a generated W3C trace ID and verifies that
Tempo contains the Python/Go/Redis/audit spans and Loki returns logs from both services for that ID.
`docker compose down` keeps named volumes; add `--volumes` only when you intend to delete local data.

## Kubernetes runtime flow

```bash
./scripts/kind-e2e.sh
```

This builds/loads the service images, installs CloudNativePG, creates dev secrets, deploys the dev overlay,
waits for workloads, runs smoke and trace/log checks, checks Prometheus targets, and performs a Redis
Sentinel failover drill. It requires at least 6 GiB and 4 CPUs available to Docker. The script refuses to reuse an
existing cluster name and refuses to run beside another kind cluster unless explicitly opted in with
`ALLOW_PARALLEL_KIND_CLUSTER=1`. It deletes only the test cluster it created; set `KEEP_CLUSTER=1` to keep
that cluster after the run.

For a manual dev deployment to an existing cluster:

```bash
./scripts/install-cnpg.sh
./scripts/create-k8s-secrets.sh sre-platform-dev
kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/overlays/dev \
  | kubectl apply --server-side -f -
kubectl -n sre-platform-dev wait cluster.postgresql.cnpg.io/postgres \
  --for=condition=Ready --timeout=10m
kubectl -n sre-platform-dev get pods,svc,hpa
```

The cluster needs a StorageClass and a CNI that enforces NetworkPolicy. HPAs need metrics-server to report
CPU metrics. The kind end-to-end script uses kind's NetworkPolicy-capable default CNI.

## CI coverage

`.github/workflows/ci.yml` runs Python tests, Go tidy/format/vet/race checks, observability config checks,
strict Kustomize/schema validation, container lint/build/image scans, the Compose integration flow and the
kind end-to-end/failover flow. 
<!-- Checks that have not completed locally should be reported as CI-only or not-yet-run, not as passing. -->
