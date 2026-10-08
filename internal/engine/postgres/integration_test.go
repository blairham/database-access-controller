// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/blairham/database-access-controller/internal/engine"
)

// These tests run the generated statements against a real PostgreSQL server,
// proving the SQL parses, the privilege split is accepted, and plans re-run.
//
//	docker run --rm -d -p 5433:5432 -e POSTGRES_PASSWORD=test --name pgtest postgres:16-alpine
//	PGTEST_DSN='postgres://postgres:test@127.0.0.1:5433/postgres' go test -tags integration ./internal/engine/postgres/

func connect(t *testing.T) pgxConn {
	t.Helper()
	dsn := os.Getenv("PGTEST_DSN")
	if dsn == "" {
		t.Skip("PGTEST_DSN is unset; see the comment at the top of this file")
	}
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to PGTEST_DSN: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return pgxConn{c}
}

// seed builds a schema owned by someone other than the app role that owns its
// relations, with a view among the tables.
func seed(t *testing.T, c pgxConn, schema, appRole string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, QuoteIdent(schema))); err != nil {
		t.Fatalf("dropping schema %s: %v", schema, err)
	}
	dropRole(t, c, appRole)

	stmts := []string{
		fmt.Sprintf(`CREATE ROLE %s WITH LOGIN`, QuoteIdent(appRole)),
		fmt.Sprintf(`CREATE SCHEMA %s`, QuoteIdent(schema)),
		fmt.Sprintf(`CREATE TABLE %s.fixtures (id bigserial PRIMARY KEY, name text)`, QuoteIdent(schema)),
		fmt.Sprintf(
			`CREATE VIEW %s.v_fixture_metadata AS SELECT id, name FROM %s.fixtures`,
			QuoteIdent(schema),
			QuoteIdent(schema),
		),
		fmt.Sprintf(
			`CREATE MATERIALIZED VIEW %s.mv_rollup AS SELECT count(*) AS n FROM %s.fixtures`,
			QuoteIdent(schema),
			QuoteIdent(schema),
		),
		fmt.Sprintf(`ALTER TABLE %s.fixtures OWNER TO %s`, QuoteIdent(schema), QuoteIdent(appRole)),
	}
	for _, s := range stmts {
		if err := c.Exec(ctx, s); err != nil {
			t.Fatalf("seeding with %q: %v", s, err)
		}
	}
}

// dropRole removes a role and everything that depends on it. DROP OWNED BY
// first clears the default-privilege entries that would block DROP ROLE.
func dropRole(t *testing.T, c pgxConn, role string) {
	t.Helper()
	stmt := fmt.Sprintf(`
DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = %s) THEN
    EXECUTE 'DROP OWNED BY ' || quote_ident(%s) || ' CASCADE';
    EXECUTE 'DROP ROLE ' || quote_ident(%s);
  END IF;
END $$`, quoteLiteral(role), quoteLiteral(role), quoteLiteral(role))

	if err := c.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("dropping role %s: %v", role, err)
	}
}

// quoteLiteral renders a string as a SQL literal for embedding in a DO block.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func TestPlanExecutesAgainstRealPostgres(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest", "itest_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest CASCADE`)
		dropRole(t, c, "itest_reader")
		dropRole(t, c, "itest_app")
	})

	e := New(c, c, nil)
	access := engine.Access{
		Database:  "postgres",
		Principal: "itest_reader",
		IAMAuth:   false,
		Namespaces: []engine.Namespace{
			{Name: "itest", Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
		},
	}

	p, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	res, err := p.Apply(ctx)
	if err != nil {
		t.Fatalf("applying plan against real PostgreSQL: %v\n\nplan:\n%s", err, p.Describe())
	}
	if len(res.Warnings) > 0 {
		t.Logf("warnings (expected when the admin role lacks an owner's privileges): %v", res.Warnings)
	}

	// The role can actually read the table.
	var can bool
	if err := c.c.QueryRow(ctx,
		`SELECT has_table_privilege('itest_reader', 'itest.fixtures', 'SELECT')`).Scan(&can); err != nil {
		t.Fatalf("checking privilege: %v", err)
	}
	if !can {
		t.Error("itest_reader cannot SELECT from itest.fixtures after the plan applied")
	}
}

