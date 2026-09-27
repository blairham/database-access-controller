package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blairham/database-controller/internal/engine"
)

// fakeInspector serves canned answers so plan construction can be tested
// without a database.
type fakeInspector struct {
	roles     map[string]bool
	schemas   map[string]bool
	owners    map[string][]string
	relations map[string][]Relation
}

func (f fakeInspector) RoleExists(_ context.Context, role string) (bool, error) {
	return f.roles[role], nil
}

func (f fakeInspector) SchemaExists(_ context.Context, schema string) (bool, error) {
	return f.schemas[schema], nil
}

func (f fakeInspector) ObjectOwners(_ context.Context, schema string) ([]string, error) {
	return f.owners[schema], nil
}

func (f fakeInspector) RelationsNotOwnedBy(_ context.Context, schema, owner string) ([]Relation, error) {
	var out []Relation
	for _, r := range f.relations[schema] {
		if r.Owner != owner {
			out = append(out, r)
		}
	}
	return out, nil
}

// planText returns only the SQL of a plan, with the rationale comments
// stripped. Asserting against Describe() would match prose: the rationale for
// GRANT CONNECT mentions "CREATE SCHEMA IF NOT EXISTS", which is enough to make
// a test looking for a CREATE SCHEMA statement pass on a plan that has none.
func planText(t *testing.T, e *Engine, a engine.Access) string {
	t.Helper()
	p, err := e.BuildPlan(context.Background(), a)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	var b strings.Builder
	for _, s := range p.Steps() {
		b.WriteString(s.Describe())
		b.WriteString("\n")
	}
	return b.String()
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("plan is missing:\n  %s\n\nfull plan:\n%s", want, got)
	}
}

func mustNotContain(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Errorf("plan unexpectedly contains:\n  %s\n\nfull plan:\n%s", want, got)
	}
}

func TestBuildPlanRoleAndDatabase(t *testing.T) {
	e := New(nil, fakeInspector{}, nil)
	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "ingest_app",
		IAMAuth:   true,
	})

	mustContain(t, got, `CREATE ROLE "ingest_app" WITH LOGIN;`)
	mustContain(t, got, `GRANT rds_iam TO "ingest_app";`)
	mustContain(t, got, `GRANT CONNECT, CREATE ON DATABASE "appdb" TO "ingest_app";`)
}

func TestBuildPlanOmitsIAMGrantWhenNotRequested(t *testing.T) {
	e := New(nil, fakeInspector{}, nil)
	got := planText(t, e, engine.Access{Database: "appdb", Principal: "ingest_app"})
	mustNotContain(t, got, "rds_iam")
}

// The default privilege must be registered FOR ROLE each role that actually
// creates objects. Registered against the provisioner instead, it binds to a
// role that creates nothing and never fires -- the failure that left
// reporting_ro without SELECT after a view was replaced.
func TestBuildPlanRegistersDefaultPrivilegesPerOwningRole(t *testing.T) {
	e := New(nil, fakeInspector{
		owners: map[string][]string{"app": {"rds_superuser", "app_writer"}},
	}, nil)

	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "reporting_ro",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT"}},
		},
	})

	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "rds_superuser" IN SCHEMA "app" GRANT SELECT ON TABLES TO "reporting_ro";`)
	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_writer" IN SCHEMA "app" GRANT SELECT ON TABLES TO "reporting_ro";`)
	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_writer" IN SCHEMA "app" GRANT SELECT ON SEQUENCES TO "reporting_ro";`)
}

func TestDefaultPrivilegeStatementsAreBestEffort(t *testing.T) {
	e := New(nil, fakeInspector{owners: map[string][]string{"app": {"app_enricher"}}}, nil)
	p, err := e.BuildPlan(context.Background(), engine.Access{
		Database:   "appdb",
		Principal:  "reports_ro",
		Namespaces: []engine.Namespace{{Name: "app", Privileges: []string{"SELECT"}}},
	})
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}

	for _, s := range p.Steps() {
		if strings.HasPrefix(s.Describe(), "ALTER DEFAULT PRIVILEGES") && !s.IsBestEffort() {
			t.Errorf("ALTER DEFAULT PRIVILEGES is fatal, want best-effort: %s", s.Describe())
		}
		if strings.HasPrefix(s.Describe(), "GRANT CONNECT") && s.IsBestEffort() {
			t.Errorf("GRANT CONNECT is best-effort, want fatal: %s", s.Describe())
		}
	}
}

