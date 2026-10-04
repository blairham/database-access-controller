# Design: DatabaseAccess

**Status:** Draft — PostgreSQL implemented, MySQL and SQL Server not started
**Code:** `internal/engine/postgres/`, `internal/controller/databaseaccess/`

## Purpose

Provision the data plane of a managed database — the role, schemas, grants and
object ownership a service needs — with a controller rather than a one-shot Job.

The pattern this replaces is common: a Helm-templated Kubernetes Job running a
shell script that pipes SQL into `psql`, named by a hash of its own script body
so that editing the script produces a new resource. It works until it does not,
and everything below is a way it fails in practice.

## What a provisioning Job cannot do

**A Job is immutable on `.spec.template`.** Changing the script does not change
the Job, and a GitOps engine reconciling it will report success while applying
nothing — the immutable-field rejection is swallowed by a server-side diff.
Hashing the script into the resource name is the usual workaround, and it is
only as good as the hash: anything the hash does not cover can go stale
indefinitely while the resource reports as synced.

A controller has no immutable spec. The hash survives here as an observation in
`status.appliedPlanHash`, useful for answering "did anything change", but it is
no longer the mechanism that forces work to happen.

**A Job runs once.** Drift introduced afterwards — a migration creating a view
as the wrong role, a hand-run `GRANT` during an incident — persists until a
human notices. The controller re-plans on an interval and corrects it.

**A Job reports one exit code.** Provisioning SQL has statements that must not
be fatal (see *best-effort* below), so a script ends up swallowing errors, and
a half-applied grant then looks exactly like a fully-applied one. Those
failures belong on the resource, which is where `status.warnings` and a
Kubernetes event put them.

**A Job cannot be inspected.** When provisioning exists only as template text,
the sole way to learn what it will do to a database is to let it do it.
`dbctl plan` prints the statements against the real database and writes
nothing.

## Why the plan is built by reading first

The obvious way to write this SQL is server-side `DO` blocks that loop over
`pg_class` at execution time. That hides what will happen until it has
happened.

`BuildPlan` queries first and emits one concrete statement per object, so
`ALTER VIEW "ingest"."v_metadata" OWNER TO "ingest_app"` appears in the plan
before anyone runs it. The tradeoff is a race: an object created between
planning and applying is missed. It is picked up on the next reconcile, which
is exactly the loop a Job does not have.

## The failures encoded in the engine

Each of these has a test in `internal/engine/postgres/`.

**`ALTER DEFAULT PRIVILEGES` needs `FOR ROLE`.** Without it PostgreSQL records
the entry against the *current* role — the provisioning role — which creates no
objects, so the default never fires for anything. Every schema is populated by
its owning application role, not by the provisioner. The symptom is subtle: the
one-shot `GRANT ... ON ALL TABLES` covers everything that exists today, so
access works, and then a migration replaces a view or a table and the new
object silently carries no grant. Nothing re-runs, so it stays broken.

**The principal is added to the owner list when it will own the schema.** The
enumeration reflects the database as it is *before* the plan runs, and two
cases are invisible from there: a greenfield schema does not exist yet, so it
reports no owners at all; and on an existing schema, the reassignment further
down the same plan makes the principal the owner of relations the enumeration
attributed to someone else. Without this the first run registers nothing for
the role about to create every object, and only a later reconcile converges.
A read-only consumer is deliberately not added -- it creates nothing there, so
the entry would never fire.

**Owners come from `pg_class`, not `pg_namespace.nspowner`.** The schema owner
is not reliably the object owner — a schema created by an admin and populated
by an application role has two different answers. Deriving defaults from
`nspowner` alone leaves every relation in that schema uncovered. The schema
owner is unioned in anyway, so a greenfield schema with no relations yet still
registers a default for whoever creates the first one.