// Re-running must be a no-op rather than an error.
func TestPlanIsReRunnable(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest2", "itest2_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest2 CASCADE`)
		dropRole(t, c, "itest2_reader")
		dropRole(t, c, "itest2_app")
	})

	e := New(c, c, nil)
	access := engine.Access{
		Database:   "postgres",
		Principal:  "itest2_reader",
		Namespaces: []engine.Namespace{{Name: "itest2", Privileges: []string{"SELECT"}}},
	}

	for i := 1; i <= 3; i++ {
		p, err := e.BuildPlan(ctx, access)
		if err != nil {
			t.Fatalf("run %d: BuildPlan: %v", i, err)
		}
		if _, err := p.Apply(ctx); err != nil {
			t.Fatalf("run %d: applying an already-applied plan: %v", i, err)
		}
	}
}

// Ownership reassignment must cover views and materialized views.
func TestOwnershipReassignsViewsAndMatviews(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest3", "itest3_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest3 CASCADE`)
		dropRole(t, c, "itest3_app")
	})

	e := New(c, c, nil)
	p, err := e.BuildPlan(ctx, engine.Access{
		Database:  "postgres",
		Principal: "itest3_app",
		Namespaces: []engine.Namespace{
			{Name: "itest3", Privileges: []string{"SELECT", "INSERT"}, Owner: true},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := p.Apply(ctx); err != nil {
		t.Fatalf("applying: %v\n\nplan:\n%s", err, p.Describe())
	}

	for _, rel := range []string{"v_fixture_metadata", "mv_rollup"} {
		var owner string
		err := c.c.QueryRow(ctx, `
			SELECT pg_get_userbyid(c.relowner)
			  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'itest3' AND c.relname = $1`, rel).Scan(&owner)
		if err != nil {
			t.Fatalf("reading owner of %s: %v", rel, err)
		}
		if owner != "itest3_app" {
			t.Errorf("%s is owned by %q, want itest3_app -- replacing it would fail with 'must be owner of view'", rel, owner)
		}
	}
}

// ownerOf must reassign relations whose names the spec rule would reject:
// Entity Framework's PascalCase tables, and a name holding a quote, which
// QuoteIdent alone keeps a single identifier (#30).
func TestOwnerOfReassignsPascalCaseAndQuotedRelations(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest_pascal", "itest_pascal_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest_pascal CASCADE`)
		dropRole(t, c, "itest_pascal_app")
	})

	rels := []string{"Orders", "__EFMigrationsHistory", `we"ird`}
	for _, rel := range rels {
		stmt := fmt.Sprintf(`CREATE TABLE itest_pascal.%s (id int)`, QuoteIdent(rel))
		if err := c.Exec(ctx, stmt); err != nil {
			t.Fatalf("seeding with %q: %v", stmt, err)
		}
	}

	e := New(c, c, nil)
	access := engine.Access{
		Database:  "postgres",
		Principal: "itest_pascal_app",
		Namespaces: []engine.Namespace{
			{Name: "itest_pascal", Privileges: []string{"SELECT", "INSERT"}, Owner: true},
		},
	}
	p, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := p.Apply(ctx); err != nil {
		t.Fatalf("applying: %v\n\nplan:\n%s", err, p.Describe())
	}

	for _, rel := range rels {
		var owner string
		err := c.c.QueryRow(ctx, `
			SELECT pg_get_userbyid(c.relowner)
			  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'itest_pascal' AND c.relname = $1`, rel).Scan(&owner)
		if err != nil {
			t.Fatalf("reading owner of %s: %v", rel, err)
		}
		if owner != "itest_pascal_app" {
			t.Errorf("%s is owned by %q, want itest_pascal_app", rel, owner)
		}
	}

	again, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("re-planning: %v", err)
	}
	if again.Len() != 0 {
		t.Errorf("plan did not converge, want 0 statements:\n%s", again.Describe())
	}
}

// The privilege split must produce a sequence grant PostgreSQL accepts
// (INSERT is invalid on a sequence).
func TestSequenceGrantAcceptsTheSplitPrivileges(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest4", "itest4_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest4 CASCADE`)
		dropRole(t, c, "itest4_writer")
		dropRole(t, c, "itest4_app")
	})

	e := New(c, c, nil)
	p, err := e.BuildPlan(ctx, engine.Access{
		Database:  "postgres",
		Principal: "itest4_writer",
		Namespaces: []engine.Namespace{{
			Name:       "itest4",
			Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "USAGE"},
		}},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, err := p.Apply(ctx); err != nil {
		t.Fatalf("applying the full privilege set: %v\n\nplan:\n%s", err, p.Describe())
	}

	for _, s := range p.Steps() {
		d := s.Describe()
		if strings.Contains(d, "ON ALL SEQUENCES") && strings.Contains(d, "INSERT") {
			t.Errorf("INSERT reached the sequence grant: %s", d)
		}
	}
}

// A serial or identity sequence follows its table's owner, and PostgreSQL
// refuses to change it independently ("cannot change owner of sequence"), so
// the plan must not reassign it.
func TestOwnedSequencesAreNotReassigned(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest5", "itest5_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest5 CASCADE`)
		dropRole(t, c, "itest5_app")
	})

	e := New(c, c, nil)
	p, err := e.BuildPlan(ctx, engine.Access{
		Database:  "postgres",
		Principal: "itest5_app",
		Namespaces: []engine.Namespace{
			{Name: "itest5", Privileges: []string{"SELECT", "INSERT"}, Owner: true},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	for _, s := range p.Steps() {
		if strings.Contains(s.Describe(), `ALTER SEQUENCE "itest5"."fixtures_id_seq"`) {
			t.Errorf("plan reassigns a column-owned sequence, which PostgreSQL rejects outright: %s", s.Describe())
		}
	}

	if _, err := p.Apply(ctx); err != nil {
		t.Fatalf("applying: %v\n\nplan:\n%s", err, p.Describe())
	}

	// The sequence still ends up correctly owned -- it followed its table.
	var owner string
	if err := c.c.QueryRow(ctx, `
		SELECT pg_get_userbyid(c.relowner)
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'itest5' AND c.relname = 'fixtures_id_seq'`).Scan(&owner); err != nil {
		t.Fatalf("reading sequence owner: %v", err)
	}
	if owner != "itest5_app" {
		t.Errorf("fixtures_id_seq is owned by %q, want itest5_app via its table", owner)
	}
}

// Tables are planned before sequences.
func TestOwnershipPlansTablesBeforeSequences(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest6", "itest6_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest6 CASCADE`)
		dropRole(t, c, "itest6_app")
	})

	// A sequence with no owning column, so it is genuinely reassignable.
	if err := c.Exec(ctx, `CREATE SEQUENCE itest6.standalone_seq`); err != nil {
		t.Fatalf("creating standalone sequence: %v", err)
	}

	e := New(c, c, nil)
	p, err := e.BuildPlan(ctx, engine.Access{
		Database:  "postgres",
		Principal: "itest6_app",
		Namespaces: []engine.Namespace{
			{Name: "itest6", Privileges: []string{"SELECT"}, Owner: true},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	firstTable, firstSequence := -1, -1
	for i, s := range p.Steps() {
		d := s.Describe()
		if firstTable < 0 && strings.HasPrefix(d, "ALTER TABLE ") {
			firstTable = i
		}
		if firstSequence < 0 && strings.HasPrefix(d, "ALTER SEQUENCE ") {
			firstSequence = i
		}
	}
	if firstTable >= 0 && firstSequence >= 0 && firstSequence < firstTable {
		t.Errorf("a sequence is reassigned at step %d, before the first table at step %d", firstSequence, firstTable)
	}

	if _, err := p.Apply(ctx); err != nil {
		t.Fatalf("applying: %v\n\nplan:\n%s", err, p.Describe())
	}
}

// A grant revoked by hand shows up in the next plan as exactly the statement
// that repairs it, and re-applying restores it.
func TestReapplyRepairsARevokedGrant(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest7", "itest7_app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS itest7 CASCADE`)
		dropRole(t, c, "itest7_reader")
		dropRole(t, c, "itest7_app")
	})

	e := New(c, c, nil)
	access := engine.Access{
		Database:   "postgres",
		Principal:  "itest7_reader",
		Namespaces: []engine.Namespace{{Name: "itest7", Privileges: []string{"SELECT"}}},
	}

	first, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := first.Apply(ctx); err != nil {
		t.Fatalf("applying: %v", err)
	}

	// The baseline is the plan once the role exists; the first carries CREATE
	// ROLE.
	settled, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("re-planning after the role exists: %v", err)
	}

	hasUsage := func() bool {
		t.Helper()
		var ok bool
		if err := c.c.QueryRow(ctx,
			`SELECT has_schema_privilege('itest7_reader', 'itest7', 'USAGE')`).Scan(&ok); err != nil {
			t.Fatalf("checking privilege: %v", err)
		}
		return ok
	}

	if !hasUsage() {
		t.Fatal("USAGE was not granted by the first apply")
	}

	// Drift: someone revokes by hand.
	if err := c.Exec(ctx, `REVOKE USAGE ON SCHEMA itest7 FROM itest7_reader`); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if hasUsage() {
		t.Fatal("the revoke did not take effect")
	}

	second, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("re-planning: %v", err)
	}

	if settled.Len() != 0 {
		t.Errorf("settled plan has %d statement(s), want 0:\n%s", settled.Len(), settled.Describe())
	}
	want := `GRANT USAGE ON SCHEMA "itest7" TO "itest7_reader";`
	if second.Len() != 1 || second.Steps()[0].Describe() != want {
		t.Errorf("plan after the revoke:\n%s\nwant exactly: %s", second.Describe(), want)
	}

	if _, err := second.Apply(ctx); err != nil {
		t.Fatalf("re-applying: %v", err)
	}
	if !hasUsage() {
		t.Error("re-applying did not restore the revoked USAGE")
	}
}

