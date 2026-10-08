// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build equivalence

package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/blairham/database-access-controller/internal/engine"
)

// This test checks that the engine leaves a database in the same state as an
// existing provisioner script: object ownership, relation and schema ACLs, and
// default privileges, from the same seed.
//
// Render the script from whatever Helm chart or manifest runs it today, point
// JOB_SCRIPT at it, and set EQ_SCHEMA / EQ_ROLE to the schema and role it
// targets. The script must name the same database as PGTEST_DSN.
//
//	docker run --rm -d -p 5433:5432 -e POSTGRES_PASSWORD=test --name pgtest postgres:16-alpine
//	JOB_SCRIPT=/path/to/job-script.sh \
//	EQ_SCHEMA=app EQ_ROLE=app_role \
//	PGTEST_DSN='postgres://postgres:test@127.0.0.1:5433/postgres' \
//	PGTEST_CONTAINER=pgtest \
//	  go test -tags equivalence ./internal/engine/postgres/ -run Equivalence -v

// The schema and role the comparison runs against; they must match the script.
var (
	eqSchema = envOr("EQ_SCHEMA", "app")
	eqRole   = envOr("EQ_ROLE", "app_role")

	// eqOwner is the legacy role that owns the seeded objects, standing in for
	// the common case where a schema was created by an admin and populated by
	// something else.
	eqOwner = envOr("EQ_LEGACY_OWNER", "legacy_owner")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func eqConn(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("PGTEST_DSN")
	if dsn == "" {
		t.Skip("PGTEST_DSN is unset")
	}
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func mustExec(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "..."
	}
	return s
}

// eqSeed builds a schema owned by someone other than the target role, tables
// and a sequence owned by a legacy role, and a view plus a materialized view.
func eqSeed(t *testing.T, c *pgx.Conn) {
	t.Helper()
	for _, s := range []string{
		fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, QuoteIdent(eqSchema)),
		fmt.Sprintf(`DO $$ BEGIN
			IF EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				EXECUTE 'DROP OWNED BY %s CASCADE'; EXECUTE 'DROP ROLE %s';
			END IF; END $$`, eqRole, QuoteIdent(eqRole), QuoteIdent(eqRole)),
		fmt.Sprintf(`DO $$ BEGIN
			IF EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				EXECUTE 'DROP OWNED BY %s CASCADE'; EXECUTE 'DROP ROLE %s';
			END IF; END $$`, eqOwner, QuoteIdent(eqOwner), QuoteIdent(eqOwner)),
		fmt.Sprintf(`CREATE ROLE %s WITH LOGIN`, QuoteIdent(eqOwner)),
		fmt.Sprintf(`CREATE SCHEMA %s`, QuoteIdent(eqSchema)),
		fmt.Sprintf(`CREATE TABLE %s.fixtures (id bigserial PRIMARY KEY, name text)`, QuoteIdent(eqSchema)),
		fmt.Sprintf(`CREATE TABLE %s.events (id bigserial PRIMARY KEY, payload jsonb)`, QuoteIdent(eqSchema)),
		fmt.Sprintf(`CREATE VIEW %s.v_metadata AS SELECT id, name FROM %s.fixtures`, QuoteIdent(eqSchema), QuoteIdent(eqSchema)),
		fmt.Sprintf(`CREATE MATERIALIZED VIEW %s.mv_rollup AS SELECT count(*) AS n FROM %s.fixtures`, QuoteIdent(eqSchema), QuoteIdent(eqSchema)),
		fmt.Sprintf(`ALTER TABLE %s.fixtures OWNER TO %s`, QuoteIdent(eqSchema), QuoteIdent(eqOwner)),
		fmt.Sprintf(`ALTER TABLE %s.events OWNER TO %s`, QuoteIdent(eqSchema), QuoteIdent(eqOwner)),
		fmt.Sprintf(`ALTER SEQUENCE %s.fixtures_id_seq OWNER TO %s`, QuoteIdent(eqSchema), QuoteIdent(eqOwner)),
	} {
		mustExec(t, c, s)
	}
}