// Views and materialized views must be reassigned along with tables and
// sequences. Driving the reassignment off pg_tables and pg_sequences skips
// them, and replacing a view requires ownership of it -- so a migration doing
// DROP VIEW / CREATE VIEW fails even when every table it reads was reassigned.
func TestBuildPlanReassignsViewsAndMatviews(t *testing.T) {
	e := New(nil, fakeInspector{
		schemas: map[string]bool{"ingest": true},
		owners:  map[string][]string{"ingest": {"ingest_app"}},
		relations: map[string][]Relation{"ingest": {
			{Name: "fixtures", Kind: RelKindTable, Owner: "rds_superuser"},
			{Name: "fixtures_id_seq", Kind: RelKindSequence, Owner: "rds_superuser"},
			{Name: "v_fixture_metadata", Kind: RelKindView, Owner: "rds_superuser"},
			{Name: "mv_fixture_rollup", Kind: RelKindMatView, Owner: "rds_superuser"},
			{Name: "already_mine", Kind: RelKindTable, Owner: "ingest_app"},
		}},
	}, nil)

	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "ingest_app",
		Namespaces: []engine.Namespace{
			{Name: "ingest", Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}, Owner: true},
		},
	})

	mustContain(t, got, `ALTER TABLE "ingest"."fixtures" OWNER TO "ingest_app";`)
	mustContain(t, got, `ALTER SEQUENCE "ingest"."fixtures_id_seq" OWNER TO "ingest_app";`)
	mustContain(t, got, `ALTER VIEW "ingest"."v_fixture_metadata" OWNER TO "ingest_app";`)
	mustContain(t, got, `ALTER MATERIALIZED VIEW "ingest"."mv_fixture_rollup" OWNER TO "ingest_app";`)
	mustNotContain(t, got, `"already_mine"`)
}

// CREATE SCHEMA is emitted only when the schema is genuinely absent. A bare
// CREATE SCHEMA IF NOT EXISTS ... AUTHORIZATION checks the SET ROLE privilege
// before the existence short-circuit, so on re-runs against a legacy role it
// fails with "must be able to SET ROLE" with nothing to do.
func TestBuildPlanCreatesSchemaOnlyWhenAbsent(t *testing.T) {
	absent := New(nil, fakeInspector{schemas: map[string]bool{}}, nil)
	got := planText(t, absent, engine.Access{
		Database:   "appdb",
		Principal:  "catalog_app",
		Namespaces: []engine.Namespace{{Name: "catalog", Privileges: []string{"SELECT"}, Owner: true}},
	})
	mustContain(t, got, `CREATE SCHEMA "catalog" AUTHORIZATION "catalog_app";`)

	present := New(nil, fakeInspector{schemas: map[string]bool{"catalog": true}}, nil)
	got = planText(t, present, engine.Access{
		Database:   "appdb",
		Principal:  "catalog_app",
		Namespaces: []engine.Namespace{{Name: "catalog", Privileges: []string{"SELECT"}, Owner: true}},
	})
	mustNotContain(t, got, "CREATE SCHEMA")
}

func TestBuildPlanDoesNotCreateSchemaWhenNotOwner(t *testing.T) {
	e := New(nil, fakeInspector{schemas: map[string]bool{}}, nil)
	got := planText(t, e, engine.Access{
		Database:   "appdb",
		Principal:  "reports_ro",
		Namespaces: []engine.Namespace{{Name: "app", Privileges: []string{"SELECT"}}},
	})
	mustNotContain(t, got, "CREATE SCHEMA")
}

func TestPlanOrderingRoleBeforeGrants(t *testing.T) {
	e := New(nil, fakeInspector{owners: map[string][]string{"app": {"app_writer"}}}, nil)
	p, err := e.BuildPlan(context.Background(), engine.Access{
		Database:   "appdb",
		Principal:  "reports_ro",
		IAMAuth:    true,
		Namespaces: []engine.Namespace{{Name: "app", Privileges: []string{"SELECT"}}},
	})
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if got := p.Steps()[0].Describe(); !strings.HasPrefix(got, "CREATE ROLE") {
		t.Errorf("first statement is %q, want CREATE ROLE -- nothing can be granted to a role that does not exist", got)
	}
}

func TestValidateRejectsInjectionInIdentifiers(t *testing.T) {
	e := New(nil, fakeInspector{}, nil)
	err := e.Validate(engine.Access{
		Database:  "appdb",
		Principal: `x"; DROP DATABASE appdb; --`,
	})
	if err == nil {
		t.Fatal("Validate accepted a role name carrying a statement break, want an error")
	}
}

func TestValidateRejectsDuplicateSchemas(t *testing.T) {
	e := New(nil, fakeInspector{}, nil)
	err := e.Validate(engine.Access{
		Database:  "appdb",
		Principal: "app",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT"}},
			{Name: "app", Privileges: []string{"INSERT"}},
		},
	})
	if err == nil {
		t.Fatal("Validate accepted a duplicated schema, want an error")
	}
}

func TestPlanHashChangesWithContent(t *testing.T) {
	e := New(nil, fakeInspector{}, nil)
	base := engine.Access{Database: "appdb", Principal: "app"}
	p1, _ := e.BuildPlan(context.Background(), base)

	base.IAMAuth = true
	p2, _ := e.BuildPlan(context.Background(), base)

	if p1.Hash() == p2.Hash() {
		t.Error("plan hash did not change when the plan did")
	}
}

