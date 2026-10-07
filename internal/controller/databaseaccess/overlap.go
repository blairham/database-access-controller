// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package databaseaccess

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
)

// ReasonOverlappingAccess is the Warning event raised when another resource
// declares the same role on the same database and either side revokes on
// delete (#31).
const ReasonOverlappingAccess = "OverlappingAccess"

// accessKey is what makes two resources act on the same grants: the role is
// server-wide, grants are per database.
type accessKey struct {
	endpoint string
	database string
	role     string
	port     int32
}

func keyOf(da *dbv1alpha1.DatabaseAccess) accessKey {
	a := Access(da.Spec)
	return accessKey{
		endpoint: strings.TrimSuffix(strings.ToLower(da.Spec.Instance.Endpoint), "."),
		port:     da.Spec.Instance.Port,
		database: a.Database,
		role:     a.Principal,
	}
}

// overlaps returns the warning for each live resource in others that shares
// da's role and database where a revokeOnDelete on either side would withdraw
// grants the other still declares. Revocation does not know about other
// resources, so this is a warning, not a guard; and two endpoints that reach
// one server (a cluster and an instance endpoint) are not recognized as one.
func overlaps(da *dbv1alpha1.DatabaseAccess, others []dbv1alpha1.DatabaseAccess) []string {
	key := keyOf(da)
	var out []string
	for i := range others {
		o := &others[i]
		if o.UID == da.UID || !o.DeletionTimestamp.IsZero() || keyOf(o) != key {
			continue
		}
		if !da.Spec.RevokeOnDelete && !o.Spec.RevokeOnDelete {
			continue
		}
		var who string
		switch {
		case da.Spec.RevokeOnDelete && o.Spec.RevokeOnDelete:
			who = "deleting either revokes"
		case da.Spec.RevokeOnDelete:
			who = "deleting this resource (revokeOnDelete) revokes"
		default:
			who = "deleting " + o.Namespace + "/" + o.Name + " (revokeOnDelete) revokes"
		}
		out = append(out, fmt.Sprintf(
			"role %q on database %q is also declared by %s/%s; %s the database grants, and any shared schema's, "+
				"that the other still declares until its next reconcile. Use one DatabaseAccess per role per database",
			key.role, key.database, o.Namespace, o.Name, who,
		))
	}
	return out
}

// warnOverlaps records an OverlappingAccess event for each overlap. A failed
// list only skips the warning; it never fails the reconcile.
func (r *Reconciler) warnOverlaps(ctx context.Context, da *dbv1alpha1.DatabaseAccess) {
	if r.Recorder == nil {
		return
	}
	var all dbv1alpha1.DatabaseAccessList
	if err := r.List(ctx, &all); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing DatabaseAccess resources to check for overlaps")
		return
	}
	for _, msg := range overlaps(da, all.Items) {
		r.Recorder.Event(da, corev1.EventTypeWarning, ReasonOverlappingAccess, msg)
	}
}
