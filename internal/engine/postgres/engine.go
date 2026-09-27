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
func (e *Engine) BuildPlan(ctx context.Context, a engine.Access) (*plan.Plan, error) {
	if err := e.Validate(a); err != nil {
		return nil, err
	}

	role := QuoteIdent(a.Principal)
	p := &plan.Plan{}

	// PostgreSQL has no CREATE ROLE IF NOT EXISTS. The tolerated error is
	// narrow and explicit rather than a blanket stderr filter.
	p.Add(&Statement{
		SQL:    fmt.Sprintf("CREATE ROLE %s WITH LOGIN", role),
		Why:    "per-service login role; PostgreSQL has no CREATE ROLE IF NOT EXISTS",
		Ignore: []string{"already exists"},
		exec:   e.exec,
	})

	if a.IAMAuth {
		p.Add(&Statement{
			SQL:  fmt.Sprintf("GRANT rds_iam TO %s", role),
			Why:  "authenticate with an IAM auth token instead of a stored password",
			exec: e.exec,
		})
	}

	// CONNECT opens a session. CREATE is needed for CREATE SCHEMA IF NOT
	// EXISTS: PostgreSQL checks the privilege before it checks existence, so
	// even a no-op call from a service's own migrator requires it.
	p.Add(&Statement{
		SQL:  fmt.Sprintf("GRANT CONNECT, CREATE ON DATABASE %s TO %s", QuoteIdent(a.Database), role),
		Why:  "CONNECT opens a session; CREATE is required even by a no-op CREATE SCHEMA IF NOT EXISTS",
		exec: e.exec,
	})

	for _, ns := range a.Namespaces {
		if err := e.addNamespace(ctx, p, a.Principal, ns); err != nil {
			return nil, err
		}
	}

	return p, nil
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

	p.Add(&Statement{
		SQL:  fmt.Sprintf("GRANT %s ON SCHEMA %s TO %s", Join(privs.Schema), schema, role),
		Why:  "USAGE resolves names in the schema; without it every table grant below is unusable",
		exec: e.exec,
	})

	if len(privs.Table) > 0 {
		p.Add(&Statement{
			SQL:  fmt.Sprintf("GRANT %s ON ALL TABLES IN SCHEMA %s TO %s", Join(privs.Table), schema, role),
			Why:  "covers tables that exist now; the default privileges below cover tables created later",
			exec: e.exec,
		})
		if err := e.addDefaultPrivileges(ctx, p, principal, ns.Name, "TABLES", privs.Table, ns.Owner); err != nil {
			return err
		}
	}

	if len(privs.Sequence) > 0 {
		p.Add(&Statement{
			SQL:  fmt.Sprintf("GRANT %s ON ALL SEQUENCES IN SCHEMA %s TO %s", Join(privs.Sequence), schema, role),
			Why:  "sequence privileges are the requested set intersected with SELECT/UPDATE/USAGE, the only ones a sequence accepts",
			exec: e.exec,
		})
		if err := e.addDefaultPrivileges(ctx, p, principal, ns.Name, "SEQUENCES", privs.Sequence, ns.Owner); err != nil {
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
func (e *Engine) addDefaultPrivileges(ctx context.Context, p *plan.Plan, principal, schema, objType string, privs []string, principalWillOwn bool) error {
	owners, err := e.inspect.ObjectOwners(ctx, schema)
	if err != nil {
		return fmt.Errorf("enumerating owners of schema %q: %w", schema, err)
	}

	// The principal is added when it is going to own the schema, because the
	// enumeration above reflects the database as it is BEFORE this plan runs.
	//
	// Two cases need it, and both are invisible to a query made first. A
	// greenfield schema does not exist yet, so ObjectOwners returns nothing at
	// all and no default is registered for the role about to create every
	// object in it. And on an existing schema the reassignment further down
	// this plan makes the principal the owner of relations the enumeration
	// attributed to someone else.
	//
	// Without this the first run registers nothing and only a later reconcile
	// converges -- correct eventually, but the implementation this replaces
	// enumerated owners server-side at execution time and got it in one pass.
	if principalWillOwn {
		owners = append(owners, principal)
	}

	seen := make(map[string]bool, len(owners))
	for _, owner := range owners {
		if seen[owner] {
			continue
		}
		seen[owner] = true
		if err := ValidateIdent("owner role", owner); err != nil {
			return fmt.Errorf("schema %q: %w", schema, err)
		}
		p.Add(&Statement{
			SQL: fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT %s ON %s TO %s",
				QuoteIdent(owner), QuoteIdent(schema), Join(privs), objType, QuoteIdent(principal)),
			Why: fmt.Sprintf("future %s created by %s stay reachable; FOR ROLE is required or the default binds to the provisioner, which creates nothing",
				objType, owner),
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