// A greenfield owned schema must register its default privileges in the SAME
// pass that creates it. The owner enumeration runs against the database as it
// is before the plan, where the schema does not exist and therefore has no
// owners at all -- so without adding the principal explicitly, the first run
// registers nothing and only a later reconcile converges.
func TestGreenfieldOwnedSchemaRegistersDefaultsInOnePass(t *testing.T) {
	e := New(nil, fakeInspector{schemas: map[string]bool{}}, nil)
	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "app_role",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT", "INSERT"}, Owner: true},
		},
	})

	mustContain(t, got, `CREATE SCHEMA "app" AUTHORIZATION "app_role";`)
	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_role" IN SCHEMA "app" GRANT INSERT, SELECT ON TABLES TO "app_role";`)
	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_role" IN SCHEMA "app" GRANT SELECT ON SEQUENCES TO "app_role";`)
}

// On an existing schema the principal is still added, because the ownership
// reassignment further down the same plan makes it the owner of relations the
// enumeration attributed to someone else.
func TestOwnedSchemaAddsThePrincipalToTheDefaultPrivilegeOwners(t *testing.T) {
	e := New(nil, fakeInspector{
		schemas: map[string]bool{"app": true},
		owners:  map[string][]string{"app": {"legacy_owner"}},
	}, nil)
	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "app_role",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT"}, Owner: true},
		},
	})

	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "legacy_owner" IN SCHEMA "app" GRANT SELECT ON TABLES TO "app_role";`)
	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_role" IN SCHEMA "app" GRANT SELECT ON TABLES TO "app_role";`)
}

// A consumer that does not own the schema must NOT have a default registered
// for itself: it creates nothing there, so the entry would never fire and
// would only add a statement that can fail on a role it cannot assume.
func TestReadOnlyAccessDoesNotAddThePrincipalAsAnOwner(t *testing.T) {
	e := New(nil, fakeInspector{
		schemas: map[string]bool{"app": true},
		owners:  map[string][]string{"app": {"legacy_owner"}},
	}, nil)
	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "reporting_ro",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT"}},
		},
	})

	mustContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "legacy_owner"`)
	mustNotContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "reporting_ro"`)
}

// The owner list is deduplicated: a principal already enumerated as an owner
// must not produce the same statement twice.
func TestDefaultPrivilegeOwnersAreDeduplicated(t *testing.T) {
	e := New(nil, fakeInspector{
		schemas: map[string]bool{"app": true},
		owners:  map[string][]string{"app": {"app_role"}},
	}, nil)
	p, err := e.BuildPlan(context.Background(), engine.Access{
		Database:  "appdb",
		Principal: "app_role",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT"}, Owner: true},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	count := 0
	for _, s := range p.Steps() {
		if strings.Contains(s.Describe(), `ALTER DEFAULT PRIVILEGES FOR ROLE "app_role" IN SCHEMA "app" GRANT SELECT ON TABLES`) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the same default-privilege statement appears %d times, want 1", count)
	}
}

// CREATE ROLE is omitted when the role already exists.
//
// Relying on the tolerated "already exists" error instead works, but every
// re-run then writes ERROR: role "x" already exists to the SERVER log -- one
// line per resource per reconcile, forever, in the log an operator greps
// during an incident.
func TestCreateRoleIsOmittedWhenTheRoleExists(t *testing.T) {
	e := New(nil, fakeInspector{roles: map[string]bool{"app_role": true}}, nil)
	got := planText(t, e, engine.Access{Database: "appdb", Principal: "app_role"})

	mustNotContain(t, got, "CREATE ROLE")
	mustContain(t, got, `GRANT CONNECT, CREATE ON DATABASE "appdb" TO "app_role";`)
}

func TestCreateRoleIsEmittedWhenTheRoleIsAbsent(t *testing.T) {
	e := New(nil, fakeInspector{roles: map[string]bool{}}, nil)
	got := planText(t, e, engine.Access{Database: "appdb", Principal: "app_role"})
	mustContain(t, got, `CREATE ROLE "app_role" WITH LOGIN;`)
}

// The tolerance stays as a backstop: between the existence check and the
// write, a concurrent reconcile or a human can create the role, and that race
// must not fail the plan.
func TestCreateRoleStillToleratesAConcurrentCreate(t *testing.T) {
	e := New(nil, fakeInspector{roles: map[string]bool{}}, nil)
	p, err := e.BuildPlan(context.Background(), engine.Access{Database: "appdb", Principal: "app_role"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for _, s := range p.Steps() {
		if !strings.HasPrefix(s.Describe(), "CREATE ROLE") {
			continue
		}
		if !s.Tolerates(errors.New(`pq: role "app_role" already exists`)) {
			t.Error("CREATE ROLE no longer tolerates a concurrent create")
		}
		return
	}
	t.Fatal("no CREATE ROLE statement in the plan")
}
