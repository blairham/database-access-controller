// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package databaseaccess

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/engine"
	"github.com/blairham/database-controller/internal/plan"
)

// fakeEngine records what it was asked to do and returns a canned plan.
type fakeEngine struct {
	mu        sync.Mutex
	applied   int
	revoked   int
	warnings  []string
	planErr   error
	applyErr  error
	lastAcces engine.Access
}

func (f *fakeEngine) Kind() engine.Kind            { return engine.KindPostgres }
func (f *fakeEngine) Validate(engine.Access) error { return nil }
func (f *fakeEngine) Close() error                 { return nil }

func (f *fakeEngine) BuildPlan(_ context.Context, a engine.Access) (*plan.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAcces = a
	if f.planErr != nil {
		return nil, f.planErr
	}
	p := &plan.Plan{}
	p.Add(&fakeStep{owner: f, kind: "apply"})
	return p, nil
}

func (f *fakeEngine) BuildRevokePlan(context.Context, engine.Access) (*plan.Plan, error) {
	p := &plan.Plan{}
	p.Add(&fakeStep{owner: f, kind: "revoke"})
	return p, nil
}

type fakeStep struct {
	owner *fakeEngine
	kind  string
}

func (s *fakeStep) Describe() string  { return "-- fake " + s.kind }
func (s *fakeStep) Rationale() string { return "" }
func (s *fakeStep) IsBestEffort() bool {
	return len(s.owner.warnings) > 0 && s.kind == "apply"
}
func (s *fakeStep) Tolerates(error) bool { return false }

func (s *fakeStep) Apply(context.Context) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if s.kind == "revoke" {
		s.owner.revoked++
		return nil
	}
	if s.owner.applyErr != nil {
		return s.owner.applyErr
	}
	if len(s.owner.warnings) > 0 {
		return errors.New(s.owner.warnings[0])
	}
	s.owner.applied++
	return nil
}

func (f *fakeEngine) factory() EngineFactory {
	return func(context.Context, *dbv1alpha1.DatabaseAccess) (engine.Engine, error) {
		return f, nil
	}
}

func newAccess(t *testing.T, name string, mutate func(*dbv1alpha1.DatabaseAccess)) *dbv1alpha1.DatabaseAccess {
	t.Helper()
	da := &dbv1alpha1.DatabaseAccess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: dbv1alpha1.DatabaseAccessSpec{
			Instance: dbv1alpha1.InstanceRef{
				Endpoint: "example.abc123.us-east-1.rds.amazonaws.com",
				Region:   "us-east-1",
			},
			Role: "app_role",
			Grants: []dbv1alpha1.SchemaGrant{
				{Schema: "app", Privileges: []string{"SELECT"}},
			},
		},
	}
	if mutate != nil {
		mutate(da)
	}
	if err := k8sClient.Create(context.Background(), da); err != nil {
		t.Fatalf("creating DatabaseAccess: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), da)
	})
	return da
}

func get(t *testing.T, name string) *dbv1alpha1.DatabaseAccess {
	t.Helper()
	var out dbv1alpha1.DatabaseAccess
	key := client.ObjectKey{Name: name, Namespace: "default"}
	if err := k8sClient.Get(context.Background(), key, &out); err != nil {
		t.Fatalf("getting %s: %v", name, err)
	}
	return &out
}

// The CRD default must be applied by the API server.
func TestCRDDefaultsEngineToPostgres(t *testing.T) {
	da := newAccess(t, "defaults", nil)
	got := get(t, da.Name)

	if got.Spec.Engine != dbv1alpha1.EnginePostgres {
		t.Errorf("spec.engine = %q after create, want the API server to default it to %q",
			got.Spec.Engine, dbv1alpha1.EnginePostgres)
	}
	if got.Spec.Instance.Port != 5432 {
		t.Errorf("spec.instance.port = %d, want the default 5432", got.Spec.Instance.Port)
	}
	if got.Spec.Instance.Database != "appdb" {
		t.Errorf("spec.instance.database = %q, want the default appdb", got.Spec.Instance.Database)
	}
	if got.Spec.Instance.AdminUser != "db_provisioner" {
		t.Errorf("spec.instance.adminUser = %q, want the default db_provisioner", got.Spec.Instance.AdminUser)
	}
	if !got.Spec.GrantRdsIam {
		t.Error("spec.grantRdsIam = false, want the default true")
	}
	if got.Spec.RevokeOnDelete {
		t.Error("spec.revokeOnDelete = true, want the default false")
	}
}

