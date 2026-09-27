package databaseaccess

import (
	"context"
	"fmt"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/engine"
	"github.com/blairham/database-controller/internal/engine/postgres"
	"github.com/blairham/database-controller/internal/rdsauth"
)

// DefaultEngineFactory opens the engine a DatabaseAccess names.
//
// Only PostgreSQL is implemented. MySQL and SQL Server land here as additional
// cases; DynamoDB deliberately never will, because it has no data-plane
// principals -- access to a table is an IAM policy, which provider-aws-iam
// already manages.
func DefaultEngineFactory(ctx context.Context, spec dbv1alpha1.DatabaseAccessSpec) (engine.Engine, error) {
	// The CRD enum rejects an unsupported engine at admission, so reaching this
	// error means either a resource that predates the enum or a controller
	// older than the CRD it is serving. Both are worth saying out loud rather
	// than defaulting to PostgreSQL and provisioning the wrong dialect.
	switch spec.Engine {
	case "", dbv1alpha1.EnginePostgres:
	default:
		return nil, fmt.Errorf("unsupported engine %q: this controller implements %q",
			spec.Engine, dbv1alpha1.EnginePostgres)
	}

	inst := spec.Instance

	tokens, err := rdsauth.New(ctx, rdsauth.Config{
		Host:   inst.Endpoint,
		Port:   inst.Port,
		Region: inst.Region,
		User:   adminUser(inst),
	})
	if err != nil {
		return nil, fmt.Errorf("preparing IAM auth: %w", err)
	}

	conn, err := postgres.Connect(ctx, postgres.ConnConfig{
		Host:     inst.Endpoint,
		Port:     inst.Port,
		Database: database(inst),
		User:     adminUser(inst),
		SSLMode:  inst.SSLMode,
		Tokens:   tokens,
	})
	if err != nil {
		return nil, err
	}

	closeFn := func() error { return conn.Close(context.WithoutCancel(ctx)) }
	return postgres.New(conn, conn, closeFn), nil
}

func adminUser(i dbv1alpha1.InstanceRef) string {
	if i.AdminUser == "" {
		return "db_provisioner"
	}
	return i.AdminUser
}

func database(i dbv1alpha1.InstanceRef) string {
	if i.Database == "" {
		return "appdb"
	}
	return i.Database
}
