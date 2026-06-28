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
