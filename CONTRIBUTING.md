# Contributing

Thank you for considering a contribution. Please read the two rules below
before opening a pull request — both are non-negotiable.

## 1. Identifiers reach SQL only through `ValidateIdent` then `QuoteIdent`

The controller runs DDL as an administrative role, and every role, schema and
table name in that DDL comes from a `DatabaseAccess` someone else wrote.
PostgreSQL does not accept a bind parameter where an identifier is required, so
these two functions are the only thing between a CRD field and injected SQL.
A new statement that interpolates a name any other way will not be merged.

The CRD's own validation patterns are a second line, not a substitute: a
resource created before a pattern tightened, or `dbctl` run from a terminal,
never passes through the API server.

## 2. The Contributor License Agreement

Contributions require a signed CLA; the text is in [`CLA.md`](CLA.md).

**Why.** The project may need to offer different licensing terms in future.
That is only possible if one party can license the whole work, and copyright
in a contribution stays with its author unless licensed onward.

The CLA does **not** take your copyright. You keep it; you grant a license
broad enough to include sublicensing, and you affirm the work is your own —
including that no employer holds rights to it.

## Practical

- Open an issue before a large change, so the design can be agreed first.
  Read [`docs/design/database-access.md`](docs/design/database-access.md)
  before changing the engine.
- Work on a branch and open a pull request against `main`.
- Commit messages explain the **why**, not a restatement of the diff.
  Conventional-commit prefixes (`feat:`, `fix:`, `docs:`, …).
- Commits must be signed.
- Every `.go` file carries the two-line SPDX header; the pre-commit hook fails
  without it. Generated files get it from `hack/boilerplate.go.txt`.
- Never hand-edit generated files. A change under `apis/` or to a
  kubebuilder marker needs `make generate`, committed with it; CI fails on
  stale output.
- `pre-commit install` once per checkout. The hooks format, lint, scan for
  secrets and check for vulnerable dependencies on every commit; never bypass
  them with `--no-verify`.
- `go test -race ./...` must pass. A bug in plan construction gets a unit
  test; a bug about what PostgreSQL accepts gets an integration test
  (`make test-integration`, needs Docker); a bug in the reconcile contract or
  the CRD schema gets an envtest (`make test-envtest`).

Please also read the [Code of Conduct](CODE_OF_CONDUCT.md). Security issues go
through [SECURITY.md](SECURITY.md), never a public issue.