**Ownership reassignment must cover views and materialized views.** Driving it
off `pg_tables` and `pg_sequences` covers `relkind IN ('r','p')` and `'S'` and
silently skips `'v'` and `'m'`. This is not cosmetic: replacing a view requires
ownership of it, so a migration doing `DROP VIEW IF EXISTS ...; CREATE VIEW`
fails with `must be owner of view` even when every table it reads was
reassigned correctly. `IF EXISTS` suppresses the not-found error, not the
permission one.

**Sequence privileges are an intersection, not a pass-through.** A sequence
accepts only `SELECT`, `UPDATE` and `USAGE`. Letting `INSERT` reach the
sequence grant produces `invalid privilege type INSERT for sequence`, which
aborts the transaction and takes the whole run with it.

**`CREATE SCHEMA` is emitted only when the schema is absent.** A bare
`CREATE SCHEMA IF NOT EXISTS ... AUTHORIZATION` checks the `SET ROLE`
privilege *before* the existence short-circuit, so re-running it against a role
the provisioner did not create fails with `must be able to SET ROLE` with
nothing to do. PostgreSQL 16 grants a `CREATEROLE` creator `SET`/`ADMIN`
membership on roles it makes, so provisioner-created roles are fine and
pre-existing ones are not.

**`GRANT CONNECT, CREATE ON DATABASE`, not just `CONNECT`.** PostgreSQL checks
the `CREATE` privilege before it checks existence, so even a no-op
`CREATE SCHEMA IF NOT EXISTS` from a service's own migrator requires it.

**Tolerated errors are an explicit list.** PostgreSQL has no
`CREATE ROLE IF NOT EXISTS`, and the usual fix is piping `psql` stderr through
`grep -v "already exists"` — which suppresses the exit status for every error,
not only that one. `Statement.Ignore` names the substrings a given statement
expects and nothing else.

## Why best-effort is load-bearing

`ALTER DEFAULT PRIVILEGES FOR ROLE x` requires the current role to hold x's
privileges, and a provisioning role does not necessarily hold all of them — a
role granted with `set_option=f` cannot be assumed into. Making that fatal
takes provisioning down for every service the moment one unreachable owner
appears in one schema.

So it is a warning. But the warning reaches the resource rather than a pod log,
because it is the difference between "access was granted" and "access was
granted and will survive the next migration".

## Idempotency

Every statement is safe to re-run, and the plan converges:

- `CREATE ROLE` is omitted when the role exists, and still tolerates a
  concurrent create.
- `CREATE SCHEMA` is emitted only when the schema is absent.
- `GRANT` and `ALTER DEFAULT PRIVILEGES` are emitted only when the privilege
  they confer is missing from the stored ACL (see below).
- Ownership `ALTER`s are emitted only for relations the principal does not
  already own, and never for column-owned sequences.

**The plan is a diff.** A converged database plans zero statements; a grant
revoked by hand brings back exactly the statement that repairs it. Measured on
the 11 real staging grant sets: once converged, every resource reports
`pendingStatements: 0` and applies 0 statements per reconcile, and a manual
`REVOKE SELECT ON oms.orders FROM oms_app` reappears as the single statement
`GRANT SELECT ON ALL TABLES IN SCHEMA "oms" TO "oms_app"`.

It was not always. The planner used to read only role existence, schema
existence and object owners, and re-issued every grant on every pass. A revoke
was then invisible to the plan -- revoking `USAGE` left the plan hash unchanged
-- so skipping a reconcile whose hash matched `status.appliedPlanHash` would
have left drift in place forever while reporting Ready. Re-applying on every
reconcile was the only safe choice. `TestReapplyRepairsARevokedGrant` used to
pin that the hash did NOT move; it now pins that the settled plan is empty and
the revoke reappears as exactly one statement, and the repair half is
unchanged.

The controller still re-plans on every reconcile and applies whatever the plan
holds. It never skips on a hash.

### Why the diff reads explicit ACLs, not has_*_privilege

