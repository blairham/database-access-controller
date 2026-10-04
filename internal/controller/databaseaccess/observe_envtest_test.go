// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package databaseaccess

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/engine"
	"github.com/blairham/database-controller/internal/plan"
)

// countingEngine returns a plan of `pending` steps and counts every Apply,
// including revokes. Observe mode is correct only if that count stays zero.
type countingEngine struct {
	mu      sync.Mutex
	pending int
	applied int
	revoked int
	plans   int
}

func (c *countingEngine) Kind() engine.Kind            { return engine.KindPostgres }
func (c *countingEngine) Validate(engine.Access) error { return nil }
func (c *countingEngine) Close() error                 { return nil }

func (c *countingEngine) BuildPlan(context.Context, engine.Access) (*plan.Plan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans++
	p := &plan.Plan{}
	for i := range c.pending {
		p.Add(&countingStep{owner: c, sql: fmt.Sprintf("GRANT SELECT ON t%d TO app_role", i)})
	}
	return p, nil
}

func (c *countingEngine) BuildRevokePlan(context.Context, engine.Access) (*plan.Plan, error) {
	p := &plan.Plan{}
	p.Add(&countingStep{owner: c, sql: "REVOKE ALL", revoke: true})
	return p, nil
}

func (c *countingEngine) counts() (applied, revoked, plans int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applied, c.revoked, c.plans
}

type countingStep struct {
	owner  *countingEngine
	sql    string
	revoke bool
}

func (s *countingStep) Describe() string     { return s.sql }
func (s *countingStep) Rationale() string    { return "" }
func (s *countingStep) IsBestEffort() bool   { return false }
func (s *countingStep) Tolerates(error) bool { return false }
func (s *countingStep) Apply(context.Context) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if s.revoke {
		s.owner.revoked++
	} else {
		s.owner.applied++
	}
	return nil
}

func (c *countingEngine) factory() EngineFactory {
	return func(context.Context, *dbv1alpha1.DatabaseAccess) (engine.Engine, error) { return c, nil }
}

func observeMode(da *dbv1alpha1.DatabaseAccess) { da.Spec.Mode = dbv1alpha1.ModeObserve }

// The CRD must default mode to Enforce, so an existing manifest that predates
// the field keeps doing what it did.
func TestCRDDefaultsModeToEnforce(t *testing.T) {
	da := newAccess(t, uniqueName("mode-default"), nil)
	if got := get(t, da.Name).Spec.Mode; got != dbv1alpha1.ModeEnforce {
		t.Errorf("spec.mode = %q after create, want the API server to default it to %q", got, dbv1alpha1.ModeEnforce)
	}
}

// Observe plans and reports, and executes NOTHING.
func TestObserveReportsPendingAndAppliesNothing(t *testing.T) {
	c := &countingEngine{pending: 3}
	startManager(t, c.factory())

	da := newAccess(t, uniqueName("observe"), observeMode)

	eventually(t, 10*time.Second, "an Observed status", func() bool {
		cond := meta.FindStatusCondition(get(t, da.Name).Status.Conditions, ConditionReady)
		return cond != nil && cond.Reason == "Observed"
	})
	got := get(t, da.Name)

	if applied, _, _ := c.counts(); applied != 0 {
		t.Fatalf("Observe applied %d statement(s), want 0", applied)
	}
	if got.Status.PendingStatements != 3 || len(got.Status.Pending) != 3 {
		t.Errorf("pendingStatements=%d pending=%d, want 3 and 3", got.Status.PendingStatements, len(got.Status.Pending))
	}
	if got.Status.LastAppliedTime != nil {
		t.Errorf("lastAppliedTime = %v, want unset: nothing was applied", got.Status.LastAppliedTime)
	}
	if got.Status.LastPlannedTime == nil {
		t.Error("lastPlannedTime is unset, want the plan time recorded")
	}
	conv := meta.FindStatusCondition(got.Status.Conditions, ConditionConverged)
	if conv == nil || conv.Status != metav1.ConditionFalse || conv.Reason != "Pending" {
		t.Errorf("Converged = %+v, want False/Pending", conv)
	}
}

