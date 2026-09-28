//go:build envtest

package databaseaccess

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
)

// The controller does the same work against RDS and against a PostgreSQL
// running anywhere else; the only thing that varies is how the admin
// connection authenticates. Region is the one AWS-shaped field in the spec,
// and it belongs to the IAM path alone -- so it has to be required there and
// absent-able everywhere else.
//
// Both halves matter. A rule that only accepted the in-cluster shape would let
// an IAM instance through with no region and fail at reconcile time, which is
// the failure this schema exists to prevent.
func TestInstanceRegionIsRequiredOnlyForIAM(t *testing.T) {
	ctx := context.Background()

	instance := func(mutate func(*dbv1alpha1.InstanceRef)) dbv1alpha1.InstanceRef {
		inst := dbv1alpha1.InstanceRef{Endpoint: "db.example"}
		mutate(&inst)
		return inst
	}

	cases := []struct {
		name     string
		instance dbv1alpha1.InstanceRef
		wantErr  bool
	}{
		{
			// The default auth method is IAM, so an omitted auth block is an
			// AWS instance and still owes a region.
			name:     "iam-by-default-without-region",
			instance: instance(func(*dbv1alpha1.InstanceRef) {}),
			wantErr:  true,
		},
		{
			name: "iam-explicit-without-region",
			instance: instance(func(i *dbv1alpha1.InstanceRef) {
				i.Auth = &dbv1alpha1.InstanceAuth{Method: dbv1alpha1.AuthIAM}
			}),
			wantErr: true,
		},
		{
			// The AWS path, unchanged.
			name: "iam-with-region",
			instance: instance(func(i *dbv1alpha1.InstanceRef) {
				i.Region = "us-east-1"
			}),
			wantErr: false,
		},
		{
			// The in-cluster path: no region to give, and none demanded.
			name: "password-without-region",
			instance: instance(func(i *dbv1alpha1.InstanceRef) {
				i.Auth = &dbv1alpha1.InstanceAuth{
					Method:            dbv1alpha1.AuthPassword,
					PasswordSecretRef: &dbv1alpha1.SecretKeySelector{Name: "admin"},
				}
			}),
			wantErr: false,
		},
		{
			// Password auth with nowhere to read the password from was a
			// reconcile-time error. The API server can say it at admission.
			name: "password-without-secret-ref",
			instance: instance(func(i *dbv1alpha1.InstanceRef) {
				i.Auth = &dbv1alpha1.InstanceAuth{Method: dbv1alpha1.AuthPassword}
			}),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(st *testing.T) {
			da := &dbv1alpha1.DatabaseAccess{
				ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "default"},
				Spec: dbv1alpha1.DatabaseAccessSpec{
					Instance: tc.instance,
					Role:     "app_role",
					Grants:   []dbv1alpha1.SchemaGrant{{Schema: "app", Privileges: []string{"SELECT"}}},
				},
			}

			err := k8sClient.Create(ctx, da)
			if err == nil {
				// Cleanup hangs off the parent: the accepted object has to
				// outlive its subtest for the round-trip check below.
				t.Cleanup(func() { _ = k8sClient.Delete(ctx, da) })
			}

			switch {
			case tc.wantErr && err == nil:
				st.Fatal("the API server accepted the spec, want it rejected")
			case !tc.wantErr && err != nil:
				st.Fatalf("the API server rejected the spec: %v", err)
			}
		})
	}

	// Region genuinely round-trips on the IAM path; the rule above is not
	// passing it by ignoring the field.
	var got dbv1alpha1.DatabaseAccess
	key := client.ObjectKey{Name: "iam-with-region", Namespace: "default"}
	if err := k8sClient.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Instance.Region != "us-east-1" {
		t.Errorf("spec.instance.region = %q, want %q", got.Spec.Instance.Region, "us-east-1")
	}
}
