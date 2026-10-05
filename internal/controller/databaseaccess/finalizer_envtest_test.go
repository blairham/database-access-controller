// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package databaseaccess

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
)

const otherFinalizer = "example.com/other"

// racingClient runs race once, immediately before the reconciler's first
// non-status write to a DatabaseAccess. That is exactly the window stg hit: the
// object changes between the reconciler's read and its finalizer write. It
// fires on the write itself, not on a timer, so the conflict happens on every
// run.
type racingClient struct {
	client.Client
	fired atomic.Bool
	race  func(ctx context.Context, key types.NamespacedName)
}

func (c *racingClient) maybeRace(ctx context.Context, obj client.Object) {
	if _, ok := obj.(*dbv1alpha1.DatabaseAccess); ok && c.fired.CompareAndSwap(false, true) {
		c.race(ctx, client.ObjectKeyFromObject(obj))
	}
}

func (c *racingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.maybeRace(ctx, obj)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *racingClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	c.maybeRace(ctx, obj)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// concurrently writes to the object through the test's own client, the way
// another controller or a status update would.
func concurrently(t *testing.T, mutate func(*dbv1alpha1.DatabaseAccess)) func(context.Context, types.NamespacedName) {
	return func(ctx context.Context, key types.NamespacedName) {
		var cur dbv1alpha1.DatabaseAccess
		if err := k8sClient.Get(ctx, key, &cur); err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
		mutate(&cur)
		if err := k8sClient.Update(ctx, &cur); err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
}

func bumpAnnotation(da *dbv1alpha1.DatabaseAccess) {
	if da.Annotations == nil {
		da.Annotations = map[string]string{}
	}
	da.Annotations["example.com/touched"] = "true"
}

// reconcileOnce runs one reconcile with a racing client and no manager, so
// the test sees exactly what a single pass does: no work-queue retry hides a
// failure behind a second attempt.
func reconcileOnce(t *testing.T, f *fakeEngine, rc *racingClient, name string) error {
	t.Helper()
	r := &Reconciler{
		Client:    rc,
		Scheme:    testSch,
		Recorder:  record.NewFakeRecorder(100),
		NewEngine: f.factory(),
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
	return err
}

// releaseAll lets the object go at cleanup whatever finalizers it was left with.
func releaseAll(t *testing.T, name string) {
	t.Cleanup(func() {
		var cur dbv1alpha1.DatabaseAccess
		key := client.ObjectKey{Name: name, Namespace: "default"}
		if err := k8sClient.Get(context.Background(), key, &cur); err != nil {
			return
		}
		cur.Finalizers = nil
		_ = k8sClient.Update(context.Background(), &cur)
	})
}

// The stg failure: a write lands while the revoke runs, so the finalizer
// removal used to hit "the object has been modified" and the whole delete,
// revoke included, went back to the queue. One pass must now finish it, and
// revoke once.
func TestFinalizerReleaseSurvivesAConcurrentWrite(t *testing.T) {
	f := &fakeEngine{}
	name := uniqueName("release-race")
	da := newAccess(t, name, func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.RevokeOnDelete = true
		da.Finalizers = []string{finalizer}
	})
	releaseAll(t, name)
	if err := k8sClient.Delete(context.Background(), da); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	rc := &racingClient{Client: k8sClient, race: concurrently(t, bumpAnnotation)}
	if err := reconcileOnce(t, f, rc, name); err != nil {
		t.Fatalf("a concurrent write failed the delete reconcile: %v", err)
	}
	if !rc.fired.Load() {
		t.Fatal("the race never fired, so this test proved nothing")
	}

	var out dbv1alpha1.DatabaseAccess
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &out)
	if !apierrors.IsNotFound(err) {
		t.Errorf("resource still present after one pass (finalizers %v, err %v)", out.Finalizers, err)
	}
	if f.revoked != 1 {
		t.Errorf("revoke plan ran %d time(s), want exactly 1", f.revoked)
	}
}

// Another controller's finalizer that was already there must survive ours
// being removed under a concurrent write.
func TestFinalizerReleaseKeepsAnotherControllersFinalizer(t *testing.T) {
	f := &fakeEngine{}
	name := uniqueName("release-keeps-other")
	da := newAccess(t, name, func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.RevokeOnDelete = true
		da.Finalizers = []string{finalizer, otherFinalizer}
	})
	releaseAll(t, name)
	if err := k8sClient.Delete(context.Background(), da); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	rc := &racingClient{Client: k8sClient, race: concurrently(t, bumpAnnotation)}
	if err := reconcileOnce(t, f, rc, name); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	got := get(t, name)
	if slices.Contains(got.Finalizers, finalizer) {
		t.Errorf("our finalizer is still present: %v", got.Finalizers)
	}
	if !slices.Contains(got.Finalizers, otherFinalizer) {
		t.Errorf("another controller's finalizer was dropped: %v", got.Finalizers)
	}
}

// The case a lock-free merge patch gets wrong. The API forbids ADDING a
// finalizer to an object being deleted, so the concurrent change that matters
// is another controller REMOVING its own. A patch built from our stale list
// would write that finalizer back. With the lock the patch conflicts,
// re-reads, and leaves it removed, and the object is released.
func TestFinalizerReleaseDoesNotResurrectARemovedFinalizer(t *testing.T) {
	f := &fakeEngine{}
	name := uniqueName("release-no-resurrect")
	da := newAccess(t, name, func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.RevokeOnDelete = true
		da.Finalizers = []string{finalizer, otherFinalizer}
	})
	releaseAll(t, name)
	if err := k8sClient.Delete(context.Background(), da); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	rc := &racingClient{Client: k8sClient, race: concurrently(t, func(cur *dbv1alpha1.DatabaseAccess) {
		cur.Finalizers = slices.DeleteFunc(cur.Finalizers, func(f string) bool { return f == otherFinalizer })
	})}
	if err := reconcileOnce(t, f, rc, name); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	var out dbv1alpha1.DatabaseAccess
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &out)
	if !apierrors.IsNotFound(err) {
		t.Errorf("resource still present with finalizers %v (err %v): a removed finalizer came back", out.Finalizers, err)
	}
}

// The add path has the same window, and the status write after it must not
// conflict either: setFinalizer carries the new resourceVersion forward.
func TestFinalizerAddSurvivesAConcurrentWrite(t *testing.T) {
	f := &fakeEngine{}
	name := uniqueName("add-race")
	newAccess(t, name, func(da *dbv1alpha1.DatabaseAccess) {
		da.Spec.RevokeOnDelete = true
	})
	releaseAll(t, name)

	rc := &racingClient{Client: k8sClient, race: concurrently(t, bumpAnnotation)}
	if err := reconcileOnce(t, f, rc, name); err != nil {
		t.Fatalf("a concurrent write failed the reconcile: %v", err)
	}
	if !rc.fired.Load() {
		t.Fatal("the race never fired, so this test proved nothing")
	}

	got := get(t, name)
	if !slices.Contains(got.Finalizers, finalizer) {
		t.Errorf("finalizer not added: %v", got.Finalizers)
	}
	if got.Annotations["example.com/touched"] != "true" {
		t.Errorf("the concurrent write was lost: annotations %v", got.Annotations)
	}
	cond := findCondition(got.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True: the status write after the add must not conflict", cond)
	}
}

func findCondition(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}