// The engine enum must be enforced by the API server.
func TestCRDRejectsUnsupportedEngine(t *testing.T) {
	da := &dbv1alpha1.DatabaseAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-engine", Namespace: "default"},
		Spec: dbv1alpha1.DatabaseAccessSpec{
			Engine:   "mysql",
			Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
			Role:     "app_role",
		},
	}
	err := k8sClient.Create(context.Background(), da)
	if err == nil {
		_ = k8sClient.Delete(context.Background(), da)
		t.Fatal("the API server accepted engine=mysql, want the enum to reject it")
	}
}

// Role and schema names reach SQL, so their pattern must be enforced at
// admission too.
func TestCRDRejectsAnInjectedIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name string
		da   *dbv1alpha1.DatabaseAccess
	}{
		{"role", &dbv1alpha1.DatabaseAccess{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-role", Namespace: "default"},
			Spec: dbv1alpha1.DatabaseAccessSpec{
				Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
				Role:     `x"; DROP DATABASE appdb; --`,
			},
		}},
		{"schema", &dbv1alpha1.DatabaseAccess{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-schema", Namespace: "default"},
			Spec: dbv1alpha1.DatabaseAccessSpec{
				Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
				Role:     "app_role",
				Grants:   []dbv1alpha1.SchemaGrant{{Schema: "a b", Privileges: []string{"SELECT"}}},
			},
		}},
		{"privilege", &dbv1alpha1.DatabaseAccess{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-priv", Namespace: "default"},
			Spec: dbv1alpha1.DatabaseAccessSpec{
				Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
				Role:     "app_role",
				Grants:   []dbv1alpha1.SchemaGrant{{Schema: "app", Privileges: []string{"SUPERUSER"}}},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := k8sClient.Create(context.Background(), tc.da); err == nil {
				_ = k8sClient.Delete(context.Background(), tc.da)
				t.Errorf("the API server accepted an invalid %s", tc.name)
			}
		})
	}
}

func TestReconcileAppliesAndReportsReady(t *testing.T) {
	f := &fakeEngine{}
	startManager(t, f.factory())

	da := newAccess(t, "applies", nil)

	eventually(t, 10*time.Second, "Ready=True", func() bool {
		got := get(t, da.Name)
		for _, c := range got.Status.Conditions {
			if c.Type == ConditionReady && c.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	})

	got := get(t, da.Name)
	if got.Status.StatementsApplied != 1 {
		t.Errorf("status.statementsApplied = %d, want 1", got.Status.StatementsApplied)
	}
	if got.Status.AppliedPlanHash == "" {
		t.Error("status.appliedPlanHash is empty, want the plan fingerprint recorded")
	}
	if got.Status.LastAppliedTime == nil {
		t.Error("status.lastAppliedTime is unset")
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("status.observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}

	// The spec must reach the engine intact.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastAcces.Principal != "app_role" {
		t.Errorf("engine saw principal %q, want app_role", f.lastAcces.Principal)
	}
	if len(f.lastAcces.Namespaces) != 1 || f.lastAcces.Namespaces[0].Name != "app" {
		t.Errorf("engine saw namespaces %+v, want one named app", f.lastAcces.Namespaces)
	}
}

// A failure has to land on the resource's status.
func TestReconcileReportsFailureOnTheResource(t *testing.T) {
	f := &fakeEngine{planErr: fmt.Errorf("connection refused")}
	startManager(t, f.factory())

	da := newAccess(t, "fails", nil)

	eventually(t, 10*time.Second, "Ready=False", func() bool {
		got := get(t, da.Name)
		for _, c := range got.Status.Conditions {
			if c.Type == ConditionReady && c.Status == metav1.ConditionFalse {
				return true
			}
		}
		return false
	})

	got := get(t, da.Name)
	for _, c := range got.Status.Conditions {
		if c.Type != ConditionReady {
			continue
		}
		if c.Reason != "PlanFailed" {
			t.Errorf("condition reason = %q, want PlanFailed", c.Reason)
		}
		if c.Message == "" {
			t.Error("condition message is empty, want the underlying error")
		}
	}
}

// The finalizer is only added when deletion has work to do.
func TestFinalizerOnlyWhenRevokeOnDelete(t *testing.T) {
	f := &fakeEngine{}
	startManager(t, f.factory())

	plain := newAccess(t, "no-revoke", nil)
	revoking := newAccess(t, "revokes", func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.RevokeOnDelete = true
	})

	eventually(t, 10*time.Second, "the finalizer to be added", func() bool {
		return len(get(t, revoking.Name).Finalizers) > 0
	})

	eventually(t, 10*time.Second, "the plain resource to reconcile", func() bool {
		return get(t, plain.Name).Status.AppliedPlanHash != ""
	})

	if fin := get(t, plain.Name).Finalizers; len(fin) != 0 {
		t.Errorf("revokeOnDelete=false carries finalizers %v, want none", fin)
	}
}

// Deleting with revokeOnDelete must run the revoke plan and then release the
// finalizer, so the resource actually goes away.
func TestDeleteRevokesThenReleasesTheFinalizer(t *testing.T) {
	f := &fakeEngine{}
	startManager(t, f.factory())

	da := newAccess(t, "delete-revokes", func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.RevokeOnDelete = true
	})

	eventually(t, 10*time.Second, "the finalizer to be added", func() bool {
		return len(get(t, da.Name).Finalizers) > 0
	})

	if err := k8sClient.Delete(context.Background(), da); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	eventually(t, 10*time.Second, "the resource to be gone", func() bool {
		var out dbv1alpha1.DatabaseAccess
		err := k8sClient.Get(context.Background(),
			client.ObjectKey{Name: da.Name, Namespace: "default"}, &out)
		return err != nil
	})

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked == 0 {
		t.Error("the revoke plan never ran, but revokeOnDelete was true")
	}
}

