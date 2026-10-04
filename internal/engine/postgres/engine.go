// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"

	"github.com/blairham/database-controller/internal/engine"
	"github.com/blairham/database-controller/internal/plan"
)

// Engine provisions roles, schemas and grants on RDS PostgreSQL and Aurora
// PostgreSQL. The two are one implementation: they speak the same wire protocol
// and take the same DDL.
type Engine struct {
	exec      Execer
	inspect   Inspector
	closeFunc func() error
}

var _ engine.Engine = (*Engine)(nil)

// New returns an Engine backed by the given connection.
func New(exec Execer, inspect Inspector, closeFunc func() error) *Engine {
	return &Engine{exec: exec, inspect: inspect, closeFunc: closeFunc}
}

// Kind identifies the engine.
func (e *Engine) Kind() engine.Kind { return engine.KindPostgres }

// Close releases the connection.
func (e *Engine) Close() error {
	if e.closeFunc == nil {
		return nil
	}
	return e.closeFunc()
}

// Validate rejects an Access this engine cannot honor.
func (e *Engine) Validate(a engine.Access) error {
	if err := ValidateIdent("role", a.Principal); err != nil {
		return err
	}
	if err := ValidateIdent("database", a.Database); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, ns := range a.Namespaces {
		if err := ValidateIdent("schema", ns.Name); err != nil {
			return err
		}
		if seen[ns.Name] {
			return fmt.Errorf("schema %q appears more than once", ns.Name)
		}
		seen[ns.Name] = true
		if _, err := SplitPrivileges(ns.Privileges); err != nil {
			return fmt.Errorf("schema %q: %w", ns.Name, err)
		}
	}
	return nil
}

// BuildPlan returns the ordered statements that bring the database to the state
// Access describes. Every statement is safe to re-run.
//
// The plan is a DIFF: a statement appears only when the database does not
// already hold what it would grant (see the ACL reads on Inspector). An empty
// plan therefore means "nothing to do", which is what Observe mode reports and
// what lets a converged database show zero pending statements.
func (e *Engine) BuildPlan(ctx context.Context, a engine.Access) (*plan.Plan, error) {
	if err := e.Validate(a); err != nil {
		return nil, err
	}

	role := QuoteIdent(a.Principal)
	p := &plan.Plan{}

	// PostgreSQL has no CREATE ROLE IF NOT EXISTS, so the role is looked up
	// first and the statement omitted when it is already there.
	//
	// Relying on the tolerated error instead works, but every re-run then
	// writes `ERROR: role "x" already exists` to the SERVER log -- on a
	// reconciling controller that is one line per resource per interval,
	// forever, in the log an operator greps during an incident. Checking first
	// keeps it quiet.
	//
	// The tolerance stays as a backstop: between this read and the write, a
	// concurrent reconcile or a human can create the role, and that race
	// should not fail the plan.
	exists, err := e.inspect.RoleExists(ctx, a.Principal)
	if err != nil {
		return nil, fmt.Errorf("checking role %q: %w", a.Principal, err)
	}
	if !exists {
		p.Add(&Statement{
			SQL:    fmt.Sprintf("CREATE ROLE %s WITH LOGIN", role),
			Why:    "per-service login role; PostgreSQL has no CREATE ROLE IF NOT EXISTS",
			Ignore: []string{"already exists"},
			exec:   e.exec,
		})
	}

	if a.IAMAuth {
		// Checked while planning, not discovered mid-apply.
		//
		// rds_iam is created by RDS and Aurora; a self-managed PostgreSQL has
		// no such role. Letting the GRANT find out means the plan aborts after
		// CREATE ROLE has already run, with `role "rds_iam" does not exist` --
		// true, but it does not say the likely cause, and the caller is left
		// with a half-applied plan to reason about.
		//
		// This is the failure the grantRdsIam/auth.method naming collision
		// invites: the two fields read as one setting, so grantRdsIam gets
		// left at its default against a database that cannot honor it.
		hasRdsIam, err := e.inspect.RoleExists(ctx, "rds_iam")
		if err != nil {
			return nil, fmt.Errorf("checking for the rds_iam role: %w", err)
		}
		if !hasRdsIam {
			return nil, fmt.Errorf(
				"grantRdsIam is set but this server has no rds_iam role, which exists only on "+
					"RDS and Aurora: set grantRdsIam=false if %q is a self-managed PostgreSQL. "+
					"Note this is separate from instance.auth.method, which governs how the "+
					"controller itself connects", a.Database,
			)
		}

		member, err := e.inspect.RoleIsMemberOf(ctx, a.Principal, "rds_iam")
		if err != nil {
			return nil, fmt.Errorf("checking rds_iam membership of %q: %w", a.Principal, err)
		}
		if !member {
			p.Add(&Statement{
				SQL:  fmt.Sprintf("GRANT rds_iam TO %s", role),
				Why:  "authenticate with an IAM auth token instead of a stored password",
				exec: e.exec,
			})
		}
	}

	// CONNECT opens a session. CREATE is needed for CREATE SCHEMA IF NOT
	// EXISTS: PostgreSQL checks the privilege before it checks existence, so
	// even a no-op call from a service's own migrator requires it.
	dbPrivs, err := e.inspect.DatabasePrivileges(ctx, a.Database, a.Principal)
	if err != nil {
		return nil, fmt.Errorf("reading privileges of %q on database %q: %w", a.Principal, a.Database, err)
	}
	if !hasAll(dbPrivs, []string{PrivConnect, PrivCreate}) {
		p.Add(&Statement{
			SQL:  fmt.Sprintf("GRANT CONNECT, CREATE ON DATABASE %s TO %s", QuoteIdent(a.Database), role),
			Why:  "CONNECT opens a session; CREATE is required even by a no-op CREATE SCHEMA IF NOT EXISTS",
			exec: e.exec,
		})
	}

	// Namespaces are planned into their own plans first, because whether the
	// admin needs a membership in the principal depends on what they plan:
	// ownerOf work is done AS the role, everything else is not. A converged
	// database plans nothing here, so it plans no membership either.
	var nsSteps []plan.Step
	ownedWork := false
	for _, ns := range a.Namespaces {
		sub := &plan.Plan{}
		if err := e.addNamespace(ctx, sub, a.Principal, ns); err != nil {
			return nil, err
		}
		if ns.Owner && sub.Len() > 0 {
			ownedWork = true
		}
		nsSteps = append(nsSteps, sub.Steps()...)
	}
	if ownedWork {
		if err := e.addAdminMembership(ctx, p, a.Principal, exists); err != nil {
			return nil, err
		}
	}
	p.Add(nsSteps...)

	return p, nil
}

