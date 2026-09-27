GO ?= go
BIN ?= bin
IMG ?= database-controller:dev
PGTEST_DSN ?= postgres://postgres:test@127.0.0.1:5433/postgres

.PHONY: all
all: generate fmt vet test build

.PHONY: generate
generate: ## Regenerate deepcopy functions and CRD manifests.
	$(GO) tool controller-gen object:headerFile=/dev/null paths=./apis/...
	$(GO) tool controller-gen crd paths=./apis/... output:crd:artifacts:config=config/crd
	$(GO) tool controller-gen rbac:roleName=database-controller paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test: ## Unit tests. No database required.
	$(GO) test ./... -race -coverprofile=cover.out

.PHONY: test-integration
test-integration: pg-up ## Run the generated SQL against a real PostgreSQL.
	PGTEST_DSN='$(PGTEST_DSN)' $(GO) test -tags integration ./internal/engine/postgres/ -v

.PHONY: pg-up
pg-up: ## Start the PostgreSQL the integration tests use.
	@docker inspect pgtest >/dev/null 2>&1 || \
		docker run --rm -d -p 5433:5432 -e POSTGRES_PASSWORD=test --name pgtest postgres:16-alpine >/dev/null
	@until docker exec pgtest pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done

.PHONY: pg-down
pg-down:
	-docker rm -f pgtest

.PHONY: build
build:
	$(GO) build -o $(BIN)/manager ./cmd/manager
	$(GO) build -o $(BIN)/dbctl ./cmd/dbctl

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: check
check: fmt vet test ## What CI runs.

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: test-equivalence
test-equivalence: pg-up ## Diff this engine against the provisioner it replaces. Needs JOB_SCRIPT, EQ_SCHEMA, EQ_ROLE.
	@test -n "$(JOB_SCRIPT)" || { echo "set JOB_SCRIPT to a rendered provisioner script"; exit 1; }
	JOB_SCRIPT='$(JOB_SCRIPT)' JOB_SCRIPT_RO='$(JOB_SCRIPT_RO)' \
	EQ_SCHEMA='$(EQ_SCHEMA)' EQ_ROLE='$(EQ_ROLE)' \
	PGTEST_DSN='$(PGTEST_DSN)' PGTEST_CONTAINER=pgtest \
	  $(GO) test -tags equivalence ./internal/engine/postgres/ -run Equivalence -v
