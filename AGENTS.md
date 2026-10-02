# AGENTS.md — database-controller

Guidance for AI coding agents working in this repo. `CLAUDE.md` imports it, and
other tools read this file directly.

## Project Overview

Kubernetes controllers that provision the **data plane** of managed AWS data
services — the PostgreSQL role, schemas, grants and object ownership a service
needs inside an RDS or Aurora database.

- Module: `github.com/blairham/database-controller`
- Go 1.26, `controller-runtime` v0.25, `aws-sdk-go-v2`, `pgx/v5`,
  `hashicorp/cli`

## Quick Reference

```sh
make check              # vet, unit tests, envtest, chart lint -- CI's non-database half
make test               # unit tests, no database needed
make test-envtest       # controller against a real kube-apiserver, no cluster
make test-integration   # runs generated SQL against a real PostgreSQL 16
make test-equivalence   # diffs this engine against the provisioner it replaces
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
internal/plan/                   engine-neutral Plan: Describe, Apply, Hash
internal/engine/                 the Engine interface each database implements
internal/engine/postgres/        PostgreSQL statements, inspection, plan building
internal/rdsauth/                IAM auth tokens for RDS and Aurora
internal/controller/             reconcilers
cmd/manager/                     one binary; --controllers selects which run
cmd/dbctl/              plan and apply from a terminal
config/crd/, config/rbac/        generated manifests -- never hand-edit
charts/database-controller/      the Helm chart; the install path
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
pushes `ghcr.io/blairham/database-controller:<version>` (amd64 and arm64,
built from the same binaries as the archives) and a `dbctl` archive per
platform, and signs both with keyless cosign (see `SECURITY.md`). A push to
main only validates. The tag is the version, so a deployed ref is always
reproducible -- publishing a moving tag per push is what makes a rollout
silently deploy the binary it already had.

The release workflow refuses to publish when `Chart.yaml`'s `appVersion` does
not match the tag. An unset `image.tag` falls back to `appVersion`, so a
mismatch would install the previous binary while reporting the new version.

To cut a release: bump `version` and `appVersion` in
`charts/database-controller/Chart.yaml`, commit, then tag `vX.Y.Z` (signed).

## Code Conventions

- Formatting and lint run as **pre-commit hooks**, never by hand:
  golangci-lint v2 (config in `.golangci.yml`, formatters gofumpt, gci,
  goimports, golines) pinned in go.mod's `tool` block and in
  `.pre-commit-config.yaml` -- move the two together. The linter set was
  adopted after the code was written and the hook reports only what a commit
  introduces, so an older finding surfaces when its lines are next touched.
- Every `.go` file starts with the two-line SPDX header (`hack/boilerplate.go.txt`,
  which controller-gen also stamps onto generated files). The
  `check-license-headers` hook fails without it.
- Go is pinned once: `.tool-versions` and go.mod's `go` directive must match
  (the `check-go-version-sync` hook enforces it).
- **Comments explain why, not what.** Nearly every non-obvious line here exists
  because of a specific production failure. When you touch such a line, keep the
  comment and the test that pins it, or explain in the PR why the failure can no
  longer happen.
- **Never hand-edit generated files.** `zz_generated.deepcopy.go`, `config/crd/`,
  `config/rbac/`, and the chart's `templates/crds.yaml` and `templates/rbac.yaml`
  all come from `make generate`. The chart's two are copied from `config/` by
  `hack/sync-chart.sh`: writing either by hand means the chart drifts from what
  the controller actually needs, and that failure shows up as a permission error
  at runtime rather than anything review would catch.
- Identifiers reaching SQL go through `ValidateIdent` then `QuoteIdent`.
  PostgreSQL does not accept a parameter where an identifier is required, so
  this is the only thing standing between a CRD field and injected SQL.
- American English spelling.

## Architecture

The load-bearing split: **creating** an AWS resource is a control-plane API call
that Crossplane's upjet-generated `provider-aws` already covers, and this repo
does not do it. **Provisioning inside** the resource is spoken in the database's
own protocol, no AWS API can do it, and that is all this repo does.

Provisioning is modeled as a `plan.Plan` — an ordered list of steps that can be
printed before being run. This is what lets one code path serve both the
controller and `dbctl plan`, and it is the fix for the thing it
replaced, which could only be understood by running it.

`Engine` is the per-database interface. PostgreSQL is implemented and covers
Aurora PostgreSQL unchanged. MySQL and SQL Server are additional cases.
DynamoDB is deliberately out of scope: it has no data-plane principals.

## Scope of this repo

Everything here provisions the data plane of a **database**. A controller for
another domain -- Kafka topics, say -- belongs in its own repo named for that
domain, following `aws-load-balancer-controller` and the ACK per-service
controllers rather than accumulating unrelated controllers behind a generic
name.

## Adding an engine

1. Implement `engine.Engine` in `internal/engine/<name>/`.
2. Unit-test plan construction with a fake inspector.
3. Add integration tests behind the `integration` build tag, against a real
   server. Unit tests cannot tell you the DDL parses.
4. Add the case to `DefaultEngineFactory`.

## Testing

Unit tests need nothing. Integration tests need a database and are behind the
`integration` tag so `make test` stays fast and hermetic. Every bug fixed in
plan construction gets a unit test; every bug about what the *server* accepts
gets an integration test.

A third suite behind the `envtest` tag runs the reconciler against a real
kube-apiserver and etcd, downloaded by `setup-envtest` -- no cluster, no
Docker. It covers what a function call cannot see: the CRD schema as the API
server enforces it (defaults, enums, the identifier patterns that keep injected
SQL out), and the reconcile contract (status subresource writes, conditions,
finalizer handling, deletion). The engine is faked there; the SQL is not under
test.

There is a fourth suite behind the `equivalence` tag. It runs a rendered script
from whatever implementation is being replaced and this engine against the same
seed, then diffs the resulting ownership, ACLs and default privileges. Use it
before cutting a database over. It has already earned its keep once: the
column-owned sequence rule was found this way and no unit test would have
caught it.

## Documentation

`docs/design/database-access.md` — the design, and the production failure behind
each decision. Read it before changing the engine.
