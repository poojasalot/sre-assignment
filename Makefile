K8S_ROOTS := platform apps/python-service apps/go-service overlays/dev overlays/staging overlays/production
KUSTOMIZE := kubectl kustomize --load-restrictor LoadRestrictionsNone
CRD_SCHEMAS := https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json

.PHONY: compose-up compose-down test python-test go-test smoke observability-validation \
	validate-config k8s-render k8s-validate k8s-secrets cnpg-install k8s-dev-apply kind-e2e

compose-up:
	docker compose up --build -d --wait

compose-down:
	docker compose down

test: python-test go-test

python-test:
	python -m pytest -q

go-test:
	cd golang-service && test -z "$$(gofmt -l .)" && go vet ./... && go test -race ./...

smoke:
	./scripts/smoke-test.sh

observability-validation:
	./scripts/validate-observability.sh

# Requires promtool, amtool and otelcol-contrib on PATH (CI runs them from the deployed images).
validate-config:
	promtool check config --syntax-only observability/prometheus/prometheus-compose.yml
	promtool check config --syntax-only observability/prometheus/prometheus-k8s.yml
	promtool check rules observability/prometheus/alert-rules.yml
	promtool test rules observability/prometheus/alert-rules.test.yml
	amtool check-config observability/alertmanager/alertmanager.yml
	otelcol-contrib validate --config=observability/otel/collector.yaml

k8s-render:
	mkdir -p rendered
	for root in $(K8S_ROOTS); do $(KUSTOMIZE) k8s/$$root > rendered/$$(echo $$root | tr / -).yaml; done

k8s-validate: k8s-render
	kubeconform -strict -summary -schema-location default -schema-location '$(CRD_SCHEMAS)' \
		rendered/*.yaml k8s/argocd-application.example.yaml

cnpg-install:
	./scripts/install-cnpg.sh

k8s-secrets:
	./scripts/create-k8s-secrets.sh sre-platform-dev

k8s-dev-apply: k8s-secrets
	$(KUSTOMIZE) k8s/overlays/dev | kubectl apply --server-side -f -

kind-e2e:
	./scripts/kind-e2e.sh
