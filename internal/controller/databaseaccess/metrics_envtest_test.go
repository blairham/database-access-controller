// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package databaseaccess

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/engine"
)

// metricsRun makes every resource name in this file unique to one test run.
// The gauges are process-global and a test's manager stops before its cleanup
// deletes the resource, so nothing forgets the series; under -count=N a reused
// name would let run 2 pass on the value run 1 left behind -- a test satisfied
// by stale state rather than by its own reconcile.
var metricsRun atomic.Int64

func uniqueName(base string) string {
	return fmt.Sprintf("%s-%d", base, metricsRun.Add(1))
}

// series reads one resource's value off a vector without creating it.
// WithLabelValues would mint the series it is asked about, which turns every
// "is it gone?" assertion into a pass.
func series(t *testing.T, vec *prometheus.GaugeVec, name string) (float64, bool) {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() { vec.Collect(ch); close(ch) }()
	var (
		value float64
		found bool
	)
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("reading metric: %v", err)
		}
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["namespace"] == "default" && labels["name"] == name {
			value, found = pb.GetGauge().GetValue(), true
		}
	}
	return value, found
}

func waitForSeries(t *testing.T, vec *prometheus.GaugeVec, name string, want float64) {
	t.Helper()
	eventually(t, 10*time.Second, name+" series to read "+fmt.Sprint(want), func() bool {
		v, ok := series(t, vec, name)
		return ok && v == want
	})
}

// A clean apply is what the alert treats as healthy. It must read 1, report no
// skipped statements, and stamp a last-applied time from this reconcile.
func TestMetricsReportAReadyResource(t *testing.T) {
	f := &fakeEngine{}
	startManager(t, f.factory())
	before := time.Now().Add(-time.Second)

	da := newAccess(t, uniqueName("metrics-ready"), nil)

	waitForSeries(t, readyGauge, da.Name, 1)
	if v, ok := series(t, warningsGauge, da.Name); !ok || v != 0 {
		t.Errorf("warnings series = %v (present %v), want 0", v, ok)
	}
	at, ok := series(t, lastAppliedGauge, da.Name)
	if !ok {
		t.Fatal("last-applied series is missing after a successful apply")
	}
	if got := time.Unix(int64(at), 0); got.Before(before.Truncate(time.Second)) {
		t.Errorf("last-applied = %v, want a time from this reconcile (after %v)", got, before)
	}
}

// A connection failure is the case the alert exists for. It has to read 0, and
// it must not invent a last-applied time for an apply that never happened.
func TestMetricsReportAConnectionFailure(t *testing.T) {
	startManager(t, func(context.Context, *dbv1alpha1.DatabaseAccess) (engine.Engine, error) {
		return nil, errors.New("dial tcp 10.0.0.1:5432: i/o timeout")
	})

	da := newAccess(t, uniqueName("metrics-conn-fail"), nil)

	waitForSeries(t, readyGauge, da.Name, 0)
	eventually(t, 10*time.Second, "Ready=False/ConnectionFailed on the resource", func() bool {
		for _, c := range get(t, da.Name).Status.Conditions {
			if c.Type == ConditionReady && c.Reason == "ConnectionFailed" {
				return true
			}
		}
		return false
	})
	if v, ok := series(t, lastAppliedGauge, da.Name); ok {
		t.Errorf("last-applied series = %v for a resource that never applied, want none", v)
	}
}

// Skipped best-effort statements still count as Ready -- access was granted --
// but the count is the early warning that the next DROP/CREATE of an object
// may silently revoke it again.
func TestMetricsCountSkippedStatements(t *testing.T) {
	f := &fakeEngine{warnings: []string{"permission denied to set role"}}
	startManager(t, f.factory())

	da := newAccess(t, uniqueName("metrics-warnings"), nil)

	waitForSeries(t, readyGauge, da.Name, 1)
	waitForSeries(t, warningsGauge, da.Name, 1)
}

// Recovery must clear the alert on the same series. A reason label would have
// left the failing series behind, still reading 0.
func TestMetricsRecoverOnTheSameSeries(t *testing.T) {
	var healthy atomic.Bool
	f := &fakeEngine{}
	startManager(t, func(ctx context.Context, da *dbv1alpha1.DatabaseAccess) (engine.Engine, error) {
		if !healthy.Load() {
			return nil, errors.New("connection refused")
		}
		return f, nil
	})

	da := newAccess(t, uniqueName("metrics-recovers"), nil)
	waitForSeries(t, readyGauge, da.Name, 0)

	healthy.Store(true)
	waitForSeries(t, readyGauge, da.Name, 1)
}