// eqDump renders the state both implementations are supposed to produce.
// ACL arrays are unnested and sorted because their stored order is not
// meaningful and differs with the order grants were issued.
func eqDump(t *testing.T, c *pgx.Conn) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder

	b.WriteString("## role\n")
	rows, err := c.Query(ctx, `SELECT rolname, rolcanlogin FROM pg_roles WHERE rolname = $1`, eqRole)
	if err != nil {
		t.Fatalf("querying role: %v", err)
	}
	for rows.Next() {
		var name string
		var login bool
		if err := rows.Scan(&name, &login); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s login=%t\n", name, login)
	}
	rows.Close()

	b.WriteString("\n## relation owners\n")
	rows, err = c.Query(ctx, `
		SELECT c.relname, c.relkind::text, pg_get_userbyid(c.relowner)
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','S')
		 ORDER BY c.relname`, eqSchema)
	if err != nil {
		t.Fatalf("querying owners: %v", err)
	}
	for rows.Next() {
		var name, kind, owner string
		if err := rows.Scan(&name, &kind, &owner); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s [%s] owner=%s\n", name, kind, owner)
	}
	rows.Close()

	b.WriteString("\n## schema acl\n")
	rows, err = c.Query(ctx, `
		SELECT acl::text FROM pg_namespace n, unnest(COALESCE(n.nspacl, '{}')) acl
		 WHERE n.nspname = $1 ORDER BY 1`, eqSchema)
	if err != nil {
		t.Fatalf("querying schema acl: %v", err)
	}
	for rows.Next() {
		var acl string
		if err := rows.Scan(&acl); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s\n", acl)
	}
	rows.Close()

	b.WriteString("\n## relation acls\n")
	rows, err = c.Query(ctx, `
		SELECT c.relname, acl::text
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace,
		       unnest(COALESCE(c.relacl, '{}')) acl
		 WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','S')
		 ORDER BY c.relname, acl::text`, eqSchema)
	if err != nil {
		t.Fatalf("querying relation acls: %v", err)
	}
	for rows.Next() {
		var name, acl string
		if err := rows.Scan(&name, &acl); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s: %s\n", name, acl)
	}
	rows.Close()

	b.WriteString("\n## default privileges\n")
	rows, err = c.Query(ctx, `
		SELECT pg_get_userbyid(d.defaclrole), d.defaclobjtype::text, acl::text
		  FROM pg_default_acl d
		  JOIN pg_namespace n ON n.oid = d.defaclnamespace,
		       unnest(d.defaclacl) acl
		 WHERE n.nspname = $1
		 ORDER BY 1, 2, 3`, eqSchema)
	if err != nil {
		t.Fatalf("querying default acl: %v", err)
	}
	for rows.Next() {
		var owner, objType, acl string
		if err := rows.Scan(&owner, &objType, &acl); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "for=%s type=%s %s\n", owner, objType, acl)
	}
	rows.Close()

	return b.String()
}

// runJobScript executes the rendered script inside the PostgreSQL container,
// minus the AWS CLI install, the auth-token export and GRANT rds_iam, none of
// which can run outside RDS.
func runJobScript(t *testing.T, path string) {
	t.Helper()
	container := os.Getenv("PGTEST_CONTAINER")
	if container == "" {
		t.Skip("PGTEST_CONTAINER is unset")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading job script: %v", err)
	}

	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "apk add"),
			strings.HasPrefix(line, `export PGPASSWORD=`),
			strings.HasPrefix(strings.TrimSpace(line), "--hostname"),
			strings.HasPrefix(strings.TrimSpace(line), "--region"),
			strings.HasPrefix(line, "GRANT rds_iam"):
			continue
		}
		kept = append(kept, line)
	}
	script := strings.Join(kept, "\n")

	cmd := exec.Command("docker", "exec", "-i",
		"-e", "PGPASSWORD=test",
		"-e", "PGHOST=127.0.0.1",
		"-e", "PGUSER=postgres",
		"-e", "PGDATABASE=postgres",
		"-e", "TARGET_USER="+eqRole,
		container, "sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the Job script: %v\n%s", err, out)
	}
	t.Logf("Job script output:\n%s", out)
}

func TestEquivalenceWithTheProvisionerJob(t *testing.T) {
	cases := []struct {
		name      string
		scriptEnv string
		access    engine.Access
	}{
		{
			// An owning service: the shape a service that runs its own
			// migrations declares.
			name:      "owner with full DML",
			scriptEnv: "JOB_SCRIPT",
			access: engine.Access{
				Database:  "postgres",
				Principal: eqRole,
				Namespaces: []engine.Namespace{{
					Name:       eqSchema,
					Privileges: []string{"USAGE", "CREATE", "SELECT", "INSERT", "UPDATE", "DELETE"},
					Owner:      true,
				}},
			},
		},
		{
			// A read-only consumer of someone else's schema. No ownership, no
			// CREATE, and therefore no schema creation either.
			name:      "read-only, no ownership",
			scriptEnv: "JOB_SCRIPT_RO",
			access: engine.Access{
				Database:  "postgres",
				Principal: eqRole,
				Namespaces: []engine.Namespace{{
					Name:       eqSchema,
					Privileges: []string{"SELECT"},
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := os.Getenv(tc.scriptEnv)
			if script == "" {
				t.Skipf("%s is unset; render it from the service-template chart first", tc.scriptEnv)
			}

			ctx := context.Background()
			c := eqConn(t)

			// Half one: the script, run to convergence. Settled state is what
			// is compared, since a script may need more than one pass.
			eqSeed(t, c)
			runJobScript(t, script)
			runJobScript(t, script)
			jobState := eqDump(t, c)

			// Half two: the engine. IAMAuth is false to match the stripped
			// rds_iam grant.
			eqSeed(t, c)
			conn := pgxConn{c}
			e := New(conn, conn, nil)
			// Twice, so both sides are compared at rest.
			var applied, warnings int
			for pass := 1; pass <= 2; pass++ {
				p, err := e.BuildPlan(ctx, tc.access)
				if err != nil {
					t.Fatalf("pass %d: BuildPlan: %v", pass, err)
				}
				res, err := p.Apply(ctx)
				if err != nil {
					t.Fatalf("pass %d: applying engine plan: %v\n\nplan:\n%s", pass, err, p.Describe())
				}
				applied, warnings = res.Applied, len(res.Warnings)
			}
			t.Logf("engine applied %d statement(s), %d warning(s) on the settling pass", applied, warnings)
			engineState := eqDump(t, c)

			if jobState != engineState {
				t.Errorf("the engine and the Job left the database in different states\n\n"+
					"=== Job ===\n%s\n=== engine ===\n%s", jobState, engineState)
				return
			}
			t.Logf("states match:\n%s", engineState)
		})
	}
}
