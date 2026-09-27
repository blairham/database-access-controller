// Package engine defines what a database engine must implement to be
// provisionable by the DatabaseAccess controller.
//
// The split this interface draws is the important one in this repo. Creating an
// RDS instance, an Aurora cluster or a DynamoDB table is an AWS control-plane
// call, and Crossplane's upjet-generated provider-aws already does all of it --
// hand-writing that would be reimplementing generated code. What no AWS API can
// do is connect to the database and create a role, a schema and its grants:
// that is spoken in the engine's own protocol, and that is what lives here.
//
// DynamoDB is therefore deliberately absent. It has no data-plane principals;
// access to a table is an IAM policy, which provider-aws-iam already manages.
package engine

import (
	"context"

	"github.com/blairham/database-controller/internal/plan"
)

// Kind names a supported database engine.
type Kind string

const (
	// KindPostgres covers RDS PostgreSQL and Aurora PostgreSQL. They speak the
	// same wire protocol and take the same DDL, so one implementation serves
	// both; the distinction only matters to whoever created the instance.
	KindPostgres Kind = "postgres"

	// KindMySQL covers RDS MySQL, RDS MariaDB and Aurora MySQL.
	KindMySQL Kind = "mysql"

	// KindSQLServer covers RDS for SQL Server.
	KindSQLServer Kind = "sqlserver"
)

// Access is the engine-neutral description of what a service needs. It is the
// controller's spec, flattened so an engine never imports the API types.
type Access struct {
	// Database to connect to and grant connect-equivalent access on.
	Database string

	// Principal is the role, user or login to provision.
	Principal string

	// IAMAuth asks for the engine's IAM authentication path, where it has one:
	// GRANT rds_iam on PostgreSQL, the AWSAuthenticationPlugin on MySQL. SQL
	// Server has no equivalent and rejects it.
	IAMAuth bool

	// Namespaces are the schemas (PostgreSQL, SQL Server) or databases (MySQL)
	// the principal needs privileges in.
	Namespaces []Namespace
}

// Namespace is one schema or database and the privileges held on it.
type Namespace struct {
	Name       string
	Privileges []string

	// Owner makes the principal the owner of the namespace and everything in
	// it, so its migrations can ALTER and DROP what it created.
	Owner bool
}

// Engine builds a provisioning plan for one database engine.
//
// Building a plan is allowed to read from the database -- enumerating the
// current owners of a schema, say -- so that the plan holds concrete,
// reviewable statements rather than server-side DO blocks that hide what they
// will do until they have done it.
type Engine interface {
	// Kind identifies the engine.
	Kind() Kind

	// Validate rejects an Access this engine cannot honor, before any
	// connection is opened.
	Validate(a Access) error

	// BuildPlan returns the ordered steps that bring the database to the state
	// Access describes. The plan must be safe to re-run against a database
	// already in that state.
	BuildPlan(ctx context.Context, a Access) (*plan.Plan, error)

	// BuildRevokePlan returns the steps that withdraw the access again. It is
	// only used when the resource asks for it: dropping a principal that owns
	// objects fails, so teardown is opt-in.
	BuildRevokePlan(ctx context.Context, a Access) (*plan.Plan, error)

	// Close releases the engine's connection.
	Close() error
}
