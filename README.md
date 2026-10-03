# database-controller

[![CI](https://github.com/blairham/database-controller/actions/workflows/ci.yml/badge.svg)](https://github.com/blairham/database-controller/actions/workflows/ci.yml)
[![CodeQL](https://github.com/blairham/database-controller/actions/workflows/codeql.yml/badge.svg)](https://github.com/blairham/database-controller/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/blairham/database-controller/badge)](https://scorecard.dev/viewer/?uri=github.com/blairham/database-controller)
[![Go version](https://img.shields.io/github/go-mod/go-version/blairham/database-controller)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Kubernetes controllers that provision the **data plane** of managed AWS data
services: the PostgreSQL role, schemas, grants and object ownership a service
needs inside an RDS or Aurora database.

```sh
make check              # vet, unit tests, envtest, chart lint
make test-integration   # runs the generated SQL against a real PostgreSQL
make build              # bin/manager, bin/dbctl
```

## What this is, and what it deliberately is not

Creating an RDS instance, an Aurora cluster or a DynamoDB table is an AWS
control-plane call, and Crossplane's upjet-generated `provider-aws` already
covers roughly a thousand resource types. **This repo does not do that** —
hand-writing it would be reimplementing generated code.

What no AWS API can do is open a connection to the database and create a role,
a schema and its grants. That is spoken in the engine's own protocol, it is
where the real operational pain lives, and it is all this repo does.

| Engine | Covered | Why |
|---|---|---|
| RDS PostgreSQL | yes | roles, schemas, grants, ownership |
| Aurora PostgreSQL | yes | same wire protocol, same DDL — one implementation |
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

The controller connects as the admin role using an IAM auth token from Pod
Identity, reads the database's current state, builds a plan, applies it, and
records what it did in `status`.

## Authentication

The admin connection uses an RDS IAM token by default, minted in-process from
whatever AWS credentials the pod has -- on EKS, a Pod Identity Association on
the controller's ServiceAccount.

IAM auth only exists on RDS and Aurora. For a self-managed PostgreSQL, or the
one in a local development rig, point the instance at a Secret instead:

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

The Secret is read from the resource's own namespace, never from a namespace
named in the reference. `dbctl` takes the password from `PGPASSWORD` instead,
since there is no cluster to read a Secret from on a workstation.

### TLS

`sslMode` defaults to `verify-full`: the server's certificate must chain to a
trusted CA and name the endpoint. Amazon's RDS CAs are not in any system trust
store, so the controller and `dbctl` carry the
[RDS CA bundle](https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem)
built in, alongside the system CAs. Nothing needs mounting.

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

The chart follows the `aws-load-balancer-controller` layout -- same value
names, same file structure -- with one deliberate difference: the CRD lives in
`templates/` rather than `crds/`, because Helm never upgrades `crds/`. See
`charts/database-controller/README.md`.

There is also a `k5s.yaml` lane that brings up PostgreSQL plus the controller
built from source, for exercising the deployment itself -- the image starting,
the generated RBAC being sufficient, leader election against a real Lease. It
uses password auth, because a kind cluster has no IAM, and it therefore says
nothing about whether the Pod Identity path works. See the header of `k5s.yaml`.

## Seeing what it will do

```sh
dbctl plan -f examples/databaseaccess-ingest.yaml
```

This connects to the real database and prints the exact statements the
controller would run, without running them. The plan reflects actual state:
which schemas exist, which roles own objects in them, which relations would be
reassigned.

## Layout

```
apis/db/v1alpha1/                the DatabaseAccess API
internal/plan/                   engine-neutral Plan: Describe, Apply, Hash
internal/engine/                 the Engine interface every database implements
internal/engine/postgres/        PostgreSQL: statements, inspection, plan building
internal/rdsauth/                IAM auth tokens for RDS and Aurora
internal/controller/             the reconcilers
cmd/manager/                     one binary, --controllers selects which run
cmd/dbctl/              plan and apply from a terminal
```

`docs/design/database-access.md` explains why the design is shaped this way and
which production failures each piece exists to prevent.

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
