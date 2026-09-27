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
	"flag"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "database-controller.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	enabled := map[string]bool{}
	for _, name := range strings.Split(enabledFlag, ",") {
		if n := strings.TrimSpace(name); n != "" {
			enabled[n] = true
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
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