// addAdminMembership makes sure the connecting admin can act as the principal
// before any ownerOf statement needs to.
//
// PostgreSQL 16 gives a CREATEROLE role that creates another role only an
// ADMIN OPTION membership in it; SET and INHERIT come only if the server's
// createrole_self_grant says so, and its default is empty. So the admin that
// just ran CREATE ROLE cannot run `CREATE SCHEMA ... AUTHORIZATION` for that
// role ("must be able to SET ROLE"), nor grant on the schema the role then
// owns ("permission denied for schema"). The first is exactly how the first
// greenfield ownerOf resource failed against RDS.
//
// ADMIN OPTION is enough to grant the missing membership to oneself, so the
// plan does that, once, before the namespace statements. The grant names
// INHERIT TRUE explicitly: SET alone was measured insufficient for the schema
// grant ownerOf plans.
//
// A role the admin holds no ADMIN OPTION on (one created by someone else, and
// never given the one-time grant) cannot be fixed from here. That is a hard
// failure at plan time, naming the statement an administrator must run,
// rather than a bare 42501 halfway through applying.
func (e *Engine) addAdminMembership(ctx context.Context, p *plan.Plan, principal string, exists bool) error {
	role := QuoteIdent(principal)
	grant := &Statement{
		SQL: fmt.Sprintf("GRANT %s TO CURRENT_USER WITH INHERIT TRUE, SET TRUE", role),
		Why: "ownerOf acts as this role: SET for CREATE SCHEMA ... AUTHORIZATION and OWNER TO, INHERIT for " +
			"granting on what it owns; a CREATEROLE creator holds only ADMIN OPTION by default",
		exec: e.exec,
	}

	// Created by this very plan: the admin is about to be its creator and
	// will hold ADMIN OPTION, and the role cannot be inspected yet.
	if !exists {
		p.Add(grant)
		return nil
	}

	acc, err := e.inspect.AdminAccessTo(ctx, principal)
	if err != nil {
		return fmt.Errorf("checking the admin's access to role %q: %w", principal, err)
	}
	if acc.Set && acc.Inherit {
		return nil
	}
	if !acc.Grant {
		return fmt.Errorf(
			"ownerOf needs %q to be able to act as role %q (it has SET=%t, INHERIT=%t) and %q holds no ADMIN "+
				"OPTION on it to grant itself that membership; an administrator must run once: "+
				"GRANT %s TO %s WITH ADMIN TRUE, INHERIT TRUE, SET TRUE",
			acc.Admin, principal, acc.Set, acc.Inherit, acc.Admin, role, QuoteIdent(acc.Admin),
		)
	}
	p.Add(grant)
	return nil
}

