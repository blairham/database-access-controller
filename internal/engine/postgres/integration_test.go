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

type pgxConn struct{ c *pgx.Conn }

func (p pgxConn) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.c.Exec(ctx, sql, args...)
	return err
}

func (p pgxConn) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var b bool
	err := p.c.QueryRow(ctx, QuerySchemaExists, schema).Scan(&b)
	return b, err
}

func (p pgxConn) ObjectOwners(ctx context.Context, schema string) ([]string, error) {
	rows, err := p.c.Query(ctx, QueryObjectOwners, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p pgxConn) RelationsNotOwnedBy(ctx context.Context, schema, owner string) ([]Relation, error) {
	rows, err := p.c.Query(ctx, QueryRelationsNotOwnedBy, schema, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Relation
	for rows.Next() {
		var r Relation
		if err := rows.Scan(&r.Name, &r.Kind, &r.Owner); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

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
		fmt.Sprintf(`CREATE VIEW %s.v_fixture_metadata AS SELECT id, name FROM %s.fixtures`, QuoteIdent(schema), QuoteIdent(schema)),
		fmt.Sprintf(`CREATE MATERIALIZED VIEW %s.mv_rollup AS SELECT count(*) AS n FROM %s.fixtures`, QuoteIdent(schema), QuoteIdent(schema)),
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
