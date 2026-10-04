# Design: DatabaseAccess

**Status:** PostgreSQL implemented; MySQL and SQL Server not started
**Code:** `internal/engine/postgres/`, `internal/controller/databaseaccess/`

## Purpose

Provision the data plane of a managed database (the role, schemas, grants and
object ownership a service needs) with a controller rather than a one-shot Job.

The common alternative is a Helm-templated Job that pipes SQL into `psql`,
named by a hash of its script. Its problems are the reasons for this design:

- **It is immutable.** A GitOps engine's server-side diff swallows the
  immutable-field rejection, so a changed script can silently never run. Here
  the plan hash is only an observation in `status.appliedPlanHash`.
- **It runs once.** Drift (a view created as the wrong role, a hand-run
  `REVOKE`) persists. The controller re-plans hourly and corrects it.
- **It reports one exit code.** Non-fatal failures get swallowed. Here they land
  in `status.warnings` and an event.
- **It cannot be inspected.** `dbctl plan` prints the statements against the
  real database and writes nothing.

## The plan is read first, then written

`BuildPlan` queries the catalog and emits one concrete statement per object,
rather than server-side `DO` blocks that loop over `pg_class` at execution
time. Every statement is visible before it runs. An object created between
planning and applying is picked up on the next reconcile.

The plan is a **diff**: a statement appears only when the database lacks what
it would establish. A converged database plans nothing, and a grant revoked by
hand comes back as exactly the statement that repairs it. The controller still
re-plans and applies on every reconcile; it never skips on a hash.

- `CREATE ROLE` is omitted when the role exists (PostgreSQL has no `IF NOT
  EXISTS`), and `already exists` is still tolerated for a concurrent create.
- `CREATE SCHEMA` is emitted only when the schema is absent.
- `GRANT` and `ALTER DEFAULT PRIVILEGES` are emitted only when the privilege is
  missing from the stored ACL.
- Ownership `ALTER`s are emitted only for relations the principal does not own.

### Explicit ACLs, not has_*_privilege

`has_table_privilege` counts privileges held through membership and an owner's
implicit rights, so it would call a grant present that was never made, and
removing that membership would later take access away. The diff reads stored
ACLs with `aclexplode`. A NULL ACL has no explicit entries and reads as
missing.

The exception is what the grantee owns. PostgreSQL never records an owner's
rights, so a table its owner creates keeps a NULL ACL; counting it would plan
the same `GRANT` forever and never converge. Relations the grantee owns are
skipped, and no default privilege is registered for an owner to itself. If
ownership moves away, the relation counts again.

## Engine rules

Each has a test in `internal/engine/postgres/`.

**`ALTER DEFAULT PRIVILEGES` needs `FOR ROLE`.** Without it the entry binds to
the provisioning role, which creates no objects, so it never fires. Access then
works until a migration replaces an object. Defaults are registered for every
role that owns relations in the schema, from `pg_class` (the schema owner is
often not the object owner), plus the schema owner so an empty schema is
covered.

**Ownership covers views and materialized views.** Replacing a view requires
owning it, and `pg_tables`/`pg_sequences` miss both, so reassignment reads
`pg_class`.

**Column-owned sequences are never reassigned.** A serial or identity sequence
follows its table's owner, and PostgreSQL refuses to change it independently
(`cannot change owner of sequence ... is linked to table`). The query excludes
`pg_depend.deptype = 'a'` and orders tables first. The equivalence suite found
this one.

**Sequence privileges are an intersection.** A sequence accepts only `SELECT`,
`UPDATE` and `USAGE`; `INSERT` there is an error that aborts the transaction.

**`CREATE SCHEMA` without `IF NOT EXISTS`.** `CREATE SCHEMA IF NOT EXISTS ...
AUTHORIZATION` checks `SET ROLE` before existence, so it fails with nothing to
do on a role the admin cannot assume.

**`GRANT CONNECT, CREATE ON DATABASE`.** `CREATE` is checked before existence,
so even a service migrator's no-op `CREATE SCHEMA IF NOT EXISTS` needs it.

