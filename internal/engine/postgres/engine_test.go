// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

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

	// The ACL reads. Zero values mean "nothing granted", so a test that sets
	// none of these sees the full plan -- the shape every older test asserts.
	members     map[string]bool     // "member/group"
	dbPrivs     map[string][]string // "database/grantee"
	schemaPrivs map[string][]string // "schema/grantee"
	tablesOK    map[string]bool     // "schema/grantee/kinds" -> no relation lacks a privilege
	defaults    map[string][]string // "owner/schema/objtype/grantee"

	// access is the admin's standing per role. A role absent from the map
	// reads as full access -- what a superuser sees, and what every test
	// written before this read existed implicitly assumed.
	access map[string]AdminAccess
}

func (f fakeInspector) AdminAccessTo(_ context.Context, role string) (AdminAccess, error) {
	if a, ok := f.access[role]; ok {
		return a, nil
	}
	return AdminAccess{Admin: "admin", Set: true, Inherit: true, Grant: true}, nil
}

func (f fakeInspector) RoleIsMemberOf(_ context.Context, member, group string) (bool, error) {
	return f.members[member+"/"+group], nil
}

func (f fakeInspector) DatabasePrivileges(_ context.Context, database, grantee string) ([]string, error) {
	return f.dbPrivs[database+"/"+grantee], nil
}

func (f fakeInspector) SchemaPrivileges(_ context.Context, schema, grantee string) ([]string, error) {
	return f.schemaPrivs[schema+"/"+grantee], nil
}

func (f fakeInspector) RelationsLackPrivileges(
	_ context.Context,
	schema, grantee string,
	kinds, _ []string,
) (bool, error) {
	return !f.tablesOK[schema+"/"+grantee+"/"+strings.Join(kinds, "")], nil
}

func (f fakeInspector) DefaultPrivileges(_ context.Context, owner, schema, objType, grantee string) ([]string, error) {
	return f.defaults[owner+"/"+schema+"/"+objType+"/"+grantee], nil
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

// planText returns only the SQL of a plan. Describe() includes rationale prose,
// which can mention statements the plan does not contain.
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
	// rds_iam must exist for the grant below to be plannable.
	e := New(nil, fakeInspector{roles: map[string]bool{"rds_iam": true}}, nil)
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

// The default privilege must be registered FOR ROLE each role that owns
// objects; against the provisioner it never fires.
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

	mustContain(
		t,
		got,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "rds_superuser" IN SCHEMA "app" GRANT SELECT ON TABLES TO "reporting_ro";`,
	)
	mustContain(
		t,
		got,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "app_writer" IN SCHEMA "app" GRANT SELECT ON TABLES TO "reporting_ro";`,
	)
	mustContain(
		t,
		got,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "app_writer" IN SCHEMA "app" GRANT SELECT ON SEQUENCES TO "reporting_ro";`,
	)
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
// sequences; replacing a view requires owning it.
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

// CREATE SCHEMA is emitted only when the schema is absent: IF NOT EXISTS ...
// AUTHORIZATION still checks SET ROLE first.
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
	e := New(nil, fakeInspector{
		// rds_iam must be present, or asking for the grant is a planning error.
		roles:  map[string]bool{"rds_iam": true},
		owners: map[string][]string{"app": {"app_writer"}},
	}, nil)
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
	e := New(nil, fakeInspector{schemas: map[string]bool{"app": true}}, nil)
	base := engine.Access{Database: "appdb", Principal: "app"}

	p1, err := e.BuildPlan(context.Background(), base)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	// Granting a schema is the representative change. IAMAuth used to be the
	// lever here, and stopped being usable as one once asking for rds_iam
	// against a server without that role became a planning error.
	base.Namespaces = []engine.Namespace{{Name: "app", Privileges: []string{"SELECT"}}}
	p2, err := e.BuildPlan(context.Background(), base)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if p1.Hash() == p2.Hash() {
		t.Error("plan hash did not change when the plan did")
	}
}

// An ownerOf role registers no default privileges for itself: the self-grant
// has no effect and is never recorded, so it would never read as converged.
func TestOwnerRegistersNoDefaultPrivilegesForItself(t *testing.T) {
	e := New(nil, fakeInspector{schemas: map[string]bool{}}, nil)
	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "app_role",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT", "INSERT"}, Owner: true},
		},
	})

	mustContain(t, got, `CREATE SCHEMA "app" AUTHORIZATION "app_role";`)
	mustNotContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_role"`)
}