func (e *Engine) addNamespace(ctx context.Context, p *plan.Plan, principal string, ns engine.Namespace) error {
	role := QuoteIdent(principal)
	schema := QuoteIdent(ns.Name)

	privs, err := SplitPrivileges(ns.Privileges)
	if err != nil {
		return fmt.Errorf("schema %q: %w", ns.Name, err)
	}

	if ns.Owner {
		exists, err := e.inspect.SchemaExists(ctx, ns.Name)
		if err != nil {
			return fmt.Errorf("checking schema %q: %w", ns.Name, err)
		}
		// Only emit CREATE SCHEMA when it is genuinely missing. A bare CREATE
		// SCHEMA IF NOT EXISTS ... AUTHORIZATION checks the SET ROLE privilege
		// before the existence short-circuit, so on re-runs against a role the
		// controller did not create it fails with `must be able to SET ROLE`
		// even though there is nothing to do.
		if !exists {
			p.Add(&Statement{
				SQL:  fmt.Sprintf("CREATE SCHEMA %s AUTHORIZATION %s", schema, role),
				Why:  "greenfield owned schema: the GRANT below and the service's own migrator both need a target",
				exec: e.exec,
			})
		}
	}

	schemaPrivs, err := e.inspect.SchemaPrivileges(ctx, ns.Name, principal)
	if err != nil {
		return fmt.Errorf("reading privileges of %q on schema %q: %w", principal, ns.Name, err)
	}
	if !hasAll(schemaPrivs, privs.Schema) {
		p.Add(&Statement{
			SQL:  fmt.Sprintf("GRANT %s ON SCHEMA %s TO %s", Join(privs.Schema), schema, role),
			Why:  "USAGE resolves names in the schema; without it every table grant below is unusable",
			exec: e.exec,
		})
	}

	if len(privs.Table) > 0 {
		lacks, err := e.inspect.RelationsLackPrivileges(ctx, ns.Name, principal, tableGrantKinds, privs.Table)
		if err != nil {
			return fmt.Errorf("reading table privileges of %q in schema %q: %w", principal, ns.Name, err)
		}
		if lacks {
			p.Add(&Statement{
				SQL:  fmt.Sprintf("GRANT %s ON ALL TABLES IN SCHEMA %s TO %s", Join(privs.Table), schema, role),
				Why:  "covers tables that exist now; the default privileges below cover tables created later",
				exec: e.exec,
			})
		}
		if err := e.addDefaultPrivileges(ctx, p, principal, ns.Name, "TABLES", privs.Table); err != nil {
			return err
		}
	}

	if len(privs.Sequence) > 0 {
		lacks, err := e.inspect.RelationsLackPrivileges(ctx, ns.Name, principal, sequenceGrantKinds, privs.Sequence)
		if err != nil {
			return fmt.Errorf("reading sequence privileges of %q in schema %q: %w", principal, ns.Name, err)
		}
		if lacks {
			p.Add(&Statement{
				SQL:  fmt.Sprintf("GRANT %s ON ALL SEQUENCES IN SCHEMA %s TO %s", Join(privs.Sequence), schema, role),
				Why:  "sequence privileges are the requested set intersected with SELECT/UPDATE/USAGE, the only ones a sequence accepts",
				exec: e.exec,
			})
		}
		if err := e.addDefaultPrivileges(ctx, p, principal, ns.Name, "SEQUENCES", privs.Sequence); err != nil {
			return err
		}
	}

	if ns.Owner {
		if err := e.addOwnership(ctx, p, principal, ns.Name); err != nil {
			return err
		}
	}

	return nil
}

