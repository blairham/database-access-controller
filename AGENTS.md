# AGENTS.md — database-access-controller

Guidance for AI coding agents working in this repo. `CLAUDE.md` imports it, and
other tools read this file directly.

## Project Overview

A Kubernetes controller that provisions the **data plane** of a PostgreSQL
database: the role, schemas, grants and object ownership a service needs inside
RDS, Aurora or a self-managed PostgreSQL.

- Module: `github.com/blairham/database-access-controller`
- Go 1.26, `controller-runtime` v0.25, `aws-sdk-go-v2`, `pgx/v5`,
  `hashicorp/cli`, [`k8s-controller-kit`](https://github.com/blairham/k8s-controller-kit)

## Quick Reference

```sh
make check              # vet, unit tests, envtest, chart lint -- CI's non-database half
make test               # unit tests, no database needed
make test-envtest       # controller against a real kube-apiserver, no cluster
make test-integration   # runs generated SQL against a real PostgreSQL 16
make test-equivalence   # diffs this engine against an existing provisioner script
make generate           # deepcopy, CRDs, RBAC + sync them into the chart
make helm-lint          # lint and render the chart with the toggles flipped
make check-generated    # fail if the generated files are stale
make build              # bin/manager, bin/dbctl
make fmt                # gofumpt
make pg-down            # stop the test database
```

## Project Structure

```
apis/db/v1alpha1/                DatabaseAccess API + generated deepcopy
internal/engine/                 the Engine interface each database implements
internal/engine/postgres/        PostgreSQL statements, inspection, plan building
internal/rdsauth/                IAM auth tokens for RDS and Aurora
internal/rdsca/                  embedded RDS CA bundle (hack/update-rds-ca.sh refreshes it)
internal/controller/             the DatabaseAccess API wired onto the kit's reconciler
cmd/manager/                     the controller binary; --controllers selects which run
cmd/dbctl/                       plan and apply from a terminal
config/crd/, config/rbac/        generated manifests -- never hand-edit
charts/database-access-controller/      the Helm chart; the install path
docs/design/                     why the design is shaped this way
```

## CI/CD

`.github/workflows/ci.yml` holds everything that gates a merge. Jobs (their
names are the required checks): **Pre-commit** (the hooks below, via
`blairham/go-pre-commit`), **Detect changed files** (skips the code jobs for
prose-only PRs without leaving a required check pending), **Build and test**
(build, `check-generated`, vet across all build tags, `go test -race`,
envtest), **PostgreSQL integration** (a PostgreSQL 16 service container),
**Build image** and **Helm chart**. `codeql.yml` and `scorecard.yml` run
alongside. Every action is pinned to a commit SHA with a `# vX.Y.Z` comment;
Dependabot moves the pins.

A **`v*` tag is what publishes**: `goreleaser.yml` runs GoReleaser, which
pushes `ghcr.io/blairham/database-access-controller:<version>` (amd64 and arm64) and a
`dbctl` archive per platform, signed with keyless cosign and carrying SLSA
provenance (see `SECURITY.md`). A push to main only validates. The release
refuses to publish when `Chart.yaml`'s `appVersion` does not match the tag.
The same tag runs `chart.yml`, which pushes the chart to
`oci://ghcr.io/blairham/charts/database-access-controller:<version>` and signs it; it
refuses unless both `version` and `appVersion` match the tag. It is its own
workflow so an existing tag can be published alone:
`gh workflow run chart.yml -f tag=vX.Y.Z`.

To cut a release: bump `version` and `appVersion` in
`charts/database-access-controller/Chart.yaml`, commit, then tag `vX.Y.Z` (signed).

## Code Conventions

- Formatting and lint run as **pre-commit hooks**, never by hand:
  golangci-lint v2 (config in `.golangci.yml`, formatters gofmt, gofumpt,
  goimports, gci, golines) pinned in go.mod's `tool` block and in
  `.pre-commit-config.yaml` -- move the two together. The hook reports only
  what a commit introduces, so an older finding surfaces when its lines are
  next touched.
- Every `.go` file starts with the two-line SPDX header (`hack/boilerplate.go.txt`,
  which controller-gen also stamps onto generated files). The
  `check-license-headers` hook fails without it.
- Go is pinned once: `.tool-versions` and go.mod's `go` directive must match
  (the `check-go-version-sync` hook enforces it).
- **Comments are short and explain why, not what.** A sentence or two; the
  longer story belongs in `docs/design/database-access.md` or the commit
  message. Many non-obvious lines exist because of a specific failure: keep the
  test that pins it, or explain in the PR why the failure can no longer happen.
- **Never hand-edit generated files.** `zz_generated.deepcopy.go`, `config/crd/`,
  `config/rbac/`, and the chart's `templates/crds.yaml` and `templates/rbac.yaml`
  all come from `make generate`; the chart's two are copied from `config/` by
  `hack/sync-chart.sh`. API field comments become the CRD descriptions, so a
  change to them needs `make generate` too.
- Identifiers reaching SQL go through `ValidateIdent` (or
  `ValidateHyphenatedIdent`, for role and database names only) then
  `QuoteIdent`. PostgreSQL does not accept a parameter where an identifier is
  required, so this is the only thing standing between a CRD field and
  injected SQL. Never put an identifier in an expression position: unquoted,
  `a-b` parses as subtraction. A name read back from the catalog rather than
  the spec (a relation under `ownerOf`) goes through `ValidateCatalogIdent`,
  which checks length only (#30); never use it for a spec field.
- The naming rule: a name the controller accepts must, typed unquoted, either
  mean the same object or fail to parse -- never name a different one. That is
  why single hyphens are allowed in roles and databases (#27) while uppercase,
  dots and `--` are not, and why schemas stay strict.
- American English spelling.

## Architecture

**Creating** an AWS database is a control-plane call that Crossplane's
`provider-aws` or ACK already covers; this repo does not do it. It only
provisions **inside** the database, in the database's own protocol.

Provisioning is a `plan.Plan`: an ordered list of steps that can be printed
before being run, so the controller and `dbctl plan` share one code path. The
plan is a diff against the database's current state.

The plan type and the reconcile loop -- Enforce and Observe, the `Ready` and
`Converged` conditions, the finalizer, the per-resource metrics, the shared
`main` -- come from [k8s-controller-kit](https://github.com/blairham/k8s-controller-kit),
pinned in `go.mod`, which kafka-controller shares. A change to any of those
belongs in the kit: land it there, tag it, then bump the version here. This
repo keeps the engine, the API and the mapping between them
(`internal/controller/databaseaccess/controller.go`). Pruning is not wired
in: the kit supports it, but revoking grants a spec stops declaring would
change what a released controller does, and needs its own decision.

`Engine` is the per-database interface. PostgreSQL (including RDS and Aurora)
is implemented; MySQL and SQL Server are planned. DynamoDB is out of scope: it
has no data-plane principals. Controllers for other domains belong in their own
repos.

## Adding an engine

1. Implement `engine.Engine` in `internal/engine/<name>/`.
2. The engine owns its identifier validation and quoting: its own length
   limits and quote function, with a fuzz test proving any input renders as
   exactly one identifier (see `identifier_fuzz_test.go`). The CRD patterns
   are only the floor every engine shares. MySQL in particular: users are
   string literals (`'name'@'host'`), whose escaping depends on `sql_mode`,
   and `_` and `%` are wildcards in database-level grants, so they need
   escaping there.
3. Unit-test plan construction with a fake inspector.
4. Add integration tests behind the `integration` build tag, against a real
   server. Unit tests cannot tell you the DDL parses.
5. Add the value to the `Engine` enum in `apis/db/v1alpha1` and the case to
   `NewEngineFactory` in `internal/controller/databaseaccess/engine.go`.

## Testing

Unit tests need nothing. Integration tests need a database and are behind the
`integration` tag so `make test` stays fast and hermetic. Every bug fixed in
plan construction gets a unit test; every bug about what the *server* accepts
gets an integration test.

The `envtest` suite runs the reconciler against a real kube-apiserver and etcd
(no cluster, no Docker): CRD defaults, enums and identifier patterns, status,
conditions and finalizers. The engine is faked there.

The `equivalence` suite runs an existing provisioner script and this engine
against the same seed and diffs ownership, ACLs and default privileges. Use it
before cutting a database over.

## Documentation

`docs/design/database-access.md` — the design decisions and the failures behind
them. Read it before changing the engine.
