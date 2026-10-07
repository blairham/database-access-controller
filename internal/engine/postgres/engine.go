// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"

	"github.com/blairham/k8s-controller-kit/plan"

	"github.com/blairham/database-controller/internal/engine"
)

// Engine provisions roles, schemas and grants on PostgreSQL, including RDS and
// Aurora.
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
	if err := ValidateHyphenatedIdent("role", a.Principal); err != nil {
		return err
	}
	if err := ValidateHyphenatedIdent("database", a.Database); err != nil {
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
// Access describes. The plan is a diff: a statement appears only when the
// database lacks what it would grant, so a converged database plans nothing.
func (e *Engine) BuildPlan(ctx context.Context, a engine.Access) (*plan.Plan, error) {
	if err := e.Validate(a); err != nil {
		return nil, err
	}

	role := QuoteIdent(a.Principal)
	p := &plan.Plan{}

	// Checked first so re-runs don't log `role already exists` on the server;
	// the tolerated error covers a race with a concurrent create.
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
		// rds_iam exists only on RDS and Aurora. Checking while planning gives
		// a clear error instead of a half-applied plan.
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

	// CREATE is needed even for a no-op CREATE SCHEMA IF NOT EXISTS: the
	// privilege is checked before existence.
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

	// Namespaces are planned first because the admin needs membership in the
	// principal only if ownerOf work is actually planned.
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
// On PostgreSQL 16 a CREATEROLE admin gets only ADMIN OPTION on the roles it
// creates (createrole_self_grant is empty by default), so it cannot SET ROLE
// for CREATE SCHEMA ... AUTHORIZATION or inherit the privileges needed to grant
// on the schema the role owns. ADMIN OPTION is enough to grant itself both. A
// role it holds no ADMIN OPTION on fails at plan time with the statement an
// administrator must run.
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
		// Not IF NOT EXISTS: that still checks SET ROLE before existence.
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
// owns objects in the schema. FOR ROLE is required: without it the default
// binds to the provisioner, which creates nothing.
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

	seen := make(map[string]bool, len(owners))
	for _, owner := range owners {
		if seen[owner] {
			continue
		}
		seen[owner] = true
		// A default an owner grants to itself has no effect.
		if owner == principal {
			continue
		}
		if err := ValidateHyphenatedIdent("owner role", owner); err != nil {
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
			// The admin may not hold every owner's privileges; one unreachable
			// owner must not fail the plan.
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

// BuildRevokePlan withdraws the grants. It never drops the role: a role that
// owns objects cannot be dropped.
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