// addDefaultPrivileges registers ALTER DEFAULT PRIVILEGES once per role that
// actually creates objects in the schema.
//
// FOR ROLE is the whole point and is easy to omit. Without it PostgreSQL
// records the entry against the CURRENT role -- the provisioner -- so the
// default only ever applies to objects the provisioner itself created. It
// creates none: every schema is populated by its owning app role. The safety
// net then never fires anywhere, in any schema, and the only thing granting
// access is the one-shot GRANT ON ALL TABLES above, which does not survive the
// next migration that replaces an object.
func (e *Engine) addDefaultPrivileges(
	ctx context.Context,
	p *plan.Plan,
	principal, schema, objType string,
	privs []string,
) error {
	owners, err := e.inspect.ObjectOwners(ctx, schema)
	if err != nil {
		return fmt.Errorf("enumerating owners of schema %q: %w", schema, err)
	}

	// The principal is NOT added for a schema it is about to own. That used to
	// register `FOR ROLE principal ... TO principal` in one pass, but a default
	// an owner grants to itself has no effect (see below), so there is nothing
	// to register. Other owners in the schema are what defaults are for.

	seen := make(map[string]bool, len(owners))
	for _, owner := range owners {
		if seen[owner] {
			continue
		}
		seen[owner] = true
		// A default privilege an owner grants to itself does nothing: an owner
		// holds every privilege on what it creates, and PostgreSQL does not
		// even record the self-grant on new objects. Planning it only adds a
		// statement, and an INHERIT dependency, with no effect on access.
		if owner == principal {
			continue
		}
		if err := ValidateIdent("owner role", owner); err != nil {
			return fmt.Errorf("schema %q: %w", schema, err)
		}
		have, err := e.inspect.DefaultPrivileges(ctx, owner, schema, defaclObjType(objType), principal)
		if err != nil {
			return fmt.Errorf("reading default privileges for role %q in schema %q: %w", owner, schema, err)
		}
		if hasAll(have, privs) {
			continue
		}
		p.Add(&Statement{
			SQL: fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT %s ON %s TO %s",
				QuoteIdent(owner), QuoteIdent(schema), Join(privs), objType, QuoteIdent(principal)),
			Why: fmt.Sprintf(
				"future %s created by %s stay reachable; FOR ROLE is required or the default binds to the provisioner, which creates nothing",
				objType,
				owner,
			),
			// ALTER DEFAULT PRIVILEGES FOR ROLE x requires the admin role to
			// hold x's privileges, and it does not hold all of them. Fatal here
			// would take provisioning down for every service the moment one
			// unreachable owner appeared.
			BestEffort: true,
			exec:       e.exec,
		})
	}
	return nil
}

// addOwnership reassigns every relation in the schema that the principal does
// not already own, so its migrations can ALTER and DROP what it created.
func (e *Engine) addOwnership(ctx context.Context, p *plan.Plan, principal, schema string) error {
	rels, err := e.inspect.RelationsNotOwnedBy(ctx, schema, principal)
	if err != nil {
		return fmt.Errorf("enumerating relations in schema %q: %w", schema, err)
	}
	for _, r := range rels {
		if err := ValidateIdent("relation", r.Name); err != nil {
			return fmt.Errorf("schema %q: %w", schema, err)
		}
		p.Add(&Statement{
			SQL: fmt.Sprintf("%s %s.%s OWNER TO %s",
				r.AlterVerb(), QuoteIdent(schema), QuoteIdent(r.Name), QuoteIdent(principal)),
			Why:  fmt.Sprintf("owned by %s; replacing a view or altering a table requires ownership of it", r.Owner),
			exec: e.exec,
		})
	}
	return nil
}

// BuildRevokePlan withdraws the grants again.
//
// It revokes and does not DROP. A role that owns objects cannot be dropped, and
// reassigning its objects to someone else is a decision that needs a human
// looking at the data.
func (e *Engine) BuildRevokePlan(ctx context.Context, a engine.Access) (*plan.Plan, error) {
	if err := e.Validate(a); err != nil {
		return nil, err
	}
	role := QuoteIdent(a.Principal)
	p := &plan.Plan{}

	for _, ns := range a.Namespaces {
		schema := QuoteIdent(ns.Name)
		p.Add(&Statement{
			SQL:        fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA %s FROM %s", schema, role),
			Why:        "withdraw table access",
			BestEffort: true,
			exec:       e.exec,
		})
		p.Add(&Statement{
			SQL:        fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA %s FROM %s", schema, role),
			Why:        "withdraw sequence access",
			BestEffort: true,
			exec:       e.exec,
		})
		p.Add(&Statement{
			SQL:        fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM %s", schema, role),
			Why:        "withdraw schema access",
			BestEffort: true,
			exec:       e.exec,
		})
	}

	p.Add(&Statement{
		SQL:        fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM %s", QuoteIdent(a.Database), role),
		Why:        "withdraw CONNECT; the role itself is left in place because dropping an owning role fails",
		BestEffort: true,
		exec:       e.exec,
	})

	return p, nil
}

// hasAll reports whether every privilege in want appears in have.
func hasAll(have, want []string) bool {
	got := make(map[string]bool, len(have))
	for _, h := range have {
		got[h] = true
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

// defaclObjType maps the ALTER DEFAULT PRIVILEGES object keyword to the code
// pg_default_acl records it under.
func defaclObjType(objType string) string {
	if objType == "SEQUENCES" {
		return defaclSequences
	}
	return defaclTables
}
