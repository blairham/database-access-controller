// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package databaseaccess

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
)

// The CRD the chart ships is not the CRD controller-gen wrote: it is passed
// through Helm templating, which injects an annotation and escapes anything
// that looks like a Helm action. This installs the CHART's rendering into a
// fresh API server and checks the schema still does its job.
//
// Rendering and installing are the only way to catch this. A chart that emits
// a subtly broken CRD lints clean, renders clean, and fails at apply time on
// someone else's cluster.
func TestChartRenderedCRDInstallsAndValidates(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("helm", "template", "test",
		filepath.Join(root, "charts", "database-controller"),
		"--show-only", "templates/crds.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("rendering the chart: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "kind: CustomResourceDefinition") {
		t.Fatalf("the chart rendered no CRD:\n%s", out)
	}

	// envtest installs every CRD in a directory, so the rendering gets its own.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "crd.yaml"), out, 0o600); err != nil {
		t.Fatal(err)
	}

	sch := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	utilruntime.Must(dbv1alpha1.AddToScheme(sch))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{dir},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest with the chart's CRD: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Defaulting survived templating.
	da := &dbv1alpha1.DatabaseAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "from-chart", Namespace: "default"},
		Spec: dbv1alpha1.DatabaseAccessSpec{
			Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
			Role:     "app_role",
			Grants:   []dbv1alpha1.SchemaGrant{{Schema: "app", Privileges: []string{"SELECT"}}},
		},
	}
	if err := c.Create(ctx, da); err != nil {
		t.Fatalf("creating a DatabaseAccess against the chart's CRD: %v", err)
	}

	var got dbv1alpha1.DatabaseAccess
	if err := c.Get(ctx, client.ObjectKeyFromObject(da), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Engine != dbv1alpha1.EnginePostgres {
		t.Errorf("spec.engine = %q, want the chart's CRD to default it to %q",
			got.Spec.Engine, dbv1alpha1.EnginePostgres)
	}
	if got.Spec.Instance.Port != 5432 {
		t.Errorf("spec.instance.port = %d, want the default 5432", got.Spec.Instance.Port)
	}

	// Validation survived templating. The identifier pattern is the only thing
	// between a CRD field and injected SQL.
	bad := &dbv1alpha1.DatabaseAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default"},
		Spec: dbv1alpha1.DatabaseAccessSpec{
			Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
			Role:     `x"; DROP DATABASE appdb; --`,
		},
	}
	if err := c.Create(ctx, bad); err == nil {
		t.Error("the chart's CRD accepted an injected role name")
	}

	// The enum survived templating.
	badEngine := &dbv1alpha1.DatabaseAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-engine", Namespace: "default"},
		Spec: dbv1alpha1.DatabaseAccessSpec{
			Engine:   "mysql",
			Instance: dbv1alpha1.InstanceRef{Endpoint: "db", Region: "us-east-1"},
			Role:     "app_role",
		},
	}
	if err := c.Create(ctx, badEngine); err == nil {
		t.Error("the chart's CRD accepted engine=mysql")
	}
}