`has_table_privilege` answers "can this role do it", counting privileges held
through role membership and an owner's implicit rights. Planning from it would
call a grant present that was never made. Measured: with that check, the
controller skipped the GRANT on every object a role owns, leaving those ACLs
NULL where the Job it replaces had materialized explicit entries
(`covenant.policies|covenant_app=arwdDxt/covenant_app` and six more lines in
the equivalence diff). Access was the same in that case. It would not be for a
grant that exists only through a membership someone later removes.

The reads therefore use `aclexplode` over the stored ACL. A NULL ACL (an object
on the built-in default) has no explicit entries, so it reads as missing and
the GRANT is planned. That is also what makes the result match the Job.

## Modes

`spec.mode: Observe` builds the plan and records it in status
(`pendingStatements`, `pending`, a `Converged` condition) without executing a
statement, adding the revoke finalizer, or revoking on delete. The reconciler
hands Observe only the plan's statement text, never the plan, so that path has
no step it could call `Apply` on.

It is the cutover tool. Enforce next to an existing provisioner proves only
that the controller does no harm, because the old grants hide a controller that
does nothing. Observe on the database that provisioner converged reports the
exact difference -- zero on all 11 staging grant sets in the equivalence
harness -- before the controller is allowed to write.

In Enforce, pending is recorded from a fresh plan built after the apply rather
than from the plan just applied. The applied plan cannot say what did not take:
a best-effort default privilege that was refused, or a GRANT PostgreSQL accepted
with "no privileges were granted". The re-plan costs one more round of catalog
reads and no writes.

## Deletion

`spec.revokeOnDelete` defaults to false, and the finalizer is added only when it
is true.

A role that owns objects cannot be dropped, and reassigning its objects
elsewhere needs a human looking at the data. Even revocation is less complete
than it appears: `ALTER DEFAULT PRIVILEGES FOR ROLE` leaves a `pg_default_acl`
entry that is itself a dependency, so `DROP OWNED BY` is required before the
role can go. Holding a finalizer that does nothing turns a stuck controller
into a namespace that cannot be deleted.

## Testing

Unit tests prove the SQL reads correctly. Only a server proves it parses, that
the privilege split is one PostgreSQL accepts, and that the plan is genuinely
re-runnable — so `internal/engine/postgres/integration_test.go` runs the
generated statements against PostgreSQL 16 behind a build tag, seeding the
shape real databases are in: an application role owning the relations, a schema
owned by someone else, and a view among the tables.

```sh
make test-integration
```

### The controller contract

`internal/controller/databaseaccess` is tested against a real kube-apiserver
via envtest, because two things matter there that no function call reveals.

The **CRD schema as the API server enforces it**: that `engine` defaults to
`postgres` and the enum rejects anything else, and that the identifier patterns
on `role` and `schema` are applied at admission. Those patterns are the only
thing standing between a CRD field and injected SQL, since PostgreSQL does not
accept a parameter where an identifier is required. A Go-side check is not
enough on its own -- anything that writes the resource bypasses it.

The **reconcile contract**: status subresource writes, conditions, that a
failure lands on the resource rather than being retried silently, that the
finalizer is added only when `revokeOnDelete` asks for one, and that deletion
runs the revoke plan before releasing it.

### Verifying the IAM auth path

Every suite above authenticates with a password, because a PostgreSQL in a
container has no IAM. That leaves the production credential path -- mint a
SigV4 RDS token, present it as the password -- covered by nothing.

It can be verified without AWS. The pieces are a PostgreSQL whose 5432 is owned
by a proxy that terminates RDS IAM auth and forwards to the real server on
loopback, plus a credential agent serving the EKS Pod Identity contract on
127.0.0.1. The agent has to be a SIDECAR rather than its own Service: the AWS
SDK refuses `AWS_CONTAINER_CREDENTIALS_FULL_URI` on anything but loopback.
`extraContainers`, `extraEnv` and `extraVolumes` in the chart exist for exactly
this.

The controller's own code is unchanged in that setup -- `rdsauth` loads
credentials the same way it does under Pod Identity -- so what it proves is the
real path. Verified this way: the proxy logged `postgres auth accepted ...
tokenTTLRemaining=14m59s` and the schema, ACLs and default privileges came out
correct.

