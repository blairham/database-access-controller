// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

// Package databaseaccess's controller tests run against a real Kubernetes API
// server via envtest, covering the CRD schema as enforced and the reconcile
// contract. The engine is faked; no database is involved.
//
//	go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use -p path
//	KUBEBUILDER_ASSETS=$(...) go test -tags envtest ./internal/controller/...
package databaseaccess

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
)

// maxPending is the cap k8s-controller-kit puts on status.pending; the
// observe tests check it holds.
const maxPending = 50

var (
	testEnv   *envtest.Environment
	testCfg   *rest.Config
	k8sClient client.Client
	testSch   = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	utilruntime.Must(clientgoscheme.AddToScheme(testSch))
	utilruntime.Must(dbv1alpha1.AddToScheme(testSch))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	testCfg, err = testEnv.Start()
	if err != nil {
		panic("starting envtest (is KUBEBUILDER_ASSETS set?): " + err.Error())
	}

	k8sClient, err = client.New(testCfg, client.Options{Scheme: testSch})
	if err != nil {
		panic("building client: " + err.Error())
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		panic("stopping envtest: " + err.Error())
	}
	os.Exit(code)
}

// eventually polls until cond returns true or the deadline passes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// startManager runs the reconciler with the given engine factory and stops it
// when the test ends.
func startManager(t *testing.T, factory EngineFactory) {
	t.Helper()

	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme: testSch,
		// A metrics listener per test would collide on the port.
		Metrics: server.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("creating manager: %v", err)
	}

	r := &Reconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("databaseaccess-test"),
		NewEngine: factory,
		// Each test runs its own manager in the same process, and
		// controller-runtime requires controller names to be unique per
		// process so two controllers cannot report the same metric.
		Options: controller.Options{SkipNameValidation: ptr.To(true)},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("setting up reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager stopped: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}
}
