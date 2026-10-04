// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package databaseaccess

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/engine"
	"github.com/blairham/database-controller/internal/plan"
)

const (
	// ConditionConverged reports whether the database already matches the
	// spec: True when the last plan was empty.
	ConditionConverged = "Converged"

	// maxPending caps status.pending. A first reconcile against an
	// unprovisioned database can plan hundreds of statements, and a status
	// object is not where they belong; the count is always exact.
	maxPending = 50
)

// observing reports whether the resource asks for plan-only reconciles.
func observing(da *dbv1alpha1.DatabaseAccess) bool {
	return da.Spec.Mode == dbv1alpha1.ModeObserve
}

// pendingOf flattens a plan into the statements it would run.
//
// Observe mode receives ONLY this list, never the plan. That is the structural
// guarantee that Observe writes nothing: the code path has no step to call
// Apply on, so no later edit to it can execute one by accident.
func pendingOf(p *plan.Plan) []string {
	out := make([]string, 0, p.Len())
	for _, s := range p.Steps() {
		out = append(out, s.Describe())
	}
	return out
}

// setPending records a plan's statements and the Converged condition.
func setPending(da *dbv1alpha1.DatabaseAccess, pending []string) {
	now := metav1.Now()
	da.Status.LastPlannedTime = &now
	da.Status.PendingStatements = len(pending)
	da.Status.Pending = nil
	if len(pending) > 0 {
		capped := pending
		if len(capped) > maxPending {
			capped = capped[:maxPending]
		}
		da.Status.Pending = append([]string(nil), capped...)
	}
	recordPlanned(da.Namespace, da.Name, len(pending), now.Time)

	cond := metav1.Condition{
		Type:               ConditionConverged,
		Status:             metav1.ConditionTrue,
		Reason:             "Converged",
		Message:            "the database matches the spec; nothing is pending",
		ObservedGeneration: da.Generation,
	}
	if len(pending) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Pending"
		cond.Message = fmt.Sprintf("%d statement(s) pending, first: %s", len(pending), firstLine(pending[0]))
	}
	setCondition(da, cond)
}

// observe records what the plan would change and changes nothing.
//
// Ready is True when the database could be read and planned against: that is
// the whole job in this mode, and a Ready=False here keeps its existing
// meaning (the controller cannot reach or plan against the database), which is
// exactly what a cutover needs to know before switching to Enforce. Whether
// the database needs anything is Converged, not Ready.
func (r *Reconciler) observe(
	ctx context.Context,
	da *dbv1alpha1.DatabaseAccess,
	pending []string,
) (ctrl.Result, error) {
	setPending(da, pending)
	da.Status.ObservedGeneration = da.Generation
	// Nothing was applied by this reconcile, so the fields describing an apply
	// are cleared rather than left showing a previous Enforce pass as current.
	// LastAppliedTime and AppliedPlanHash are history and stay.
	da.Status.StatementsApplied = 0
	da.Status.Warnings = nil
	recordObserved(da.Namespace, da.Name)
	setCondition(da, metav1.Condition{
		Type:   ConditionReady,
		Status: metav1.ConditionTrue,
		Reason: "Observed",
		Message: fmt.Sprintf(
			"observe mode: %d statement(s) pending; nothing was applied", len(pending),
		),
		ObservedGeneration: da.Generation,
	})

	if err := r.Status().Update(ctx, da); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	log.FromContext(ctx).Info("observed database access plan",
		"role", da.Spec.Role, "pending", len(pending))
	return ctrl.Result{RequeueAfter: driftInterval}, nil
}

// recordPending re-plans after an Enforce apply and records what is STILL
// pending.
//
// A fresh plan rather than the one just applied, because the applied plan
// cannot say what did not stick: a best-effort default privilege that was
// refused, or a GRANT PostgreSQL accepted with "no privileges were granted"
// because the provisioner lacks rights on that object, both leave the
// database short while the apply reports success. The re-plan reads the
// catalog again and reports the truth, at the cost of one more round of
// reads and no writes.
//
// A failed re-plan does not fail the reconcile -- the apply succeeded -- but
// it does not claim convergence either.
func recordPending(ctx context.Context, da *dbv1alpha1.DatabaseAccess, eng engine.Engine, access engine.Access) {
	p, err := eng.BuildPlan(ctx, access)
	if err != nil {
		setCondition(da, metav1.Condition{
			Type:               ConditionConverged,
			Status:             metav1.ConditionUnknown,
			Reason:             "ReplanFailed",
			Message:            err.Error(),
			ObservedGeneration: da.Generation,
		})
		return
	}
	setPending(da, pendingOf(p))
}

// firstLine trims a statement to its first line for a condition message.
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i] + " ..."
		}
	}
	return s
}
