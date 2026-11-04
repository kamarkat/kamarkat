package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(computev1alpha1.AddToScheme(s))
	return s
}

func completedJob(t *testing.T, s *runtime.Scheme, name string, owner *computev1alpha1.EphemeralJob) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: batchv1.JobStatus{
			Succeeded:  1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		},
	}
	if owner != nil {
		if err := ctrl.SetControllerReference(owner, job, s); err != nil {
			t.Fatalf("SetControllerReference: %v", err)
		}
	}
	return job
}

// A finished Job belonging to THIS request must be returned, never replaced.
//
// The replacement path exists for a new EphemeralJob that finds a PREVIOUS
// request's Job under the same derived name. It cannot tell them apart by name
// — the name is derived from the request — so it goes by ownership.
//
// Without that distinction, a status update that loses an optimistic-concurrency
// race leaves the CR non-terminal while its own Job is already Complete. The
// next pass then deletes that Job and runs the work again, and since the race
// recurs, again after that. It cost a completed 20-scene render: video.mp4 was
// uploaded, then re-rendered from scratch twice while the workflow that
// submitted it waited for a callback the deleted Job could no longer send.
//
// The expensive part is invisible from the operator's side — it just sees a Job
// — so this is asserted rather than left to review.
func TestEnsureJobKeepsOwnFinishedJob(t *testing.T) {
	s := testScheme(t)
	ej := &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ej-render", Namespace: "default", UID: "owner-uid-1",
		},
	}
	name := jobNameFor(ej)
	job := completedJob(t, s, name, ej)

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ej, job).Build()
	r := &EphemeralJobReconciler{Client: c, Scheme: s}

	got, err := r.ensureJob(context.Background(), ej)
	if err != nil {
		t.Fatalf("ensureJob returned error: %v", err)
	}
	if got == nil {
		t.Fatal("ensureJob deleted this request's own completed Job and asked for a replacement; " +
			"the work would be re-run and its callback lost")
	}
	if got.Name != name {
		t.Fatalf("got Job %q, want %q", got.Name, name)
	}

	// And it must still be there — not queued for deletion.
	var still batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &still); err != nil {
		t.Fatalf("own finished Job was deleted: %v", err)
	}
}

// A finished Job left by a DIFFERENT request is still replaced, so the guard
// above does not resurrect the bug it was added beside: adopting a Job that has
// already exited hands back compute that cannot do the work.
func TestEnsureJobReplacesForeignFinishedJob(t *testing.T) {
	s := testScheme(t)
	previous := &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{Name: "ej-render", Namespace: "default", UID: "owner-uid-OLD"},
	}
	current := &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{Name: "ej-render", Namespace: "default", UID: "owner-uid-NEW"},
	}
	name := jobNameFor(current)
	job := completedJob(t, s, name, previous)

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(current, job).Build()
	r := &EphemeralJobReconciler{Client: c, Scheme: s}

	got, err := r.ensureJob(context.Background(), current)
	if err != nil {
		t.Fatalf("ensureJob returned error: %v", err)
	}
	if got != nil {
		t.Fatal("a finished Job from an earlier request was adopted; it has already exited and " +
			"cannot run this request's work")
	}
}

// A Job still running is returned untouched whoever owns it — the idempotency
// the replacement logic was careful to preserve.
func TestEnsureJobReusesRunningJob(t *testing.T) {
	s := testScheme(t)
	ej := &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{Name: "ej-render", Namespace: "default", UID: "owner-uid-1"},
	}
	name := jobNameFor(ej)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	if err := ctrl.SetControllerReference(ej, job, s); err != nil {
		t.Fatalf("SetControllerReference: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ej, job).Build()
	r := &EphemeralJobReconciler{Client: c, Scheme: s}

	got, err := r.ensureJob(context.Background(), ej)
	if err != nil {
		t.Fatalf("ensureJob returned error: %v", err)
	}
	if got == nil || got.Name != name {
		t.Fatal("a running Job was not reused; a second Job would be created for the same work")
	}
}
