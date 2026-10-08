// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package databaseaccess

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/blairham/database-access-controller/apis/db/v1alpha1"
)

func overlapEvents(t *testing.T, name string) int {
	t.Helper()
	var evs corev1.EventList
	if err := k8sClient.List(context.Background(), &evs, client.InNamespace("default")); err != nil {
		t.Fatalf("listing events: %v", err)
	}
	n := 0
	for _, e := range evs.Items {
		if e.InvolvedObject.Name == name && e.Reason == ReasonOverlappingAccess && e.Type == corev1.EventTypeWarning {
			n++
		}
	}
	return n
}

// Two resources for one role on one database, one revoking on delete, each get
// an OverlappingAccess warning (#31); the same role on another database does
// not.
func TestOverlappingAccessIsWarnedOnBothSides(t *testing.T) {
	f := &fakeEngine{}
	startManager(t, f.factory())

	revoking := newAccess(t, "overlap-revoking", func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.Role = "overlap_role"
		da.Spec.RevokeOnDelete = true
	})
	keeping := newAccess(t, "overlap-keeping", func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.Role = "overlap_role"
	})
	elsewhere := newAccess(t, "overlap-elsewhere", func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.Role = "overlap_role"
		da.Spec.Instance.Database = "otherdb"
		da.Spec.RevokeOnDelete = true
	})

	for _, da := range []*dbv1alpha1.DatabaseAccess{revoking, keeping} {
		eventually(t, 10*time.Second, "OverlappingAccess on "+da.Name, func() bool {
			return overlapEvents(t, da.Name) > 0
		})
	}
	eventually(t, 10*time.Second, "Ready on "+elsewhere.Name, func() bool {
		for _, c := range get(t, elsewhere.Name).Status.Conditions {
			if c.Type == ConditionReady {
				return true
			}
		}
		return false
	})
	if n := overlapEvents(t, elsewhere.Name); n != 0 {
		t.Errorf("%s, on a different database, got %d OverlappingAccess events, want none", elsewhere.Name, n)
	}
}