Two things to get right, both of which fail in confusing ways:

- **The endpoint must be the host the token was signed against**, exactly. The
  signature covers host:port, so a resource pointing at a Service shortname
  while the proxy expects an FQDN is rejected as a bad token rather than as a
  name mismatch.
- **A rig can carry `rds_iam` too.** A proxy that terminates the IAM handshake
  makes the CONNECTION look like RDS; it does not, on its own, make the SERVER
  look like RDS, and a stock PostgreSQL has only the `pg_*` predefined roles.
  Where the rig also creates an `rds_iam` stub, `grantRdsIam` can stay at its
  production value instead of being switched off for the rig -- which is the
  point, since a setting that differs in testing is a setting that is not being
  tested.

- **`spec.grantRdsIam` is a different thing from `instance.auth.method`.**
  `grantRdsIam` grants the `rds_iam` role to the SERVICE's role, so the service
  can authenticate with a token; `auth.method` is how the CONTROLLER
  authenticates its own admin connection. They are independent -- the
  controller can connect with a password to a real RDS instance and still grant
  `rds_iam`.

  The field was called `iamAuth`, and against `auth.method: iam` sitting a few
  lines above it in the same resource, the two read as one setting. Renamed
  while `v1alpha1` and nothing was deployed; the engine keeps the neutral
  spelling `IAMAuth`, because MySQL asks for the same thing through the
  AWSAuthenticationPlugin.

### Differential testing against the implementation being replaced

Unit and integration tests prove the engine is correct on its own terms. They
cannot prove it is a *drop-in* for the script it replaces, and that is the
question that matters when cutting over a database that already has state.

`equivalence_test.go` answers it by running both implementations against the
same seed **to convergence** and diffing the resulting state -- object ownership, schema and
relation ACLs, and `pg_default_acl` entries. Comparing the two SQL texts would
not work: the older implementation runs server-side `DO` blocks that loop over
`pg_class` at execution time, while this one reads first and emits concrete
statements, so the texts are expected to differ. Only the end state has to
match.

```sh
make test-equivalence JOB_SCRIPT=/path/to/rendered-script.sh EQ_SCHEMA=app EQ_ROLE=app_role
```

**This is how the column-owned sequence bug was found.** A serial or identity
sequence is auto-dependent on its table's column, and PostgreSQL refuses to
change its owner independently:

```
ERROR: cannot change owner of sequence "events_id_seq" (SQLSTATE 0A000)
DETAIL: Sequence "events_id_seq" is linked to table "events".
```

Reassigning the table is both necessary and sufficient -- the sequence follows.
Emitting the `ALTER SEQUENCE` is an error, not merely a redundant statement.
The older implementation never hit it by construction: it reassigned tables
first, from `pg_tables`, and its separate sequence loop then found nothing left
to do. Planning everything from a single read loses that accident, so the query
now excludes column-owned sequences outright (`pg_depend.deptype = 'a'`) and
orders tables ahead of sequences.

No unit test would have caught this. The statement is well-formed, the fake
inspector happily returns the sequence, and the failure exists only in
PostgreSQL's rules about what may own what.

**Convergence, not a single run, is what gets compared**, and the difference is
real rather than a convenience. The older implementation enumerates owners
before its reassignment blocks, so its first pass registers default privileges
for the old owners only and the incoming role picks its entry up on the next
run. This engine adds that role during the first pass. The two disagree after
one run and agree once settled; for a controller that re-reconciles, settled
state is the property that matters, and reaching it in one pass rather than two
is the improvement.

## Not in scope

Creating the RDS instance, the Aurora cluster or the DynamoDB table. Those are
AWS control-plane calls that Crossplane's upjet-generated `provider-aws`
already covers, generated from the AWS API. DynamoDB has no data-plane
principals at all — access to a table is an IAM policy — so it will never
appear here.