// Password auth resolves the Secret from the resource's own namespace only.
func TestPasswordAuthReadsTheSecret(t *testing.T) {
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-creds", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("s3cret")},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("creating secret: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	da := newAccess(t, "password-auth", func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.Instance.Auth = &dbv1alpha1.InstanceAuth{
			Method:            dbv1alpha1.AuthPassword,
			PasswordSecretRef: &dbv1alpha1.SecretKeySelector{Name: "admin-creds"},
		}
	})

	got, err := resolvePassword(ctx, k8sClient, get(t, da.Name))
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "s3cret" {
		t.Errorf("resolved password %q, want s3cret", got)
	}
}

func TestPasswordAuthErrorsAreActionable(t *testing.T) {
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wrong-key", Namespace: "default"},
		Data:       map[string][]byte{"pw": []byte("s3cret")},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("creating secret: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	for _, tc := range []struct {
		name string
		auth *dbv1alpha1.InstanceAuth
		want string
	}{
		{
			name: "no reference",
			auth: &dbv1alpha1.InstanceAuth{Method: dbv1alpha1.AuthPassword},
			want: "passwordSecretRef is unset",
		},
		{
			name: "missing secret",
			auth: &dbv1alpha1.InstanceAuth{
				Method:            dbv1alpha1.AuthPassword,
				PasswordSecretRef: &dbv1alpha1.SecretKeySelector{Name: "nope"},
			},
			want: "reading secret",
		},
		{
			name: "wrong key names the keys it found",
			auth: &dbv1alpha1.InstanceAuth{
				Method:            dbv1alpha1.AuthPassword,
				PasswordSecretRef: &dbv1alpha1.SecretKeySelector{Name: "wrong-key"},
			},
			want: `has no key "password"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			da := &dbv1alpha1.DatabaseAccess{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec:       dbv1alpha1.DatabaseAccessSpec{Instance: dbv1alpha1.InstanceRef{Auth: tc.auth}},
			}
			_, err := resolvePassword(ctx, k8sClient, da)
			if err == nil {
				t.Fatal("resolvePassword succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// Auth defaults to IAM.
func TestAuthDefaultsToIAM(t *testing.T) {
	da := newAccess(t, "auth-default", func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.Instance.Auth = &dbv1alpha1.InstanceAuth{}
	})
	got := get(t, da.Name)
	if m := got.Spec.Instance.Auth.Method; m != dbv1alpha1.AuthIAM {
		t.Errorf("auth.method = %q, want the API server to default it to %q", m, dbv1alpha1.AuthIAM)
	}
	if authMethod(get(t, "auth-default").Spec.Instance) != dbv1alpha1.AuthIAM {
		t.Error("authMethod did not report iam")
	}
}
