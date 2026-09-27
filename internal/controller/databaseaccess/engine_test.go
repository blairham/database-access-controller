package databaseaccess

import (
	"context"
	"strings"
	"testing"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
)

// An unset engine must behave as postgres. Resources written before the field
// existed carry no value, and the CRD default only fills it on a write.
func TestDefaultEngineFactoryAcceptsEmptyEngine(t *testing.T) {
	_, err := DefaultEngineFactory(context.Background(), dbv1alpha1.DatabaseAccessSpec{
		Engine: "",
		Instance: dbv1alpha1.InstanceRef{
			Endpoint: "127.0.0.1",
			Port:     1,
			Region:   "us-east-1",
		},
		Role: "app",
	})
	// Connecting fails -- there is no database on port 1 -- but it must fail
	// on the connection, not on engine dispatch.
	if err != nil && strings.Contains(err.Error(), "unsupported engine") {
		t.Fatalf("empty engine was rejected as unsupported, want it treated as postgres: %v", err)
	}
}

func TestDefaultEngineFactoryRejectsUnknownEngine(t *testing.T) {
	_, err := DefaultEngineFactory(context.Background(), dbv1alpha1.DatabaseAccessSpec{
		Engine:   "cassandra",
		Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
		Role:     "app",
	})
	if err == nil {
		t.Fatal("DefaultEngineFactory accepted engine \"cassandra\", want an error")
	}
	if !strings.Contains(err.Error(), "unsupported engine") {
		t.Errorf("error is %q, want it to name the unsupported engine", err)
	}
}

// An engine that is planned but not yet implemented must be refused rather than
// silently provisioned with PostgreSQL DDL.
func TestDefaultEngineFactoryRejectsUnimplementedEngine(t *testing.T) {
	_, err := DefaultEngineFactory(context.Background(), dbv1alpha1.DatabaseAccessSpec{
		Engine:   "mysql",
		Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
		Role:     "app",
	})
	if err == nil {
		t.Fatal("DefaultEngineFactory accepted engine \"mysql\", want an error until MySQL is implemented")
	}
}
