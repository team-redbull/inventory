# Bare-metal Fleet Manager
IMG ?= REGISTRY/fleet-manager:latest
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.15.0

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-16s %s\n",$$1,$$2}'

.PHONY: tidy
tidy: ## go mod tidy (run against your internal proxy)
	go mod tidy

.PHONY: generate
generate: ## DeepCopy + CRD manifests from kubebuilder markers
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd

.PHONY: fmt vet
fmt: ; gofmt -w .
vet: ; go vet ./...

.PHONY: build
build: ## Build the manager binary
	go build -o bin/manager ./cmd/manager

.PHONY: run
run: ## Run the manager locally against the current kubecontext
	go run ./cmd/manager --mce=$(MCE)

.PHONY: test
test: ; go test ./...

.PHONY: db
db: ## Apply the store schema (set DATABASE_URL)
	psql "$(DATABASE_URL)" -f db/schema.sql

.PHONY: docker-build
docker-build: ; docker build -t $(IMG) .

# ---- local dev environment --------------------------------------------------

PG_URL ?= postgres://postgres:fleet@localhost/fleet

.PHONY: dev-setup
dev-setup: ## Create kind cluster + apply stub CRDs (run once)
	bash hack/dev-setup.sh

.PHONY: dev-teardown
dev-teardown: ## Destroy kind cluster + docker compose volumes
	bash hack/dev-teardown.sh

.PHONY: dev-store
dev-store: ## Start Postgres via docker compose
	docker compose up -d

.PHONY: dev-store-down
dev-store-down: ## Stop Postgres
	docker compose down

.PHONY: dev-samples
dev-samples: ## Apply test CRs to the dev cluster
	kubectl apply -f config/test/samples/

.PHONY: mock-ome
mock-ome: ## Run mock OME server on :8081
	go run ./hack/mock/ome

.PHONY: mock-intersight
mock-intersight: ## Run mock Intersight PVA server on :8082
	go run ./hack/mock/intersight

.PHONY: mock-ucsm
mock-ucsm: ## Run mock UCSM server on :8083
	go run ./hack/mock/ucsm

.PHONY: dev-run
dev-run: ## Run manager against dev cluster + local Postgres (set MCE=dev)
	go run ./cmd/manager \
	  --mce=$(MCE) \
	  --agent-namespace=default \
	  --postgres-url="$(PG_URL)"

.PHONY: dev-status
dev-status: ## Show store capacity + allocation state
	@echo "=== host_capacity ==="
	@psql "$(PG_URL)" -c "SELECT site, class, owner_mce, total, available, allocated, maintenance FROM host_capacity;"
	@echo ""
	@echo "=== host_allocation ==="
	@psql "$(PG_URL)" -c "SELECT service_tag, hosted_cluster, node_name, updated_at FROM host_allocation;"
	@echo ""
	@echo "=== region_headroom ==="
	@psql "$(PG_URL)" -c "SELECT class, total, allocated, spare, shortage FROM region_headroom;"
