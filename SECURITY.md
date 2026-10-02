# Security Policy

## What database-controller can do

database-controller connects to a PostgreSQL database as an **administrative
role** (`spec.instance.adminUser`) and runs DDL on behalf of every
`DatabaseAccess` in the cluster: it creates roles, schemas and grants, and
changes object ownership. Treat it accordingly:

- **Whoever can create a `DatabaseAccess` can ask for privileges inside any
  database the controller can reach.** Grant `create` on
  `databaseaccesses.database-controller.io` as you would grant the admin role
  itself, and use `--watch-namespace` to confine it.
- With `auth.method: password`, the admin password Secret is read from the
  resource's **own** namespace, never from a namespace named in the reference.
- With IAM authentication, the controller's pod identity can mint a token for
  `adminUser` on every instance its IAM policy names. Scope that policy to the
  instances and the user, not `rds-db:connect` on `*`.

## Supported versions

Only the latest release receives fixes.

## Verifying a release

Releases are signed with [cosign](https://github.com/sigstore/cosign) keyless
signing: the signature is tied to the GitHub Actions workflow that built the
release, not to a key someone could leak. Releases before the first signed one
(`v0.0.0`) carry no signatures.

**Downloads.** `checksums.txt` is signed; it lists the digest of every archive.
Verify the signature, then the archives against it:

```sh
VERSION=v0.0.1
cosign verify-blob \
  --certificate-identity "https://github.com/blairham/database-controller/.github/workflows/goreleaser.yml@refs/tags/$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

**Images.** Each published image is signed by digest:

```sh
cosign verify ghcr.io/blairham/database-controller:0.0.1 \
  --certificate-identity-regexp '^https://github\.com/blairham/database-controller/\.github/workflows/goreleaser\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Reporting a vulnerability

**Do not open a public issue.** Report it privately through GitHub:
[Security → Report a vulnerability](https://github.com/blairham/database-controller/security/advisories/new).

Please include the affected version or commit, what an attacker can do, and
the steps to reproduce. You should receive a response within a week.

In scope, among others:

- a `DatabaseAccess` field reaching SQL without passing `ValidateIdent` and
  `QuoteIdent` — SQL injection as the admin role
- a grant, ownership change or `rds_iam` membership wider than the spec asked
  for, or one that survives `revokeOnDelete`
- the controller reading a Secret from a namespace other than the resource's
- credentials or tokens written to logs, events or status

Out of scope: anything that follows from granting `DatabaseAccess` create
rights to someone you would not give the admin role.
