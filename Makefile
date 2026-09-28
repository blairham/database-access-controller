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

# A UNIQUE TAG PER BUILD, and it has to be unique for two separate reasons.
#
# A mutable tag leaves the Deployment spec byte-identical after a rebuild, so
# Kubernetes has no diff to act on and helm reports "deployed" over a pod still
# running the old binary. And `kind load` does not reliably replace an image
# already present under that tag, so even a forced restart can pull the stale
# one back. Measured: a pod 57 minutes old after a successful upgrade, and a
# node still serving sha 3eeb185c while the build had produced 94b7f9b7 --
# which reads as the change not working rather than never having shipped.
RIG_TAG ?= dev-$(shell date +%s)
RIG_IMG = database-controller:$(RIG_TAG)

# Extra values for the rig release. A rig whose PostgreSQL terminates RDS IAM
# auth needs a credential-agent sidecar here, which is more than --set can
# express -- see docs/design/database-access.md.
RIG_VALUES ?=

RIG_CONTEXT ?= k5s/database-controller
RIG_NAMESPACE ?= default
KIND_CLUSTER ?= k8s

.PHONY: rig-install
rig-install: ## Build, side-load and helm-install the controller into the k5s rig.
	docker build -t $(RIG_IMG) .
	kind load docker-image $(RIG_IMG) --name $(KIND_CLUSTER)
	helm --kube-context $(RIG_CONTEXT) upgrade --install database-controller charts/database-controller \
	  --namespace $(RIG_NAMESPACE) \
	  --set image.repository=database-controller \
	  --set image.tag=$(RIG_TAG) \
	  --set image.pullPolicy=IfNotPresent \
	  --set replicaCount=1 \
	  --set priorityClassName= \
	  --set logEncoder=console \
	  $(if $(RIG_VALUES),-f $(RIG_VALUES)) \
	  --wait --timeout 2m
	kubectl --context $(RIG_CONTEXT) rollout status deploy/database-controller -n $(RIG_NAMESPACE) --timeout=2m
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
