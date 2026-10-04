// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package databaseaccess

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderRules renders the chart's PrometheusRule with the given extra --set
// flags and returns the manifest text.
func renderRules(t *testing.T, sets ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "..", "charts", "database-controller"))
	if err != nil {
		t.Fatal(err)
	}
	args := make([]string, 0, 7+2*len(sets))
	args = append(args,
		"template", "test", chart,
		"--show-only", "templates/prometheusrule.yaml",
		"--set", "prometheusRule.enabled=true",
	)
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("rendering the PrometheusRule: %v\n%s", err, out)
	}
	return string(out)
}

// alertExpr returns the expr line of the named alert, or "" if it is absent.
func alertExpr(rules, alert string) string {
	_, after, found := strings.Cut(rules, "- alert: "+alert+"\n")
	if !found {
		return ""
	}
	for _, line := range strings.Split(after, "\n") {
		if expr, ok := strings.CutPrefix(strings.TrimSpace(line), "expr: "); ok {
			return expr
		}
	}
	return ""
}

// The stale alert is liveness for every resource, and Observe resources never
// apply. Keyed on last_applied it would fire on every Observe resource two
// hours after it was created while the controller reconciles it hourly.
func TestStaleAlertKeysOnLastPlanned(t *testing.T) {
	expr := alertExpr(renderRules(t), "DatabaseAccessStale")
	if expr == "" {
		t.Fatal("DatabaseAccessStale is not rendered with prometheusRule.enabled=true")
	}
	if !strings.Contains(expr, "database_controller_access_last_planned_timestamp_seconds") {
		t.Errorf("DatabaseAccessStale expr = %q, want it keyed on last_planned", expr)
	}
	if strings.Contains(expr, "last_applied") {
		t.Errorf("DatabaseAccessStale expr = %q still reads last_applied, which never moves in Observe", expr)
	}
}

// NotConverged is opt-in: in Observe a non-zero pending count is expected until
// the cutover, so it must not render unless asked for -- and when asked for, it
// must read the pending gauge.
func TestNotConvergedAlertIsOptIn(t *testing.T) {
	if expr := alertExpr(renderRules(t), "DatabaseAccessNotConverged"); expr != "" {
		t.Errorf("DatabaseAccessNotConverged rendered by default (expr %q), want it off", expr)
	}
	expr := alertExpr(renderRules(t, "prometheusRule.rules.notConverged.enabled=true"), "DatabaseAccessNotConverged")
	if !strings.Contains(expr, "database_controller_access_pending_statements") || !strings.Contains(expr, "> 0") {
		t.Errorf("DatabaseAccessNotConverged expr = %q, want pending_statements > 0", expr)
	}
}
