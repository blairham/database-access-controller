// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Package engine defines what a database engine must implement to be
// provisionable by the DatabaseAccess controller: the data-plane work (roles,
// schemas, grants) that no AWS control-plane API can do.
package engine

import (
	"context"

	"github.com/blairham/k8s-controller-kit/plan"
)

// Kind names a supported database engine.
type Kind string

const (
	// KindPostgres covers RDS, Aurora and self-managed PostgreSQL.
	KindPostgres Kind = "postgres"

	// KindMySQL and KindSQLServer are planned and not yet implemented.
	KindMySQL     Kind = "mysql"
	KindSQLServer Kind = "sqlserver"
)

// Access is the engine-neutral description of what a service needs. It is the
// controller's spec, flattened so an engine never imports the API types.
type Access struct {
	// Database to connect to and grant connect-equivalent access on.
	Database string

	// Principal is the role, user or login to provision.
	Principal string

	// IAMAuth lets the principal authenticate with IAM (GRANT rds_iam on
	// PostgreSQL).
	IAMAuth bool

	// Namespaces are the schemas the principal needs privileges in.
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

// Engine builds a provisioning plan for one database engine. Planning may read
// the database so the plan holds concrete, reviewable statements.
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

	// BuildRevokePlan returns the steps that withdraw the access again.
	BuildRevokePlan(ctx context.Context, a Access) (*plan.Plan, error)

	// Close releases the engine's connection.
	Close() error
}