// An empty plan is the converged signal, in either mode.
func TestObserveReportsConvergedOnAnEmptyPlan(t *testing.T) {
	c := &countingEngine{pending: 0}
	startManager(t, c.factory())

	da := newAccess(t, uniqueName("observe-converged"), observeMode)

	eventually(t, 10*time.Second, "Converged=True", func() bool {
		cond := meta.FindStatusCondition(get(t, da.Name).Status.Conditions, ConditionConverged)
		return cond != nil && cond.Status == metav1.ConditionTrue
	})
	if got := get(t, da.Name).Status; got.PendingStatements != 0 || len(got.Pending) != 0 {
		t.Errorf("pendingStatements=%d pending=%v, want 0 and empty", got.PendingStatements, got.Pending)
	}
}

// The pending list is capped; the count is not.
func TestObserveCapsThePendingList(t *testing.T) {
	c := &countingEngine{pending: maxPending + 7}
	startManager(t, c.factory())

	da := newAccess(t, uniqueName("observe-cap"), observeMode)

	eventually(t, 10*time.Second, "the pending count", func() bool {
		return get(t, da.Name).Status.PendingStatements == maxPending+7
	})
	if n := len(get(t, da.Name).Status.Pending); n != maxPending {
		t.Errorf("len(status.pending) = %d, want the cap %d", n, maxPending)
	}
}

// Observe must not take the revoke finalizer, and deleting must not revoke --
// even with revokeOnDelete set.
func TestObserveNeverRevokes(t *testing.T) {
	c := &countingEngine{pending: 1}
	startManager(t, c.factory())

	da := newAccess(t, uniqueName("observe-revoke"), func(da *dbv1alpha1.DatabaseAccess) {
		observeMode(da)
		da.Spec.RevokeOnDelete = true
	})
	eventually(t, 10*time.Second, "an Observed status", func() bool {
		cond := meta.FindStatusCondition(get(t, da.Name).Status.Conditions, ConditionReady)
		return cond != nil && cond.Reason == "Observed"
	})
	if fin := get(t, da.Name).Finalizers; len(fin) != 0 {
		t.Fatalf("Observe with revokeOnDelete carries finalizers %v, want none", fin)
	}
	if err := k8sClient.Delete(context.Background(), da); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	eventually(t, 10*time.Second, "the resource to be gone", func() bool {
		var list dbv1alpha1.DatabaseAccessList
		if err := k8sClient.List(context.Background(), &list); err != nil {
			return false
		}
		for _, item := range list.Items {
			if item.Name == da.Name {
				return false
			}
		}
		return true
	})
	if _, revoked, _ := c.counts(); revoked != 0 {
		t.Errorf("deleting an Observe resource revoked %d time(s), want 0", revoked)
	}
}

// Enforce applies, then re-plans: pending reports what is STILL missing, not
// the size of the plan it just ran. countingEngine never converges, so after
// an apply the re-plan still has every statement pending -- the shape of a
// grant that keeps failing.
func TestEnforceRecordsPendingFromAFreshPlan(t *testing.T) {
	c := &countingEngine{pending: 2}
	startManager(t, c.factory())

	da := newAccess(t, uniqueName("enforce-replan"), nil)

	eventually(t, 10*time.Second, "an applied status with a plan time", func() bool {
		st := get(t, da.Name).Status
		return st.LastAppliedTime != nil && st.LastPlannedTime != nil
	})
	applied, _, plans := c.counts()
	if applied < 2 {
		t.Errorf("Enforce applied %d statement(s), want at least 2", applied)
	}
	if plans < 2 {
		t.Errorf("BuildPlan ran %d time(s), want the apply plan plus the re-plan", plans)
	}
	if got := get(t, da.Name).Status.PendingStatements; got != 2 {
		t.Errorf("pendingStatements = %d, want 2 from the re-plan", got)
	}
}
