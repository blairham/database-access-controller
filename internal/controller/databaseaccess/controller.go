// Package databaseaccess reconciles DatabaseAccess resources against a
// database's data plane.
package databaseaccess

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/engine"
)

const (
	// finalizer keeps the resource around long enough to revoke, and is only
	// added when the spec asks for revocation.
	finalizer = "database-controller.io/revoke-on-delete"

	// ConditionReady reports whether the last reconcile applied cleanly.
	ConditionReady = "Ready"

	// driftInterval is how often a resource is re-reconciled with no event to
	// prompt it.
	//
	// This is the part a Job could not do. A Job runs once and stops, so an
	// ownership change made outside the platform -- a migration creating a view
	// as the wrong role, a hand-run GRANT -- persisted until someone noticed.
	// Re-planning on an interval turns that into a correction.
	driftInterval = time.Hour
)

// Reconciler reconciles a DatabaseAccess.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// NewEngine opens the engine for a resource.
	NewEngine EngineFactory

	// Options are passed to the underlying controller. The zero value is
	// correct in production; tests set SkipNameValidation because
	// controller-runtime requires controller names to be unique per process
	// and each test runs its own manager.
	Options controller.Options
}

// +kubebuilder:rbac:groups=database-controller.io,resources=databaseaccesses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database-controller.io,resources=databaseaccesses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database-controller.io,resources=databaseaccesses/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile brings the database in line with the resource.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var da dbv1alpha1.DatabaseAccess
	if err := r.Get(ctx, req.NamespacedName, &da); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !da.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &da)
	}

	// The finalizer is only worth carrying when deletion has work to do.
	// Holding one otherwise turns a stuck controller into a namespace that
	// cannot be deleted.
	if da.Spec.RevokeOnDelete && !controllerutil.ContainsFinalizer(&da, finalizer) {
		controllerutil.AddFinalizer(&da, finalizer)
		if err := r.Update(ctx, &da); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	eng, err := r.NewEngine(ctx, &da)
	if err != nil {
		return r.fail(ctx, &da, "ConnectionFailed", err)
	}
	defer eng.Close() //nolint:errcheck // close failure on a read-only teardown path

	access := Access(da.Spec)

	p, err := eng.BuildPlan(ctx, access)
	if err != nil {
		return r.fail(ctx, &da, "PlanFailed", err)
	}

	res, err := p.Apply(ctx)
	if err != nil {
		return r.fail(ctx, &da, "ApplyFailed", err)
	}

	logger.Info("applied database access plan",
		"role", da.Spec.Role,
		"statements", res.Applied,
		"warnings", len(res.Warnings),
		"hash", p.Hash())

	// Warnings are surfaced on the resource rather than left in a pod log. They
	// are the difference between "access was granted" and "access was granted
	// and will survive the next migration": a default privilege that could not
	// be registered means the next DROP/CREATE of an object silently revokes
	// access again.
	if len(res.Warnings) > 0 {
		r.Recorder.Eventf(&da, "Warning", "PartiallyApplied",
			"%d statement(s) were skipped; future objects may not inherit access", len(res.Warnings))
	}

	now := metav1.Now()
	da.Status.ObservedGeneration = da.Generation
	da.Status.AppliedPlanHash = p.Hash()
	da.Status.LastAppliedTime = &now
	da.Status.StatementsApplied = res.Applied
	da.Status.Warnings = res.Warnings
	meta := metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Applied",
		Message:            fmt.Sprintf("applied %d statement(s)", res.Applied),
		ObservedGeneration: da.Generation,
	}
	if len(res.Warnings) > 0 {
		meta.Reason = "AppliedWithWarnings"
		meta.Message = fmt.Sprintf("applied %d statement(s), %d skipped", res.Applied, len(res.Warnings))
	}
	setCondition(&da, meta)

	if err := r.Status().Update(ctx, &da); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: driftInterval}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, da *dbv1alpha1.DatabaseAccess) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(da, finalizer) {
		return ctrl.Result{}, nil
	}

	if da.Spec.RevokeOnDelete {
		eng, err := r.NewEngine(ctx, da)
		if err != nil {
			return r.fail(ctx, da, "ConnectionFailed", err)
		}
		defer eng.Close() //nolint:errcheck // close failure on a read-only teardown path

		p, err := eng.BuildRevokePlan(ctx, Access(da.Spec))
		if err != nil {
			return r.fail(ctx, da, "RevokePlanFailed", err)
		}
		if _, err := p.Apply(ctx); err != nil {
			return r.fail(ctx, da, "RevokeFailed", err)
		}
	}

	controllerutil.RemoveFinalizer(da, finalizer)
	if err := r.Update(ctx, da); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// fail records the error on the resource and returns it so the work queue
// retries with backoff.
func (r *Reconciler) fail(ctx context.Context, da *dbv1alpha1.DatabaseAccess, reason string, cause error) (ctrl.Result, error) {
	setCondition(da, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: da.Generation,
	})
	da.Status.ObservedGeneration = da.Generation

	if err := r.Status().Update(ctx, da); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after %s: %w (original error: %v)", reason, err, cause)
	}
	r.Recorder.Event(da, "Warning", reason, cause.Error())
	return ctrl.Result{}, cause
}

func setCondition(da *dbv1alpha1.DatabaseAccess, c metav1.Condition) {
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = metav1.Now()
	}
	for i, existing := range da.Status.Conditions {
		if existing.Type != c.Type {
			continue
		}
		if existing.Status == c.Status {
			c.LastTransitionTime = existing.LastTransitionTime
		}
		da.Status.Conditions[i] = c
		return
	}
	da.Status.Conditions = append(da.Status.Conditions, c)
}

// Access flattens the spec into the engine-neutral shape. It is exported so
// dbctl can plan from the same manifest the controller reconciles,
// rather than reimplementing the mapping and drifting from it.
func Access(spec dbv1alpha1.DatabaseAccessSpec) engine.Access {
	db := spec.Instance.Database
	if db == "" {
		db = "appdb"
	}
	a := engine.Access{
		Database:  db,
		Principal: spec.Role,
		IAMAuth:   spec.IAMAuth,
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

// SetupWithManager registers the reconciler with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.DatabaseAccess{}).
		Named("databaseaccess").
		WithOptions(r.Options).
		Complete(r)
}
