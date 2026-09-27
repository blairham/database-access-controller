// Command manager runs the provisioner controllers.
//
// One binary hosts this repo's controllers, which all provision the data plane
// of a managed database. They share the image, the RBAC, the leader election
// lease and the metrics endpoint; --controllers selects which of them run.
//
// Controllers for other domains belong in their own repo, named for that
// domain, the way aws-load-balancer-controller and the ACK per-service
// controllers are.
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
// readiness can verify each one is actually watchable. A controller added
// without a row here would report ready while unable to see its own resource.
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

	// Restricting the cache is what --watch-namespace has to do. Setting the
	// flag without this leaves the manager watching every namespace while
	// reporting that it is scoped to one, which is the kind of gap nothing
	// surfaces until a resource somewhere unexpected gets reconciled.
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

	// An unknown name is a typo in a Deployment argument, and silently running
	// nothing is the worst possible response to it.
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

	// Readiness is "can I watch what I am here to watch", NOT a ping.
	//
	// A ping passes as soon as the HTTP server is listening, which says
	// nothing about whether the manager can watch anything. With the CRD
	// absent the controller logs `no matches for kind "DatabaseAccess"` every
	// few seconds and never starts its workers -- while the pod reports Ready,
	// a rollout completes, and any PDB or readiness gate downstream is
	// satisfied. Measured on a rig: 1/1 Running, zero "Starting workers", and
	// completely inert.
	//
	// ⚠ WaitForCacheSync IS NOT THE CHECK, and it looks like it should be. It
	// waits on the informers the cache has been asked for, and when the CRD is
	// missing the informer was never successfully registered -- so there is
	// nothing to wait for and it returns true. Tried first, and the pod stayed
	// Ready with the CRD deleted.
	//
	// GetInformer resolves the type through the RESTMapper, so it fails while
	// the CRD is absent and succeeds once it exists.
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
