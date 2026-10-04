# database-controller

[![CI](https://github.com/blairham/database-controller/actions/workflows/ci.yml/badge.svg)](https://github.com/blairham/database-controller/actions/workflows/ci.yml)
[![CodeQL](https://github.com/blairham/database-controller/actions/workflows/codeql.yml/badge.svg)](https://github.com/blairham/database-controller/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/blairham/database-controller/badge)](https://scorecard.dev/viewer/?uri=github.com/blairham/database-controller)
[![Go version](https://img.shields.io/github/go-mod/go-version/blairham/database-controller)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

A Kubernetes controller that provisions the **data plane** of a PostgreSQL
database: the role, schemas, grants and object ownership a service needs inside
RDS, Aurora or a self-managed PostgreSQL.

```sh
make check              # vet, unit tests, envtest, chart lint
make test-integration   # runs the generated SQL against a real PostgreSQL
make build              # bin/manager, bin/dbctl
```

## Scope

Creating an RDS instance or Aurora cluster is an AWS control-plane call that
Crossplane's `provider-aws` or ACK already covers; this repo does not do that.
It does what no AWS API can: connect to the database and create the role,
schemas and grants.

| Engine | Covered | Why |
|---|---|---|
| RDS / Aurora / self-managed PostgreSQL | yes | one implementation |
| RDS / Aurora MySQL | not yet | same shape, different DDL |
| RDS SQL Server | not yet | logins and users instead of roles |
| DynamoDB | never | no data-plane principals; access is an IAM policy, so `provider-aws-iam` owns it |

## The shape

A `DatabaseAccess` declares what one service needs:

```yaml
apiVersion: database-controller.io/v1alpha1
kind: DatabaseAccess
metadata:
  name: ingest
  namespace: ingest
spec:
  engine: postgres
  instance:
    endpoint: example.abc123.us-east-1.rds.amazonaws.com
    database: appdb
    region: us-east-1
  role: ingest_app
  grantRdsIam: true
  grants:
    - schema: ingest
      privileges: [SELECT, INSERT, UPDATE, DELETE]
      ownerOf: true
```

The controller connects as the admin role, reads the database's current
state, builds a plan, applies it, records the result in `status`, and repeats
hourly to correct drift.

The admin needs `CREATEROLE` and the privileges it hands out. For `ownerOf` it
must also be able to act *as* the service role (`SET` and `INHERIT`). On a
role it creates, it gets only `ADMIN OPTION` from PostgreSQL 16, so the plan
grants itself the rest (`GRANT <role> TO CURRENT_USER WITH INHERIT TRUE, SET
TRUE`). A role someone else created needs a one-time
`GRANT <role> TO <admin> WITH ADMIN TRUE, INHERIT TRUE, SET TRUE` from an
administrator; until then planning fails and says so.

## Authentication

The admin connection uses an RDS IAM token by default, minted in-process from
the pod's AWS credentials (on EKS, Pod Identity or IRSA on the controller's
ServiceAccount).

IAM auth exists only on RDS and Aurora. For a self-managed PostgreSQL, use a
Secret instead:

```yaml
spec:
  instance:
    sslMode: disable
    auth:
      method: password
      passwordSecretRef:
        name: admin-creds
        key: password
```

The Secret is read from the resource's own namespace. `dbctl` takes the
password from `PGPASSWORD` instead.

`spec.grantRdsIam` (default `true`) is unrelated: it lets the *service's* role
log in with IAM, and must be `false` on a self-managed PostgreSQL, which has no
`rds_iam` role.

### TLS

`sslMode` defaults to `verify-full`: the certificate must chain to a trusted CA
and name the endpoint. The
[RDS CA bundle](https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem)
is built in alongside the system CAs, so nothing needs mounting.

| `sslMode` | Use it for |
|---|---|
| `verify-full` | Default. RDS and Aurora endpoints, or any server with a certificate from a trusted CA |
| `verify-ca` | An endpoint reached through a CNAME the certificate does not name |
| `require` | Encrypts without verifying -- the admin credential goes to whoever answers. Avoid |
| `disable` | A local PostgreSQL without TLS, such as the rig's |

Upgrading from v0.0.1: the old default was `require`, and the API server wrote
it into every `DatabaseAccess` created without an `sslMode`. Those keep
`require` until you set `sslMode: verify-full` on them.

## Running it in a cluster

```sh
helm install database-controller charts/database-controller \
  --namespace database-controller-system --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::<acct>:role/<role>
```

The annotation is for IRSA; with Pod Identity, associate the role with the
ServiceAccount instead. See
[the chart README](charts/database-controller/README.md) for values.

`k5s.yaml` brings up PostgreSQL plus the controller built from source in a kind
cluster, to exercise the deployment itself (`make rig-install rig-test`). It
uses password auth, so it does not cover the IAM path.

## Metrics and alerts

Beyond controller-runtime's own reconcile counters, the leader exports one
series per `DatabaseAccess`, labeled by `namespace` and `name` only:

| Metric | Meaning |
|---|---|
| `database_controller_access_ready` | `1` when the last reconcile succeeded (applied in Enforce, planned in Observe), `0` when it failed |
| `database_controller_access_warnings` | Best-effort statements skipped on the last apply (`status.warnings`); `0` in Observe |
| `database_controller_access_pending_statements` | Statements the database still needs (`status.pendingStatements`); `0` means converged |
| `database_controller_access_last_planned_timestamp_seconds` | Unix time the database was last planned against (`status.lastPlannedTime`), in either mode |
| `database_controller_access_last_applied_timestamp_seconds` | Unix time of the last successful apply; never moves in Observe |

Series are removed when the resource is deleted. The failure reason is on the
resource (`kubectl describe databaseaccess`), not a label.

`prometheusRule.enabled=true` installs `DatabaseAccessNotReady` (ready is `0`
for 15 minutes) and `DatabaseAccessStale` (not planned against in two hours).
`DatabaseAccessNotConverged` (pending for 2 hours) is off by default, since
Observe resources show pending until their cutover.

## Seeing what it will do

```sh
dbctl plan -f examples/databaseaccess-ingest.yaml
```

This connects to the real database and prints the statements the controller
would run, without running them. The plan is a **diff**: an empty plan means
the database already matches. `dbctl apply` runs it after a confirmation.

## Observe mode

`spec.mode` is `Enforce` (the default) or `Observe`. In `Observe` the controller
reads the database and builds the same plan on every reconcile, records it in
status, and executes nothing -- no grants, no ownership changes, no revoke on
delete, and no finalizer.

```sh
kubectl get dba -A        # MODE and PENDING columns
kubectl get dba <name> -o jsonpath='{.status.pending}'
```

| Status | Meaning |
|---|---|
| `pendingStatements` | how many statements the database still needs (exact) |
| `pending` | those statements, first 50 |
| `Converged` condition | `True` when nothing is pending |
| `Ready` condition | `Observed` in Observe: the database was reachable and planned against |
| `lastPlannedTime` | moves on every reconcile in both modes; `lastAppliedTime` does not move in Observe |

It is for cutovers from another provisioner: zero pending statements shows the
two agree, and switching to `Enforce` is the cutover. In Enforce,
`pendingStatements` comes from a re-plan after applying, so a non-zero value is
a statement that keeps failing or being undone.

## Layout

```
apis/db/v1alpha1/                the DatabaseAccess API
internal/plan/                   engine-neutral Plan: Describe, Apply, Hash
internal/engine/                 the Engine interface every database implements
internal/engine/postgres/        PostgreSQL: statements, inspection, plan building
internal/rdsauth/                IAM auth tokens for RDS and Aurora
internal/rdsca/                  embedded RDS CA bundle
internal/controller/             the DatabaseAccess reconciler
cmd/manager/                     the controller binary
cmd/dbctl/                       plan and apply from a terminal
charts/database-controller/      the Helm chart
```

[docs/design/database-access.md](docs/design/database-access.md) explains the
design decisions.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Identifiers reach SQL only through
`ValidateIdent` then `QuoteIdent`, and contributions require a signed
[CLA](CLA.md). This project follows a [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

The controller runs DDL as an administrative role, so creating a
`DatabaseAccess` is a privileged act — see [SECURITY.md](SECURITY.md) for what
to restrict, and report vulnerabilities there privately, never in a public
issue.

Releases are signed with cosign — see
[Verifying a release](SECURITY.md#verifying-a-release).

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE).
