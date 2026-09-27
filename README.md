# database-controller

Kubernetes controllers that provision the **data plane** of managed AWS data
services: the PostgreSQL role, schemas, grants and object ownership a service
needs inside an RDS or Aurora database.

```sh
make check              # fmt, vet, unit tests
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
  iamAuth: true
  grants:
    - schema: ingest
      privileges: [SELECT, INSERT, UPDATE, DELETE]
      ownerOf: true
```

The controller connects as the admin role using an IAM auth token from Pod
Identity, reads the database's current state, builds a plan, applies it, and
records what it did in `status`.

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