// Other owners in an owned schema still get defaults.
func TestOwnedSchemaRegistersDefaultsForOtherOwnersOnly(t *testing.T) {
	e := New(nil, fakeInspector{
		schemas: map[string]bool{"app": true},
		owners:  map[string][]string{"app": {"legacy_owner", "app_role"}},
	}, nil)
	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "app_role",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT"}, Owner: true},
		},
	})

	mustContain(
		t,
		got,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "legacy_owner" IN SCHEMA "app" GRANT SELECT ON TABLES TO "app_role";`,
	)
	mustNotContain(t, got, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_role"`)
}

// A consumer that does not own the schema gets no default for itself.
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

// The owner list is deduplicated.
func TestDefaultPrivilegeOwnersAreDeduplicated(t *testing.T) {
	e := New(nil, fakeInspector{
		schemas: map[string]bool{"app": true},
		owners:  map[string][]string{"app": {"legacy_owner", "legacy_owner"}},
	}, nil)
	p, err := e.BuildPlan(context.Background(), engine.Access{
		Database:  "appdb",
		Principal: "reporting_ro",
		Namespaces: []engine.Namespace{
			{Name: "app", Privileges: []string{"SELECT"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	count := 0
	for _, s := range p.Steps() {
		if strings.Contains(
			s.Describe(),
			`ALTER DEFAULT PRIVILEGES FOR ROLE "legacy_owner" IN SCHEMA "app" GRANT SELECT ON TABLES`,
		) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the same default-privilege statement appears %d times, want 1", count)
	}
}

// CREATE ROLE is omitted when the role already exists, keeping the server log
// quiet.
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

// "already exists" is still tolerated, for a race with a concurrent create.
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

// Asking for rds_iam on a server that has no such role fails while planning,
// not part-way through applying.
func TestGrantRdsIamFailsPlanningWhenTheRoleIsAbsent(t *testing.T) {
	e := New(nil, fakeInspector{roles: map[string]bool{}}, nil)
	_, err := e.BuildPlan(context.Background(), engine.Access{
		Database:  "appdb",
		Principal: "app_role",
		IAMAuth:   true,
	})
	if err == nil {
		t.Fatal("BuildPlan succeeded against a server with no rds_iam role, want an error")
	}
	for _, want := range []string{"rds_iam", "RDS and Aurora", "grantRdsIam=false", "instance.auth.method"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestGrantRdsIamPlansWhenTheRoleExists(t *testing.T) {
	e := New(nil, fakeInspector{roles: map[string]bool{"rds_iam": true}}, nil)
	got := planText(t, e, engine.Access{
		Database:  "appdb",
		Principal: "app_role",
		IAMAuth:   true,
	})
	mustContain(t, got, `GRANT rds_iam TO "app_role";`)
}

// Nothing is checked when the grant was not asked for.
func TestNoRdsIamCheckWhenNotRequested(t *testing.T) {
	e := New(nil, fakeInspector{roles: map[string]bool{}}, nil)
	got := planText(t, e, engine.Access{Database: "appdb", Principal: "app_role"})
	mustNotContain(t, got, "rds_iam")
}

// convergedInspector describes a database that already holds everything the
// access in diffAccess asks for.
func convergedInspector() fakeInspector {
	tableKinds := strings.Join(tableGrantKinds, "")
	seqKinds := strings.Join(sequenceGrantKinds, "")
	return fakeInspector{
		roles:       map[string]bool{"rds_iam": true, "app_role": true},
		schemas:     map[string]bool{"app": true},
		owners:      map[string][]string{"app": {"app_role"}},
		members:     map[string]bool{"app_role/rds_iam": true},
		dbPrivs:     map[string][]string{"appdb/app_role": {"CONNECT", "CREATE", "TEMPORARY"}},
		schemaPrivs: map[string][]string{"app/app_role": {"CREATE", "USAGE"}},
		tablesOK: map[string]bool{
			"app/app_role/" + tableKinds: true,
			"app/app_role/" + seqKinds:   true,
		},
		defaults: map[string][]string{
			"app_role/app/r/app_role": {"DELETE", "INSERT", "SELECT", "UPDATE"},
			"app_role/app/S/app_role": {"SELECT", "UPDATE", "USAGE"},
		},
	}
}

var diffAccess = engine.Access{
	Database:  "appdb",
	Principal: "app_role",
	IAMAuth:   true,
	Namespaces: []engine.Namespace{{
		Name:       "app",
		Privileges: []string{"USAGE", "CREATE", "SELECT", "INSERT", "UPDATE", "DELETE"},
		Owner:      true,
	}},
}

// A database that already matches the spec produces an empty plan.
func TestBuildPlanIsEmptyWhenTheDatabaseAlreadyMatches(t *testing.T) {
	e := New(nil, convergedInspector(), nil)
	p, err := e.BuildPlan(context.Background(), diffAccess)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Len() != 0 {
		t.Errorf("plan has %d statement(s) against a converged database, want 0:\n%s", p.Len(), p.Describe())
	}
}

// Each missing piece brings back exactly its own statement.
func TestBuildPlanPlansOnlyWhatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *fakeInspector)
		want   string
	}{
		{
			"rds_iam membership", func(f *fakeInspector) { delete(f.members, "app_role/rds_iam") },
			`GRANT rds_iam TO "app_role";`,
		},
		{
			"database CREATE", func(f *fakeInspector) { f.dbPrivs["appdb/app_role"] = []string{"CONNECT"} },
			`GRANT CONNECT, CREATE ON DATABASE "appdb" TO "app_role";`,
		},
		{
			"schema USAGE", func(f *fakeInspector) { f.schemaPrivs["app/app_role"] = []string{"CREATE"} },
			`GRANT USAGE, CREATE ON SCHEMA "app" TO "app_role";`,
		},
		{"a table grant", func(f *fakeInspector) {
			delete(f.tablesOK, "app/app_role/"+strings.Join(tableGrantKinds, ""))
		}, `GRANT DELETE, INSERT, SELECT, UPDATE ON ALL TABLES IN SCHEMA "app" TO "app_role";`},
		{"a sequence grant", func(f *fakeInspector) {
			delete(f.tablesOK, "app/app_role/"+strings.Join(sequenceGrantKinds, ""))
		}, `GRANT SELECT, UPDATE, USAGE ON ALL SEQUENCES IN SCHEMA "app" TO "app_role";`},
		{"one default privilege", func(f *fakeInspector) {
			f.owners["app"] = []string{"app_role", "legacy_owner"}
			f.defaults["legacy_owner/app/r/app_role"] = []string{"SELECT", "INSERT", "UPDATE"}
			f.defaults["legacy_owner/app/S/app_role"] = []string{"SELECT", "UPDATE", "USAGE"}
		}, `ALTER DEFAULT PRIVILEGES FOR ROLE "legacy_owner" IN SCHEMA "app" GRANT DELETE, INSERT, SELECT, UPDATE ON TABLES TO "app_role";`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := convergedInspector()
			tc.mutate(&f)
			got := planText(t, New(nil, f, nil), diffAccess)
			if strings.TrimSpace(got) != tc.want {
				t.Errorf("plan:\n%s\nwant exactly:\n%s", got, tc.want)
			}
		})
	}
}

// selfGrant is the membership statement ownerOf needs before acting as the
// principal.
const selfGrant = `GRANT "app_role" TO CURRENT_USER WITH INHERIT TRUE, SET TRUE;`

// On a role this plan creates the admin holds only ADMIN OPTION (PostgreSQL
// 16), so ownerOf work needs the self-grant after CREATE ROLE and before
// anything done as the role.
func TestGreenfieldOwnerOfGrantsTheAdminMembershipBeforeActingAsTheRole(t *testing.T) {
	e := New(nil, fakeInspector{roles: map[string]bool{"rds_iam": true}}, nil)
	got := planText(t, e, diffAccess)

	create := strings.Index(got, `CREATE ROLE "app_role" WITH LOGIN;`)
	grant := strings.Index(got, selfGrant)
	schema := strings.Index(got, `CREATE SCHEMA "app" AUTHORIZATION "app_role";`)
	if create < 0 || grant < 0 || schema < 0 {
		t.Fatalf("plan is missing a step (create=%d grant=%d schema=%d):\n%s", create, grant, schema, got)
	}
	if create >= grant || grant >= schema {
		t.Errorf("want CREATE ROLE < self-grant < CREATE SCHEMA, got %d, %d, %d:\n%s", create, grant, schema, got)
	}
	if n := strings.Count(got, "TO CURRENT_USER"); n != 1 {
		t.Errorf("self-grant planned %d times, want once:\n%s", n, got)
	}
}

// An existing role the admin can already act as needs nothing.
func TestOwnerOfPlansNoSelfGrantWhenTheAdminCanActAsTheRole(t *testing.T) {
	f := convergedInspector()
	delete(f.tablesOK, "app/app_role/"+strings.Join(tableGrantKinds, "")) // some ownerOf work
	f.access = map[string]AdminAccess{"app_role": {Admin: "prov", Set: true, Inherit: true}}
	mustNotContain(t, planText(t, New(nil, f, nil), diffAccess), "TO CURRENT_USER")
}

// Missing either SET or INHERIT plans the self-grant, provided the admin holds
// ADMIN OPTION.
func TestOwnerOfPlansTheSelfGrantWhenSetOrInheritIsMissing(t *testing.T) {
	for name, acc := range map[string]AdminAccess{
		"no SET":     {Admin: "prov", Inherit: true, Grant: true},
		"no INHERIT": {Admin: "prov", Set: true, Grant: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := convergedInspector()
			delete(f.tablesOK, "app/app_role/"+strings.Join(tableGrantKinds, ""))
			f.access = map[string]AdminAccess{"app_role": acc}
			got := planText(t, New(nil, f, nil), diffAccess)
			mustContain(t, got, selfGrant)
			if strings.Index(got, selfGrant) > strings.Index(got, "ON ALL TABLES") {
				t.Errorf("self-grant must precede the namespace statements:\n%s", got)
			}
		})
	}
}

// Without ADMIN OPTION, planning fails with the statement an administrator
// has to run.
func TestOwnerOfFailsPlanningWithTheRunbookGrantWhenTheAdminCannotGrantItself(t *testing.T) {
	f := convergedInspector()
	delete(f.tablesOK, "app/app_role/"+strings.Join(tableGrantKinds, ""))
	f.access = map[string]AdminAccess{"app_role": {Admin: "db_provisioner"}}
	_, err := New(nil, f, nil).BuildPlan(context.Background(), diffAccess)
	if err == nil {
		t.Fatal("BuildPlan succeeded; want a planning error naming the one-time grant")
	}
	want := `GRANT "app_role" TO "db_provisioner" WITH ADMIN TRUE, INHERIT TRUE, SET TRUE`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error does not name the fix\n got: %v\nwant substring: %s", err, want)
	}
}

// The membership is only checked when ownerOf has something to do, so a
// converged database plans nothing.
func TestConvergedOwnerOfNeedsNoMembership(t *testing.T) {
	f := convergedInspector()
	f.access = map[string]AdminAccess{"app_role": {Admin: "db_provisioner"}} // no SET, INHERIT or ADMIN
	p, err := New(nil, f, nil).BuildPlan(context.Background(), diffAccess)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Len() != 0 {
		t.Errorf("plan has %d statement(s), want 0:\n%s", p.Len(), p.Describe())
	}
}

// Without ownerOf nothing is done as the role, so no membership is needed
// even when the admin has none.
func TestReadOnlyAccessNeedsNoMembership(t *testing.T) {
	f := fakeInspector{
		roles:  map[string]bool{"rds_iam": true, "app_role": true},
		access: map[string]AdminAccess{"app_role": {Admin: "db_provisioner"}},
	}
	got := planText(t, New(nil, f, nil), engine.Access{
		Database:   "appdb",
		Principal:  "app_role",
		Namespaces: []engine.Namespace{{Name: "app", Privileges: []string{"SELECT"}}},
	})
	mustNotContain(t, got, "TO CURRENT_USER")
}
