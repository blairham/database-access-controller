// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package databaseaccess

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Per-resource metrics, so that "a service lost database access" can page
// someone instead of waiting for a `kubectl get`. controller-runtime's own
// reconcile counters say the controller is failing somewhere; these say which
// resource, and whether it is still failing now.
//
// The label set is exactly {namespace, name}, deliberately:
//
//   - No `reason`. The Ready reason moves between Applied, AppliedWithWarnings
//     and the failure reasons, and a label that changes value mints a new
//     series every time it does -- the old one lingers until it goes stale, so
//     an alert on `== 0` can keep firing for a resource that has recovered.
//     The reason is one `kubectl describe` away; the alert only has to say
//     which resource to look at.
//   - No `role`. It is a spec field, so editing it would orphan the old series
//     the same way. namespace/name is the resource's identity and never
//     changes for the life of the object.
//
// Only the leader reconciles, so only the leader exports these. A standby
// reports no series at all rather than a stale copy, and after a failover the
// new leader repopulates every series on its initial list.
var (
	// accessLabels identify one DatabaseAccess; see above for why it is only
	// these two.
	accessLabels = []string{"namespace", "name"}

	readyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "database_controller_access_ready",
		Help: "1 when the DatabaseAccess's last reconcile succeeded (Ready=True: applied in Enforce, " +
			"planned in Observe), 0 otherwise.",
	}, accessLabels)

	warningsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "database_controller_access_warnings",
		Help: "Best-effort statements skipped on the last successful apply. Non-zero means future " +
			"objects may not inherit access (see status.warnings).",
	}, accessLabels)

	lastAppliedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "database_controller_access_last_applied_timestamp_seconds",
		Help: "Unix time of the last successful apply. Never moves in Observe mode; alert on " +
			"last_planned for liveness.",
	}, accessLabels)

	// lastPlannedGauge is the liveness signal, in both modes. Observe never
	// applies, so last_applied would read as stale forever for an Observe
	// resource that is in fact reconciling every hour; the database is read
	// and planned against on every successful reconcile in either mode.
	lastPlannedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "database_controller_access_last_planned_timestamp_seconds",
		Help: "Unix time the database was last read and planned against (status.lastPlannedTime). " +
			"With a one-hour drift interval, an age well past an hour means the resource has " +
			"stopped being reconciled.",
	}, accessLabels)

	// pendingGauge mirrors status.pendingStatements: what the database still
	// lacks. In Observe that is the plan not applied -- the number a cutover
	// is waiting to see reach 0. In Enforce it comes from the re-plan after
	// applying, so a non-zero value means a statement keeps failing or being
	// undone, not that the controller is behind.
	pendingGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "database_controller_access_pending_statements",
		Help: "Statements the database still needs to match the spec, from the last plan " +
			"(status.pendingStatements). 0 means converged.",
	}, accessLabels)
)

func init() {
	metrics.Registry.MustRegister(readyGauge, warningsGauge, lastAppliedGauge, lastPlannedGauge, pendingGauge)
}

// recordApplied publishes a successful apply.
func recordApplied(namespace, name string, warnings int, at time.Time) {
	readyGauge.WithLabelValues(namespace, name).Set(1)
	warningsGauge.WithLabelValues(namespace, name).Set(float64(warnings))
	lastAppliedGauge.WithLabelValues(namespace, name).Set(float64(at.Unix()))
}

// recordObserved publishes a successful Observe reconcile. The resource is
// Ready=True (reason Observed), so ready is 1: a NotReady alert must not fire
// on a resource that is reconciling fine and merely not writing. warnings is 0
// because Observe clears status.warnings -- nothing was applied, so nothing was
// skipped -- and the gauge mirrors the status. last_applied is left alone; it
// is history in this mode, and liveness comes from last_planned.
func recordObserved(namespace, name string) {
	readyGauge.WithLabelValues(namespace, name).Set(1)
	warningsGauge.WithLabelValues(namespace, name).Set(0)
}

// recordPlanned publishes a plan: when it was built and how much of it the
// database still needs. Called wherever status.lastPlannedTime and
// status.pendingStatements are set, so the two cannot disagree.
func recordPlanned(namespace, name string, pending int, at time.Time) {
	lastPlannedGauge.WithLabelValues(namespace, name).Set(float64(at.Unix()))
	pendingGauge.WithLabelValues(namespace, name).Set(float64(pending))
}

// recordFailed publishes a failed reconcile. The warnings count and the
// last-applied time are left as they were: they describe the last apply that
// worked, and the age of that apply is exactly what the stale alert measures.
func recordFailed(namespace, name string) {
	readyGauge.WithLabelValues(namespace, name).Set(0)
}

// forget removes every series for a resource that no longer exists. Without
// it a deleted DatabaseAccess keeps exporting its last value, and a resource
// deleted while failing would page forever.
func forget(namespace, name string) {
	readyGauge.DeleteLabelValues(namespace, name)
	warningsGauge.DeleteLabelValues(namespace, name)
	lastAppliedGauge.DeleteLabelValues(namespace, name)
	lastPlannedGauge.DeleteLabelValues(namespace, name)
	pendingGauge.DeleteLabelValues(namespace, name)
}
