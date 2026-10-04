// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package databaseaccess

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Per-resource metrics, so an alert can name the resource that lost access.
//
// Labels are only {namespace, name}: a label whose value changes (a reason, the
// role) would leave a stale series behind that keeps an alert firing. Only the
// leader reconciles, so only the leader exports these.
var (
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

	// lastPlannedGauge is the liveness signal in both modes; last_applied
	// never moves in Observe.
	lastPlannedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "database_controller_access_last_planned_timestamp_seconds",
		Help: "Unix time the database was last read and planned against (status.lastPlannedTime). " +
			"With a one-hour drift interval, an age well past an hour means the resource has " +
			"stopped being reconciled.",
	}, accessLabels)

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

// recordObserved publishes a successful Observe reconcile: Ready, no warnings,
// last_applied untouched.
func recordObserved(namespace, name string) {
	readyGauge.WithLabelValues(namespace, name).Set(1)
	warningsGauge.WithLabelValues(namespace, name).Set(0)
}

// recordPlanned publishes a plan's time and pending count, alongside the
// matching status fields.
func recordPlanned(namespace, name string, pending int, at time.Time) {
	lastPlannedGauge.WithLabelValues(namespace, name).Set(float64(at.Unix()))
	pendingGauge.WithLabelValues(namespace, name).Set(float64(pending))
}

// recordFailed publishes a failed reconcile. warnings and last_applied keep
// describing the last apply that worked.
func recordFailed(namespace, name string) {
	readyGauge.WithLabelValues(namespace, name).Set(0)
}

// forget removes every series for a resource that no longer exists.
func forget(namespace, name string) {
	readyGauge.DeleteLabelValues(namespace, name)
	warningsGauge.DeleteLabelValues(namespace, name)
	lastAppliedGauge.DeleteLabelValues(namespace, name)
	lastPlannedGauge.DeleteLabelValues(namespace, name)
	pendingGauge.DeleteLabelValues(namespace, name)
}
