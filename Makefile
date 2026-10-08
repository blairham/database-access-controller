GO ?= go
BIN ?= bin
IMG ?= database-access-controller:dev
PGTEST_DSN ?= postgres://postgres:test@127.0.0.1:5433/postgres

.PHONY: all
all: generate fmt vet test build

.PHONY: generate
generate: ## Regenerate deepcopy, CRDs, RBAC, and sync them into the chart.
	$(GO) tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./apis/...
	$(GO) tool controller-gen crd paths=./apis/... output:crd:artifacts:config=config/crd
	$(GO) tool controller-gen rbac:roleName=database-access-controller paths=./internal/... output:rbac:artifacts:config=config/rbac
	./hack/sync-chart.sh

.PHONY: helm-lint
helm-lint: ## Lint and render the chart, including with the toggles flipped.
	helm lint charts/database-access-controller
	helm template database-access-controller charts/database-access-controller >/dev/null
	helm template database-access-controller charts/database-access-controller --set crds.install=false >/dev/null
	helm template database-access-controller charts/database-access-controller --set rbac.create=false >/dev/null
	helm template database-access-controller charts/database-access-controller --set autoscaling.enabled=true >/dev/null
	helm template database-access-controller charts/database-access-controller --set podDisruptionBudget.maxUnavailable=1 >/dev/null
	helm template database-access-controller charts/database-access-controller --set metrics.serviceMonitor.enabled=true >/dev/null
	helm template database-access-controller charts/database-access-controller --set prometheusRule.enabled=true >/dev/null

# Exactly the paths `generate` writes; the rest of the chart is hand-maintained.
GENERATED_PATHS = config apis/db/v1alpha1/zz_generated.deepcopy.go \
                  charts/database-access-controller/templates/crds.yaml \
                  charts/database-access-controller/templates/rbac.yaml

.PHONY: check-generated
check-generated: generate ## Fail if the generated files are out of date.
	@git diff --exit-code -- $(GENERATED_PATHS) || \
		{ echo "generated files are stale -- run 'make generate' and commit"; exit 1; }

.PHONY: fmt
fmt: ## Format with gofumpt (the commit hook also runs the configured formatters).
	$(GO) tool gofumpt -w .

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

# A unique tag per build: with a reused tag the Deployment spec does not change
# (so nothing rolls) and `kind load` may keep the old image.
RIG_TAG ?= dev-$(shell date +%s)
RIG_IMG = database-access-controller:$(RIG_TAG)

# Extra values file for the rig release, e.g. a credential-agent sidecar for
# testing IAM auth (see docs/design/database-access.md).
RIG_VALUES ?=

RIG_CONTEXT ?= k5s/database-access-controller
RIG_NAMESPACE ?= default
KIND_CLUSTER ?= k8s

.PHONY: rig-install
rig-install: ## Build, side-load and helm-install the controller into the k5s rig.
	docker build -t $(RIG_IMG) .
	kind load docker-image $(RIG_IMG) --name $(KIND_CLUSTER)
	helm --kube-context $(RIG_CONTEXT) upgrade --install database-access-controller charts/database-access-controller \
	  --namespace $(RIG_NAMESPACE) \
	  --set image.repository=database-access-controller \
	  --set image.tag=$(RIG_TAG) \
	  --set image.pullPolicy=IfNotPresent \
	  --set replicaCount=1 \
	  --set priorityClassName= \
	  --set logEncoder=console \
	  $(if $(RIG_VALUES),-f $(RIG_VALUES)) \
	  --wait --timeout 2m
	kubectl --context $(RIG_CONTEXT) rollout status deploy/database-access-controller -n $(RIG_NAMESPACE) --timeout=2m
	@echo "running image: $(RIG_IMG)"

.PHONY: rig-test
rig-test: ## Apply the example DatabaseAccess and wait for it to go Ready.
	kubectl --context $(RIG_CONTEXT) apply -f examples/rig-databaseaccess.yaml
	kubectl --context $(RIG_CONTEXT) wait --for=condition=Ready \
	  databaseaccess/rig-app -n $(RIG_NAMESPACE) --timeout=90s
	kubectl --context $(RIG_CONTEXT) get databaseaccess -n $(RIG_NAMESPACE) -o wide

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: check
check: vet test test-envtest helm-lint ## The non-database half of CI. No database or cluster needed.

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: test-envtest
test-envtest: ## Run the controller against a real kube-apiserver (no cluster).
	KUBEBUILDER_ASSETS="$$($(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use -p path)" \
	  $(GO) test -tags envtest ./internal/controller/... -v

.PHONY: test-equivalence
test-equivalence: pg-up ## Diff this engine against an existing provisioner script. Needs JOB_SCRIPT, EQ_SCHEMA, EQ_ROLE.
	@test -n "$(JOB_SCRIPT)" || { echo "set JOB_SCRIPT to a rendered provisioner script"; exit 1; }
	JOB_SCRIPT='$(JOB_SCRIPT)' JOB_SCRIPT_RO='$(JOB_SCRIPT_RO)' \
	EQ_SCHEMA='$(EQ_SCHEMA)' EQ_ROLE='$(EQ_ROLE)' \
	PGTEST_DSN='$(PGTEST_DSN)' PGTEST_CONTAINER=pgtest \
	  $(GO) test -tags equivalence ./internal/engine/postgres/ -run Equivalence -v
