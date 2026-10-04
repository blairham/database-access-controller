// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Command manager runs the controllers; --controllers selects which.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/controller/databaseaccess"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dbv1alpha1.AddToScheme(scheme))
}

// watchedTypes returns the resources the enabled controllers watch, so
// readiness can verify each is watchable. A new controller needs a row here.
func watchedTypes(names []string) []client.Object {
	var out []client.Object
	for _, n := range names {
		switch n {
		case "databaseaccess":
			out = append(out, &dbv1alpha1.DatabaseAccess{})
		}
	}
	return out
}

func main() {
	var (
		metricsAddr string
		probeAddr   string
		leaderElect bool
		enabledFlag string
		watchNS     string
		zapOpts     zap.Options
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	flag.BoolVar(&leaderElect, "leader-elect", false, "enable leader election, ensuring only one manager is active")
	flag.StringVar(&enabledFlag, "controllers", "databaseaccess", "comma-separated controllers to run")
	flag.StringVar(&watchNS, "watch-namespace", "", "restrict the manager to one namespace; empty watches all")
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")

	options := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "database-controller.io",
	}

	if watchNS != "" {
		options.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{watchNS: {}},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	enabled := map[string]bool{}
	var enabledNames []string
	for _, name := range strings.Split(enabledFlag, ",") {
		if n := strings.TrimSpace(name); n != "" {
			enabled[n] = true
			enabledNames = append(enabledNames, n)
		}
	}

	if enabled["databaseaccess"] {
		r := &databaseaccess.Reconciler{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			Recorder:  mgr.GetEventRecorderFor("databaseaccess"),
			NewEngine: databaseaccess.NewEngineFactory(mgr.GetClient()),
		}
		if err := r.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "databaseaccess")
			os.Exit(1)
		}
		delete(enabled, "databaseaccess")
	}

	// An unknown name is a typo; refuse rather than silently run nothing.
	if len(enabled) > 0 {
		var unknown []string
		for name := range enabled {
			unknown = append(unknown, name)
		}
		setupLog.Error(fmt.Errorf("unknown controllers: %s", strings.Join(unknown, ", ")),
			"refusing to start with an unrecognized --controllers value")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	// Readiness means the watched types resolve, not a ping: with the CRD
	// absent the manager never starts workers but a ping still passes.
	// WaitForCacheSync also passes then (no informer was registered);
	// GetInformer goes through the RESTMapper and fails.
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		for _, obj := range watchedTypes(enabledNames) {
			if _, err := mgr.GetCache().GetInformer(ctx, obj); err != nil {
				return fmt.Errorf("cannot watch %T; is its CRD installed? %w", obj, err)
			}
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
