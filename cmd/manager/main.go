// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Command manager runs the controllers; --controllers selects which.
package main

import (
	"fmt"
	"os"

	"github.com/blairham/k8s-controller-kit/manager"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/controller/databaseaccess"
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dbv1alpha1.AddToScheme(scheme))

	// Flags, leader election, --controllers and the readiness check that
	// fails while a CRD is missing all come from k8s-controller-kit.
	err := manager.Main(manager.Config{
		Scheme:           scheme,
		LeaderElectionID: "database-controller.io",
		Controllers: map[string]manager.Controller{
			"databaseaccess": {
				Watches: &dbv1alpha1.DatabaseAccess{},
				Setup: func(mgr ctrl.Manager) error {
					// The kit's Reconciler takes the old-API recorder until
					// blairham/k8s-controller-kit#6.
					recorder := mgr.GetEventRecorderFor("databaseaccess") //nolint:staticcheck // kit#6
					return (&databaseaccess.Reconciler{
						Client:    mgr.GetClient(),
						Scheme:    mgr.GetScheme(),
						Recorder:  recorder,
						NewEngine: databaseaccess.NewEngineFactory(mgr.GetClient()),
					}).SetupWithManager(mgr)
				},
			},
		},
	}, os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "manager: %v\n", err)
		os.Exit(1)
	}
}