// connectAsProvisioner returns a connection as a non-superuser CREATEROLE
// admin, like RDS's. A superuser holds every membership implicitly, so it
// cannot see membership failures.
func connectAsProvisioner(t *testing.T, su pgxConn) pgxConn {
	t.Helper()
	ctx := context.Background()
	const admin, password = "itest_provisioner", "itest"
	var exists bool
	if err := su.c.QueryRow(ctx, QueryRoleExists, admin).Scan(&exists); err != nil {
		t.Fatalf("looking up the provisioner role: %v", err)
	}
	if !exists {
		if err := su.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN CREATEROLE PASSWORD %s`,
			admin, quoteLiteral(password))); err != nil {
			t.Fatalf("creating the provisioner role: %v", err)
		}
	}
	// WITH GRANT OPTION stands in for how the real db_provisioner reaches the
	// database: through membership in the role that owns it. Without it the
	// provisioner's own GRANT ... ON DATABASE to a service role is accepted
	// as "no privileges were granted" and nothing is granted.
	if err := su.Exec(ctx, fmt.Sprintf(`GRANT CONNECT, CREATE ON DATABASE %s TO %s WITH GRANT OPTION`,
		QuoteIdent(su.c.Config().Database), admin)); err != nil {
		t.Fatalf("granting the provisioner its database privileges: %v", err)
	}

	cfg := su.c.Config().Copy()
	cfg.User, cfg.Password = admin, password
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting as %s: %v", admin, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return pgxConn{c}
}

// dropProvisionedRole drops a role the provisioner granted database
// privileges to. The grantor revokes first: DROP OWNED BY as the superuser
// leaves the provisioner's database grant, which blocks DROP ROLE.
func dropProvisionedRole(t *testing.T, su pgxConn, role string) {
	t.Helper()
	ctx := context.Background()
	var exists bool
	if err := su.c.QueryRow(ctx, QueryRoleExists, role).Scan(&exists); err != nil {
		t.Fatalf("looking up %s: %v", role, err)
	}
	if exists {
		// Only the grantor may revoke its own grant, so the superuser
		// briefly becomes it.
		for _, stmt := range []string{
			`SET ROLE itest_provisioner`,
			fmt.Sprintf(`REVOKE ALL ON DATABASE %s FROM %s`, QuoteIdent(su.c.Config().Database), QuoteIdent(role)),
			`RESET ROLE`,
		} {
			if err := su.Exec(ctx, stmt); err != nil {
				_ = su.Exec(ctx, `RESET ROLE`)
				t.Fatalf("revoking the provisioner's database grant to %s (%q): %v", role, stmt, err)
			}
		}
	}
	dropRole(t, su, role)
}

// A brand-new ownerOf role provisioned by a non-superuser admin needs the
// admin's self-grant before CREATE SCHEMA ... AUTHORIZATION.
func TestGreenfieldOwnerOfAsANonSuperuserAdmin(t *testing.T) {
	ctx := context.Background()
	su := connect(t)
	const role, schema = "itest_greenfield_app", "itest_greenfield"
	if err := su.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema)); err != nil {
		t.Fatal(err)
	}
	connectAsProvisioner(t, su) // the grantor dropProvisionedRole revokes by must exist
	dropProvisionedRole(t, su, role)
	t.Cleanup(func() {
		_ = su.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		dropProvisionedRole(t, su, role)
	})

	prov := connectAsProvisioner(t, su)
	e := New(prov, prov, nil)
	access := engine.Access{
		Database:  su.c.Config().Database,
		Principal: role,
		Namespaces: []engine.Namespace{{
			Name:       schema,
			Privileges: []string{"USAGE", "CREATE", "SELECT", "INSERT", "UPDATE", "DELETE"},
			Owner:      true,
		}},
	}

	p, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := p.Apply(ctx); err != nil {
		t.Fatalf("applying as a non-superuser admin: %v\n\nplan:\n%s", err, p.Describe())
	}

	var owner string
	if err := su.c.QueryRow(ctx,
		`SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = $1`, schema).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != role {
		t.Errorf("schema %s is owned by %q, want %q", schema, owner, role)
	}

	again, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("second BuildPlan: %v", err)
	}
	if again.Len() != 0 {
		t.Errorf("second plan has %d statement(s), want 0:\n%s", again.Len(), again.Describe())
	}
}

// A role the admin holds no ADMIN OPTION on fails planning with the one-time
// grant an administrator has to run.
func TestOwnerOfOnAForeignRoleNamesTheRunbookGrant(t *testing.T) {
	ctx := context.Background()
	su := connect(t)
	const role, schema = "itest_foreign_app", "itest_foreign"
	if err := su.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema)); err != nil {
		t.Fatal(err)
	}
	dropRole(t, su, role)
	t.Cleanup(func() {
		_ = su.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		dropRole(t, su, role)
	})
	for _, s := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN`, role), // created by the superuser, not the provisioner
		fmt.Sprintf(`CREATE SCHEMA %s`, schema),
		fmt.Sprintf(`CREATE TABLE %s.t (id int)`, schema),
	} {
		if err := su.Exec(ctx, s); err != nil {
			t.Fatalf("seeding %q: %v", s, err)
		}
	}

	prov := connectAsProvisioner(t, su)
	_, err := New(prov, prov, nil).BuildPlan(ctx, engine.Access{
		Database:   su.c.Config().Database,
		Principal:  role,
		Namespaces: []engine.Namespace{{Name: schema, Privileges: []string{"SELECT"}, Owner: true}},
	})
	if err == nil {
		t.Fatal("BuildPlan succeeded; want an error naming the one-time grant")
	}
	want := fmt.Sprintf(`GRANT %q TO "itest_provisioner" WITH ADMIN TRUE, INHERIT TRUE, SET TRUE`, role)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error does not name the fix\n got: %v\nwant substring: %s", err, want)
	}
}

