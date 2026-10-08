// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Package databaseaccess reconciles DatabaseAccess resources against a
// database's data plane. The reconcile loop -- Enforce and Observe, the
// Ready and Converged conditions, the finalizer, the metrics -- is
// k8s-controller-kit's; this package supplies the PostgreSQL engine and the
// mapping from the DatabaseAccess API.
package databaseaccess

import (
	"context"

	"github.com/blairham/k8s-controller-kit/plan"
	"github.com/blairham/k8s-controller-kit/reconciler"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	dbv1alpha1 "github.com/blairham/database-access-controller/apis/db/v1alpha1"
	"github.com/blairham/database-access-controller/internal/engine"
)

const (
	// finalizer keeps the resource around long enough to revoke, and is only
	// added when the spec asks for revocation.
	finalizer = "database-access-controller.io/revoke-on-delete"

	// ConditionReady reports whether the last reconcile applied cleanly.
	ConditionReady = reconciler.ConditionReady

	// ConditionConverged reports whether the database already matches the
	// spec: True when the last plan was empty.
	ConditionConverged = reconciler.ConditionConverged
)

// accessMetrics are registered once per process: every Reconciler in it, including
// one per envtest manager, reports into the same series. The names are the
// ones the chart's PrometheusRule alerts on, such as
// database_access_controller_access_ready.
var accessMetrics = reconciler.NewMetrics(ctrlmetrics.Registry, reconciler.MetricsConfig{
	Prefix: "database_access_controller",
	Kind:   "DatabaseAccess",
	Noun:   "statement",
})

// Reconciler reconciles a DatabaseAccess.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// NewEngine opens the engine for a resource.
	NewEngine EngineFactory

	// Options are passed to the underlying controller. Tests set
	// SkipNameValidation because each runs its own manager.
	Options controller.Options
}

// +kubebuilder:rbac:groups=database-access-controller.io,resources=databaseaccesses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database-access-controller.io,resources=databaseaccesses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database-access-controller.io,resources=databaseaccesses/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile brings the database in line with the resource.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.kit().Reconcile(ctx, req)
}

// SetupWithManager registers the reconciler with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.kit().SetupWithManager(mgr)
}

// session is one resource's engine and the access it plans for.
type session struct {
	eng    engine.Engine
	access engine.Access
}

func (s *session) Plan(ctx context.Context) (*plan.Plan, error) {
	return s.eng.BuildPlan(ctx, s.access)
}

func (s *session) RevokePlan(ctx context.Context) (*plan.Plan, error) {
	return s.eng.BuildRevokePlan(ctx, s.access)
}

func (s *session) Close() error { return s.eng.Close() }

// kit wires this API into k8s-controller-kit's reconciler.
func (r *Reconciler) kit() *reconciler.Reconciler[*dbv1alpha1.DatabaseAccess] {
	return &reconciler.Reconciler[*dbv1alpha1.DatabaseAccess]{
		Client:   r.Client,
		Recorder: r.Recorder,
		Name:     "databaseaccess",
		New:      func() *dbv1alpha1.DatabaseAccess { return &dbv1alpha1.DatabaseAccess{} },
		Open: func(ctx context.Context, da *dbv1alpha1.DatabaseAccess) (reconciler.Session, error) {
			r.warnOverlaps(ctx, da)
			eng, err := r.NewEngine(ctx, da)
			if err != nil {
				return nil, err
			}
			return &session{eng: eng, access: Access(da.Spec)}, nil
		},
		Observing:      func(da *dbv1alpha1.DatabaseAccess) bool { return da.Spec.Mode == dbv1alpha1.ModeObserve },
		RevokeOnDelete: func(da *dbv1alpha1.DatabaseAccess) bool { return da.Spec.RevokeOnDelete },
		StatusOf: func(da *dbv1alpha1.DatabaseAccess) reconciler.Status {
			st := &da.Status
			return reconciler.Status{
				Conditions:         &st.Conditions,
				ObservedGeneration: &st.ObservedGeneration,
				AppliedPlanHash:    &st.AppliedPlanHash,
				LastAppliedTime:    &st.LastAppliedTime,
				Applied:            &st.StatementsApplied,
				Warnings:           &st.Warnings,
				PendingCount:       &st.PendingStatements,
				Pending:            &st.Pending,
				LastPlannedTime:    &st.LastPlannedTime,
			}
		},
		Finalizer: finalizer,
		Metrics:   accessMetrics,
		Noun:      "statement",
		Target:    "database",
		Options:   r.Options,
	}
}

// Access flattens the spec into the engine-neutral shape. Exported so dbctl
// maps manifests exactly as the controller does.
func Access(spec dbv1alpha1.DatabaseAccessSpec) engine.Access {
	db := spec.Instance.Database
	if db == "" {
		db = "appdb"
	}
	a := engine.Access{
		Database:  db,
		Principal: spec.Role,
		IAMAuth:   spec.GrantRdsIam,
	}
	for _, g := range spec.Grants {
		a.Namespaces = append(a.Namespaces, engine.Namespace{
			Name:       g.Schema,
			Privileges: g.Privileges,
			Owner:      g.OwnerOf,
		})
	}
	return a
}