**`ownerOf` grants the admin membership in the role first.** `CREATE SCHEMA ...
AUTHORIZATION` and `ALTER ... OWNER TO` need `SET` on the role, and granting on
a schema it owns needs `INHERIT`. On PostgreSQL 16 (including RDS) a
`CREATEROLE` admin gets only `ADMIN OPTION` on roles it creates, since
`createrole_self_grant` is empty by default, so the first `ownerOf` resource
fails with `must be able to SET ROLE`. `ADMIN OPTION` suffices to grant the
rest, so the plan runs `GRANT <role> TO CURRENT_USER WITH INHERIT TRUE, SET
TRUE`, only when `ownerOf` work is planned. This check reads the admin's
effective standing (`pg_has_role`). On a role the admin holds no `ADMIN OPTION`
on, planning fails and names the one-time grant an administrator must run:
`GRANT <role> TO <admin> WITH ADMIN TRUE, INHERIT TRUE, SET TRUE`.

**Tolerated errors are an explicit list.** `Statement.Ignore` names the error
substrings a statement expects, instead of the usual `grep -v "already exists"`
that hides every error.

**`rds_iam` is checked while planning.** It exists only on RDS and Aurora, so
`grantRdsIam` against a self-managed PostgreSQL fails before anything is
applied. `grantRdsIam` (the service's role may log in with IAM) and
`instance.auth.method` (how the controller logs in) are independent.

## Best-effort statements

`ALTER DEFAULT PRIVILEGES FOR ROLE x` requires the admin to hold x's
privileges, which it may not. Making that fatal would stop provisioning for
every service over one unreachable owner, so it is a warning, recorded in
`status.warnings` because it means future objects may not inherit access.

## Modes

`spec.mode: Observe` builds the plan and records it in status
(`pendingStatements`, `pending`, the `Converged` condition) without executing a
statement, adding the finalizer, or revoking on delete. Observe receives only
the statement text, never the plan, so it has nothing to `Apply`.

It is for cutovers: zero pending statements on a database an existing
provisioner converged shows the two agree before the controller writes.

In Enforce, pending comes from a re-plan after the apply, so it reports what
did not take (a refused best-effort statement, or a `GRANT` accepted with "no
privileges were granted").

## Deletion

`spec.revokeOnDelete` defaults to false and the finalizer is added only when it
is true. Revocation never drops the role: a role that owns objects cannot be
dropped, and reassigning its objects is a human decision.

## Testing

- **Unit** tests build plans against a fake inspector.
- **Integration** (`make test-integration`) runs the generated SQL against
  PostgreSQL 16, as the superuser and as a non-superuser `CREATEROLE` admin.
  Only a server shows the SQL parses and re-runs.
- **envtest** (`make test-envtest`) runs the reconciler against a real
  kube-apiserver: CRD defaults, enums and the identifier patterns enforced at
  admission, status and conditions, and finalizer handling. The engine is
  faked.
- **Equivalence** (`make test-equivalence JOB_SCRIPT=... EQ_SCHEMA=...
  EQ_ROLE=...`) runs an existing provisioner script and this engine against the
  same seed, to convergence, and diffs ownership, ACLs and `pg_default_acl`.
  Use it before cutting a database over.

### Verifying the IAM auth path

Every suite authenticates with a password, so the token path (mint a SigV4 RDS
token, present it as the password) needs a separate check. It can be done
without AWS: put a proxy that terminates RDS IAM auth in front of PostgreSQL,
and run a credential agent serving the EKS Pod Identity contract as a sidecar
(the AWS SDK accepts `AWS_CONTAINER_CREDENTIALS_FULL_URI` only on loopback).
The chart's `extraContainers`, `extraEnv` and `extraVolumes` exist for this.
`rdsauth` loads credentials exactly as it does under Pod Identity.

- The endpoint must be exactly the host the proxy expects: the token signs
  host:port, so a mismatch reads as a bad token.
- A stock PostgreSQL has no `rds_iam`. Create a stub role, or set
  `grantRdsIam: false`.

## Not in scope

Creating RDS instances, Aurora clusters or DynamoDB tables. Those are AWS
control-plane calls that Crossplane's `provider-aws` already covers. DynamoDB
has no data-plane principals, so it will never appear here.
