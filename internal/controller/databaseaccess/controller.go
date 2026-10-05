// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Package databaseaccess reconciles DatabaseAccess resources against a
// database's data plane.
package databaseaccess

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
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

	// driftInterval is how often a resource is re-reconciled with no event, so
	// out-of-band changes to ownership or grants get corrected.
	driftInterval = time.Hour
)

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
		if apierrors.IsNotFound(err) {
			// Drop its metric series, or a resource deleted while failing
			// would alert forever.
			forget(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !da.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &da)
	}

	// Only carry the finalizer when deletion has work to do; Observe never
	// revokes.
	if da.Spec.RevokeOnDelete && !observing(&da) && !controllerutil.ContainsFinalizer(&da, finalizer) {
		if err := r.setFinalizer(ctx, &da, true); err != nil {
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

	if observing(&da) {
		return r.observe(ctx, &da, pendingOf(p))
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

	// A skipped default privilege means objects recreated later lose access.
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
	recordPending(ctx, &da, eng, access)

	// Before the status write: the database already reflects the apply.
	recordApplied(da.Namespace, da.Name, len(res.Warnings), now.Time)

	if err := r.Status().Update(ctx, &da); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: driftInterval}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, da *dbv1alpha1.DatabaseAccess) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(da, finalizer) {
		forget(da.Namespace, da.Name)
		return ctrl.Result{}, nil
	}

	// A resource switched to Observe is released without revoking.
	if da.Spec.RevokeOnDelete && !observing(da) {
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

	if err := r.setFinalizer(ctx, da, false); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	forget(da.Namespace, da.Name)
	return ctrl.Result{}, nil
}

// setFinalizer adds (present) or removes our finalizer and copies the
// resulting metadata back into da.
//
// It patches a fresh read rather than updating da. da was read at the start
// of the reconcile, and on the delete path the revoke plan runs in between, so
// any write to the object meanwhile (a status update, an annotation, another
// controller's finalizer) leaves da's resourceVersion stale. A full Update
// then fails with "the object has been modified", and the work queue retries
// the whole reconcile: the revoke plan runs again. stg showed exactly that on
// 2026-10-04.
//
// The patch keeps the optimistic lock. A JSON merge patch replaces
// metadata.finalizers wholesale, so a lock-free patch built from a list that
// went stale would, on the add path, silently drop a finalizer another
// controller added meanwhile, and on the delete path write back one it had
// just removed. (The API forbids adding finalizers to an object being deleted,
// so that fails loudly, but it still fails the reconcile.) Reading immediately
// before patching shrinks the conflict window to one round trip, and
// RetryOnConflict absorbs a conflict there, re-reading, instead of returning
// to the queue.
func (r *Reconciler) setFinalizer(ctx context.Context, da *dbv1alpha1.DatabaseAccess, present bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh dbv1alpha1.DatabaseAccess
		if err := r.Get(ctx, client.ObjectKeyFromObject(da), &fresh); err != nil {
			// Already gone: nothing left to release.
			if !present && apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		base := fresh.DeepCopy()
		var changed bool
		if present {
			changed = controllerutil.AddFinalizer(&fresh, finalizer)
		} else {
			changed = controllerutil.RemoveFinalizer(&fresh, finalizer)
		}
		if changed {
			patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
			if err := r.Patch(ctx, &fresh, patch); err != nil {
				return err
			}
		}
		// Carry the new resourceVersion forward, or the status write that
		// follows on the add path would conflict in turn.
		da.ObjectMeta = *fresh.ObjectMeta.DeepCopy()
		return nil
	})
}

// fail records the error on the resource and returns it so the work queue
// retries with backoff.
func (r *Reconciler) fail(
	ctx context.Context,
	da *dbv1alpha1.DatabaseAccess,
	reason string,
	cause error,
) (ctrl.Result, error) {
	// Before the status write, so the metric moves even if that write fails.
	recordFailed(da.Namespace, da.Name)

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

// SetupWithManager registers the reconciler with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.DatabaseAccess{}).
		Named("databaseaccess").
		WithOptions(r.Options).
		Complete(r)
}