// ownerSelfSetup converges an ownerOf role on its own schema and returns the
// engine, the access and a helper that runs SQL as a given role.
func ownerSelfSetup(t *testing.T, role, other, schema string) (*Engine, engine.Access, func(as, sql string)) {
	t.Helper()
	ctx := context.Background()
	su := connect(t)
	cleanup := func() {
		_ = su.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		dropRole(t, su, role)
		dropRole(t, su, other)
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, s := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN`, role),
		fmt.Sprintf(`CREATE ROLE %s LOGIN`, other),
		fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, schema, role),
	} {
		if err := su.Exec(ctx, s); err != nil {
			t.Fatalf("seeding %q: %v", s, err)
		}
	}
	as := func(r, sql string) {
		t.Helper()
		for _, s := range []string{`SET ROLE ` + r, sql, `RESET ROLE`} {
			if err := su.Exec(ctx, s); err != nil {
				_ = su.Exec(ctx, `RESET ROLE`)
				t.Fatalf("as %s, %q: %v", r, s, err)
			}
		}
	}
	as(role, fmt.Sprintf(`CREATE TABLE %s.before_converge (id bigserial PRIMARY KEY)`, schema))

	e := New(su, su, nil)
	access := engine.Access{
		Database:  su.c.Config().Database,
		Principal: role,
		Namespaces: []engine.Namespace{{
			Name: schema, Privileges: []string{"USAGE", "CREATE", "SELECT", "INSERT", "UPDATE", "DELETE"}, Owner: true,
		}},
	}
	for pass := 1; pass <= 2; pass++ {
		p, err := e.BuildPlan(ctx, access)
		if err != nil {
			t.Fatalf("pass %d BuildPlan: %v", pass, err)
		}
		if _, err := p.Apply(ctx); err != nil {
			t.Fatalf("pass %d apply: %v", pass, err)
		}
	}
	return e, access, as
}

// A table the owner creates after convergence has a NULL ACL; the diff must
// not count it as lacking, or the plan never converges.
func TestTablesTheOwnerCreatesLaterDoNotReopenThePlan(t *testing.T) {
	ctx := context.Background()
	e, access, as := ownerSelfSetup(t, "itest_selfown_app", "itest_selfown_other", "itest_selfown")
	as("itest_selfown_app", `CREATE TABLE itest_selfown.after_converge (id bigserial PRIMARY KEY)`)

	p, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Len() != 0 {
		t.Errorf("plan reopened after the owner created a table, want 0 statements:\n%s", p.Describe())
	}
}

// Once ownership moves away, the old owner's rights must be granted back.
func TestOwnershipMovingAwayBringsTheGrantBack(t *testing.T) {
	ctx := context.Background()
	e, access, as := ownerSelfSetup(t, "itest_moved_app", "itest_moved_other", "itest_moved")
	as("itest_moved_app", `CREATE TABLE itest_moved.moved (id int)`)
	as("postgres", `ALTER TABLE itest_moved.moved OWNER TO itest_moved_other`) // an administrator moves it

	p, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := `ON ALL TABLES IN SCHEMA "itest_moved" TO "itest_moved_app"`
	if !strings.Contains(p.Describe(), want) {
		t.Fatalf("plan does not re-grant the table that moved away; want %q in:\n%s", want, p.Describe())
	}
	if _, err := p.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// ownerOf reassigns the table back AND the grant now exists: settled.
	again, err := e.BuildPlan(ctx, access)
	if err != nil {
		t.Fatalf("second BuildPlan: %v", err)
	}
	if again.Len() != 0 {
		t.Errorf("did not settle after the re-grant, want 0 statements:\n%s", again.Describe())
	}
}

// A role that does not exist yet must still be granted on tables that do
// (its oid is NULL; hence IS DISTINCT FROM).
func TestANewRoleIsGrantedOnExistingTables(t *testing.T) {
	ctx := context.Background()
	su := connect(t)
	const role, schema = "itest_newreader", "itest_newreader_src"
	cleanup := func() {
		_ = su.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		dropRole(t, su, role)
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, s := range []string{
		fmt.Sprintf(`CREATE SCHEMA %s`, schema),
		fmt.Sprintf(`CREATE TABLE %s.events (id int)`, schema),
	} {
		if err := su.Exec(ctx, s); err != nil {
			t.Fatalf("seeding %q: %v", s, err)
		}
	}

	p, err := New(su, su, nil).BuildPlan(ctx, engine.Access{
		Database:   su.c.Config().Database,
		Principal:  role,
		Namespaces: []engine.Namespace{{Name: schema, Privileges: []string{"SELECT"}}},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := fmt.Sprintf(`GRANT SELECT ON ALL TABLES IN SCHEMA %q TO %q;`, schema, role)
	if !strings.Contains(p.Describe(), want) {
		t.Errorf("plan for a not-yet-created role is missing %q:\n%s", want, p.Describe())
	}
}

// Hyphenated role names (#27) are legal only quoted. Every statement and every
// catalog lookup must therefore carry the name as itself: a path that dropped
// the quotes would fail to parse, and one that compared a folded or quoted
// form against the catalog would never converge. The app role owns a table,
// so the default-privileges path sees a hyphenated owner too.
func TestHyphenatedNamesApplyAndConverge(t *testing.T) {
	ctx := context.Background()
	c := connect(t)
	seed(t, c, "itest_hyphen", "itest-hyphen-app")
	t.Cleanup(func() {
		_ = c.Exec(ctx, `DROP SCHEMA IF EXISTS "itest_hyphen" CASCADE`)
		dropRole(t, c, "itest-hyphen-reader")
		dropRole(t, c, "itest-hyphen-app")
	})

	e := New(c, c, nil)
	for _, access := range []engine.Access{
		{
			Database:   "postgres",
			Principal:  "itest-hyphen-reader",
			Namespaces: []engine.Namespace{{Name: "itest_hyphen", Privileges: []string{"SELECT", "UPDATE"}}},
		},
		{
			Database:   "postgres",
			Principal:  "itest-hyphen-app",
			Namespaces: []engine.Namespace{{Name: "itest_hyphen", Privileges: []string{"SELECT", "INSERT"}, Owner: true}},
		},
	} {
		p, err := e.BuildPlan(ctx, access)
		if err != nil {
			t.Fatalf("%s: BuildPlan: %v", access.Principal, err)
		}
		if _, err := p.Apply(ctx); err != nil {
			t.Fatalf("%s: applying: %v\n\nplan:\n%s", access.Principal, err, p.Describe())
		}
		again, err := e.BuildPlan(ctx, access)
		if err != nil {
			t.Fatalf("%s: re-planning: %v", access.Principal, err)
		}
		if again.Len() != 0 {
			t.Errorf("%s: plan did not converge, want 0 statements:\n%s", access.Principal, again.Describe())
		}
	}

	var can bool
	if err := c.c.QueryRow(ctx,
		`SELECT has_table_privilege('itest-hyphen-reader', '"itest_hyphen".fixtures', 'SELECT')`).Scan(&can); err != nil {
		t.Fatalf("checking privilege: %v", err)
	}
	if !can {
		t.Error(`itest-hyphen-reader cannot SELECT from "itest_hyphen".fixtures after the plan applied`)
	}
}

// connectTo opens a second connection like connect's, to another database on
// the same server.
func connectTo(t *testing.T, database string) pgxConn {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("PGTEST_DSN"))
	if err != nil {
		t.Fatalf("parsing PGTEST_DSN: %v", err)
	}
	cfg.Database = database
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting to database %s: %v", database, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return pgxConn{c}
}

// One role shared by resources on different databases (#27): roles are
// server-wide, grants are per database, so revoking one database's access
// must leave the other's intact. CREATE and a table grant are checked, not
// CONNECT, which PUBLIC holds on a new database and would pass regardless.
func TestOneRoleAcrossDatabasesRevokesIndependently(t *testing.T) {
	ctx := context.Background()
	admin := connect(t)
	const role = "itest-svc-xyz"
	dbs := []string{"itest-svc-xyz-dev", "itest-svc-xyz-qa"}

	dropRole(t, admin, role)
	for _, db := range dbs {
		if err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+QuoteIdent(db)+` WITH (FORCE)`); err != nil {
			t.Fatalf("dropping database %s: %v", db, err)
		}
		if err := admin.Exec(ctx, `CREATE DATABASE `+QuoteIdent(db)); err != nil {
			t.Fatalf("creating database %s: %v", db, err)
		}
	}
	t.Cleanup(func() {
		for _, db := range dbs {
			_ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+QuoteIdent(db)+` WITH (FORCE)`)
		}
		dropRole(t, admin, role)
	})

	conns := map[string]pgxConn{}
	accesses := map[string]engine.Access{}
	for _, db := range dbs {
		c := connectTo(t, db)
		if err := c.Exec(ctx, `CREATE TABLE public.orders (id bigint PRIMARY KEY)`); err != nil {
			t.Fatalf("seeding %s: %v", db, err)
		}
		conns[db] = c
		accesses[db] = engine.Access{
			Database:   db,
			Principal:  role,
			Namespaces: []engine.Namespace{{Name: "public", Privileges: []string{"SELECT", "INSERT"}}},
		}
		p, err := New(c, c, nil).BuildPlan(ctx, accesses[db])
		if err != nil {
			t.Fatalf("%s: BuildPlan: %v", db, err)
		}
		if _, err := p.Apply(ctx); err != nil {
			t.Fatalf("%s: applying: %v\n\nplan:\n%s", db, err, p.Describe())
		}
	}

	granted := func(db string) (create, selectOrders bool) {
		t.Helper()
		err := conns[db].c.QueryRow(ctx,
			`SELECT has_database_privilege($1, current_database(), 'CREATE'),
			        has_table_privilege($1, 'public.orders', 'SELECT')`, role).Scan(&create, &selectOrders)
		if err != nil {
			t.Fatalf("%s: reading privileges: %v", db, err)
		}
		return create, selectOrders
	}
	for _, db := range dbs {
		if c, s := granted(db); !c || !s {
			t.Fatalf("%s before revoke: CREATE=%t SELECT=%t, want both", db, c, s)
		}
	}

	revoke, err := New(conns[dbs[0]], conns[dbs[0]], nil).BuildRevokePlan(ctx, accesses[dbs[0]])
	if err != nil {
		t.Fatalf("BuildRevokePlan: %v", err)
	}
	if _, err := revoke.Apply(ctx); err != nil {
		t.Fatalf("revoking %s: %v", dbs[0], err)
	}

	if c, s := granted(dbs[0]); c || s {
		t.Errorf("%s after its revoke: CREATE=%t SELECT=%t, want neither", dbs[0], c, s)
	}
	if c, s := granted(dbs[1]); !c || !s {
		t.Errorf("%s after revoking %s: CREATE=%t SELECT=%t, want both kept", dbs[1], dbs[0], c, s)
	}
	var exists bool
	if err := admin.c.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1)`, role).
		Scan(&exists); err != nil {
		t.Fatalf("checking role: %v", err)
	}
	if !exists {
		t.Errorf("role %s was dropped by one database's revoke", role)
	}
}