// A deleted resource must stop exporting. Otherwise one deleted while failing
// pages forever. Both deletion paths are covered: no finalizer (the object
// simply disappears) and revokeOnDelete (the finalizer is released by us).
func TestMetricsAreRemovedWithTheResource(t *testing.T) {
	f := &fakeEngine{}
	startManager(t, f.factory())

	plain := newAccess(t, uniqueName("metrics-delete-plain"), nil)
	revoking := newAccess(t, uniqueName("metrics-delete-revoke"), func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.RevokeOnDelete = true
	})

	for _, da := range []*dbv1alpha1.DatabaseAccess{plain, revoking} {
		waitForSeries(t, readyGauge, da.Name, 1)
		if err := k8sClient.Delete(context.Background(), da); err != nil {
			t.Fatalf("deleting %s: %v", da.Name, err)
		}
	}

	for _, da := range []*dbv1alpha1.DatabaseAccess{plain, revoking} {
		eventually(t, 10*time.Second, da.Name+" series to be removed", func() bool {
			_, ready := series(t, readyGauge, da.Name)
			_, warnings := series(t, warningsGauge, da.Name)
			_, applied := series(t, lastAppliedGauge, da.Name)
			_, planned := series(t, lastPlannedGauge, da.Name)
			_, pending := series(t, pendingGauge, da.Name)
			return !ready && !warnings && !applied && !planned && !pending
		})
	}
}

// An Observe resource is Ready=True (reason Observed) and reconciling every
// hour while writing nothing. Its ready series must read 1, or NotReady pages
// for every resource in a cutover; its liveness comes from last_planned,
// because it never applies and so must not grow a last_applied series; and
// pending must carry the plan size, which is what a cutover is waiting on.
func TestMetricsReportAnObservedResource(t *testing.T) {
	c := &countingEngine{pending: 3}
	startManager(t, c.factory())
	before := time.Now().Add(-time.Second)

	da := newAccess(t, uniqueName("metrics-observe"), observeMode)

	waitForSeries(t, readyGauge, da.Name, 1)
	waitForSeries(t, pendingGauge, da.Name, 3)
	if v, ok := series(t, warningsGauge, da.Name); !ok || v != 0 {
		t.Errorf("warnings series = %v (present %v), want 0: Observe clears status.warnings", v, ok)
	}
	at, ok := series(t, lastPlannedGauge, da.Name)
	if !ok {
		t.Fatal("last-planned series is missing for an Observe resource; the stale alert would fire on it")
	}
	if got := time.Unix(int64(at), 0); got.Before(before.Truncate(time.Second)) {
		t.Errorf("last-planned = %v, want a time from this reconcile (after %v)", got, before)
	}
	if v, ok := series(t, lastAppliedGauge, da.Name); ok {
		t.Errorf("last-applied series = %v for an Observe resource that never applied, want none", v)
	}
	if applied, _, _ := c.counts(); applied != 0 {
		t.Fatalf("Observe applied %d statement(s), want 0", applied)
	}
}

// In Enforce the pending gauge comes from the re-plan after applying and must
// equal the status it mirrors. The fake engine plans one step every time, so
// the re-plan reports 1 still pending -- the shape of a statement that keeps
// being undone -- and last_planned must be stamped in this mode too.
func TestMetricsPendingMatchesStatusInEnforce(t *testing.T) {
	f := &fakeEngine{}
	startManager(t, f.factory())

	da := newAccess(t, uniqueName("metrics-enforce-pending"), nil)

	waitForSeries(t, readyGauge, da.Name, 1)
	var status int
	eventually(t, 10*time.Second, "status.pendingStatements to be recorded", func() bool {
		got := get(t, da.Name)
		status = got.Status.PendingStatements
		return got.Status.LastPlannedTime != nil
	})
	waitForSeries(t, pendingGauge, da.Name, float64(status))
	if _, ok := series(t, lastPlannedGauge, da.Name); !ok {
		t.Error("last-planned series is missing after an Enforce reconcile")
	}
}
