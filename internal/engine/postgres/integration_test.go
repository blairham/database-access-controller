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

	"github.com/blairham/database-controller/internal/engine"
)

// These tests run the generated statements against a real PostgreSQL server.
// Unit tests prove the SQL reads correctly; only a server proves it parses,
// that the privilege split is one PostgreSQL accepts, and that the plan is
// genuinely re-runnable.
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

// seed builds a schema that reproduces the shape the real databases are in:
// an app role that owns the relations, a schema owned by someone else, and a
// view among the tables.
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

// dropRole removes a role and everything that depends on it.
//
// DROP ROLE alone is not enough and the reason is the same one that makes
// spec.revokeOnDelete default to false: ALTER DEFAULT PRIVILEGES FOR ROLE x
// leaves a pg_default_acl entry that counts as a dependency, so the role cannot
// be dropped even after its schema is gone. DROP OWNED BY clears those entries
// along with any remaining grants.
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

// Re-running must be a no-op rather than an error. The Job this replaces was
// re-run by Argo on every drift, so every statement had to tolerate existing
// state; a controller re-runs far more often than that.
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

// Ownership reassignment must cover views and materialized views. Driving it
// off pg_tables and pg_sequences misses both, and replacing a view requires
// ownership of it.
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

// The privilege split must produce a sequence grant PostgreSQL accepts.
// Passing INSERT through produces "invalid privilege type INSERT for sequence"
// and aborts the whole transaction.
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

// A serial or identity sequence is auto-dependent on its table's column, and
// PostgreSQL refuses to change its owner independently:
//
//	ERROR: cannot change owner of sequence "events_id_seq" (SQLSTATE 0A000)
//
// Its owner follows the table's, so reassigning the table is both necessary
// and sufficient; emitting the ALTER at all is an error rather than merely
// redundant. This was a live bug, found by diffing against the Job this engine
// replaces -- that implementation never hit it because it reassigned tables
// first and its sequence loop then found nothing left to do.
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

// Tables must be planned before sequences, so a standalone sequence is never
// attempted ahead of the table whose reassignment would have settled it.
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

// Re-applying must REPAIR a revoked grant, and this is why the plan is applied
// on every reconcile rather than skipped when nothing appears to have changed.
//
// ⚠ DO NOT "OPTIMIZE" THIS BY COMPARING status.appliedPlanHash AND SKIPPING.
// The plan is built from what the engine reads -- role existence, schema
// existence, object owners -- and it does not read current grants. A revoked
// privilege therefore produces a BYTE-IDENTICAL plan with an identical hash.
// Measured on a rig: revoking USAGE left the hash at 79faccfc19bb167b, so a
// hash-based skip would have silently stopped repairing the most common drift
// there is.
//
// Re-issuing grants is cheap and GRANT is an upsert. Skipping is not.
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

	// The BASELINE is the plan once the role exists, not the first plan. The
	// first one carries CREATE ROLE and the next one does not, so comparing
	// against it would show a hash change that has nothing to do with the
	// revoke under test.
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

	// Drift, the way it actually happens: someone revokes by hand.
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

	// The plan is a diff, so drift is now VISIBLE in it: a settled database
	// plans nothing, and the revoke brings back exactly the one statement that
	// repairs it. This used to assert the opposite -- that the hash did NOT
	// change, because the plan never read grants and a revoke was invisible to
	// it. That is why re-applying on every reconcile was mandatory; it is still
	// what the controller does, and the repair below still pins it.
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
