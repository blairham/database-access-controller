GO ?= go
BIN ?= bin
IMG ?= database-controller:dev
PGTEST_DSN ?= postgres://postgres:test@127.0.0.1:5433/postgres

.PHONY: all
all: generate fmt vet test build

.PHONY: generate
generate: ## Regenerate deepcopy, CRDs, RBAC, and sync them into the chart.
	$(GO) tool controller-gen object:headerFile=/dev/null paths=./apis/...
	$(GO) tool controller-gen crd paths=./apis/... output:crd:artifacts:config=config/crd
	$(GO) tool controller-gen rbac:roleName=database-controller paths=./internal/... output:rbac:artifacts:config=config/rbac
	./hack/sync-chart.sh

.PHONY: helm-lint
helm-lint: ## Lint and render the chart, including with the toggles flipped.
	helm lint charts/database-controller
	helm template database-controller charts/database-controller >/dev/null
	helm template database-controller charts/database-controller --set crds.install=false >/dev/null
	helm template database-controller charts/database-controller --set rbac.create=false >/dev/null
	helm template database-controller charts/database-controller --set autoscaling.enabled=true >/dev/null
	helm template database-controller charts/database-controller --set podDisruptionBudget.maxUnavailable=1 >/dev/null
	helm template database-controller charts/database-controller --set metrics.serviceMonitor.enabled=true >/dev/null

.PHONY: check-generated
check-generated: generate ## Fail if the generated files are out of date.
	@git diff --exit-code -- config charts apis || \
		{ echo "generated files are stale -- run 'make generate' and commit"; exit 1; }

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

RIG_CONTEXT ?= k5s/database-controller
RIG_NAMESPACE ?= default
KIND_CLUSTER ?= k8s

.PHONY: rig-install
rig-install: docker-build ## Build, side-load and helm-install the controller into the k5s rig.
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)
	helm --kube-context $(RIG_CONTEXT) upgrade --install database-controller charts/database-controller \
	  --namespace $(RIG_NAMESPACE) \
	  --set image.repository=$(firstword $(subst :, ,$(IMG))) \
	  --set image.tag=$(lastword $(subst :, ,$(IMG))) \
	  --set image.pullPolicy=IfNotPresent \
	  --set replicaCount=1 \
	  --set priorityClassName= \
	  --set logEncoder=console \
	  --wait --timeout 2m

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
check: fmt vet test test-envtest helm-lint ## What CI runs. No database or cluster needed.

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: test-envtest
test-envtest: ## Run the controller against a real kube-apiserver (no cluster).
	KUBEBUILDER_ASSETS="$$($(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use -p path)" \
	  $(GO) test -tags envtest ./internal/controller/... -v

.PHONY: test-equivalence
test-equivalence: pg-up ## Diff this engine against the provisioner it replaces. Needs JOB_SCRIPT, EQ_SCHEMA, EQ_ROLE.
	@test -n "$(JOB_SCRIPT)" || { echo "set JOB_SCRIPT to a rendered provisioner script"; exit 1; }
	JOB_SCRIPT='$(JOB_SCRIPT)' JOB_SCRIPT_RO='$(JOB_SCRIPT_RO)' \
	EQ_SCHEMA='$(EQ_SCHEMA)' EQ_ROLE='$(EQ_ROLE)' \
	PGTEST_DSN='$(PGTEST_DSN)' PGTEST_CONTAINER=pgtest \
	  $(GO) test -tags equivalence ./internal/engine/postgres/ -run Equivalence -v
