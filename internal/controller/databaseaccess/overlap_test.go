// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package databaseaccess

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
)

func overlapAccess(name, endpoint, database, role string, revoke bool) dbv1alpha1.DatabaseAccess {
	return dbv1alpha1.DatabaseAccess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", UID: types.UID(name)},
		Spec: dbv1alpha1.DatabaseAccessSpec{
			Instance:       dbv1alpha1.InstanceRef{Endpoint: endpoint, Port: 5432, Database: database},
			Role:           role,
			RevokeOnDelete: revoke,
		},
	}
}

func TestOverlapsWarnsOnlyWhenARevokeWouldReachTheOther(t *testing.T) {
	const ep = "db.example.com"
	self := overlapAccess("self", ep, "appdb", "svc", true)
	deleting := overlapAccess("deleting", ep, "appdb", "svc", true)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	for _, tc := range []struct {
		self  dbv1alpha1.DatabaseAccess
		other dbv1alpha1.DatabaseAccess
		name  string
		want  bool
	}{
		{
			name:  "same role and database, self revokes",
			self:  self,
			other: overlapAccess("o", ep, "appdb", "svc", false),
			want:  true,
		},
		{
			name:  "same role and database, other revokes",
			self:  overlapAccess("s", ep, "appdb", "svc", false),
			other: overlapAccess("o", ep, "appdb", "svc", true),
			want:  true,
		},
		{
			name:  "endpoint differs only in case and trailing dot",
			self:  self,
			other: overlapAccess("o", "DB.example.com.", "appdb", "svc", false),
			want:  true,
		},
		{
			name:  "empty database defaults to appdb on both",
			self:  overlapAccess("s", ep, "", "svc", true),
			other: overlapAccess("o", ep, "appdb", "svc", false),
			want:  true,
		},
		{
			name:  "neither revokes",
			self:  overlapAccess("s", ep, "appdb", "svc", false),
			other: overlapAccess("o", ep, "appdb", "svc", false),
			want:  false,
		},
		{
			name:  "different database (the #27 layout)",
			self:  self,
			other: overlapAccess("o", ep, "otherdb", "svc", true),
			want:  false,
		},
		{
			name:  "different role",
			self:  self,
			other: overlapAccess("o", ep, "appdb", "other", true),
			want:  false,
		},
		{
			name:  "different server",
			self:  self,
			other: overlapAccess("o", "db2.example.com", "appdb", "svc", true),
			want:  false,
		},
		{
			name:  "other already being deleted",
			self:  self,
			other: deleting,
			want:  false,
		},
		{
			name:  "itself",
			self:  self,
			other: self,
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := overlaps(&tc.self, []dbv1alpha1.DatabaseAccess{tc.other})
			if (len(got) > 0) != tc.want {
				t.Fatalf("overlaps = %q, want a warning: %t", got, tc.want)
			}
			if tc.want && !strings.Contains(got[0], "team/"+tc.other.Name) {
				t.Errorf("warning %q does not name the other resource team/%s", got[0], tc.other.Name)
			}
		})
	}
}
