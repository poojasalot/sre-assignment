DOCKER_COMPOSE := $(shell if docker compose version >/dev/null 2>&1; then echo 'docker compose'; elif command -v docker-compose >/dev/null 2>&1; then echo 'docker-compose'; else echo 'docker compose'; fi)

.PHONY: compose-up compose-down python-test go-test go-vet smoke observability-validation k8s-render k8s-all-render cnpg-install cnpg-status

compose-up:
	$(DOCKER_COMPOSE) up --build -d

compose-down:
	$(DOCKER_COMPOSE) down

python-test:
	cd python-service && python -m pytest -q

go-test:
	cd golang-service && go test ./...

go-vet:
	cd golang-service && go vet ./...

smoke:
	./scripts/smoke-test.sh

k8s-render:
	kubectl kustomize k8s/base

observability-validation:
	./scripts/validate-observability.sh

k8s-all-render:
	kubectl kustomize k8s/base >/tmp/sre-platform.yaml
	kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/platform >/tmp/sre-platform-shared.yaml
	kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/apps/python-service >/tmp/sre-python.yaml
	kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/apps/go-service >/tmp/sre-go.yaml
	kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/overlays/dev >/tmp/sre-dev.yaml
	kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/overlays/staging >/tmp/sre-staging.yaml
	kubectl kustomize --load-restrictor LoadRestrictionsNone k8s/overlays/production >/tmp/sre-production.yaml

cnpg-install:
	./scripts/install-cnpg.sh

cnpg-status:
	kubectl -n sre-platform get clusters.postgresql.cnpg.io,pods,pvc
