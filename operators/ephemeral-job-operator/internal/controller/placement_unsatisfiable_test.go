package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// A pod whose selector matches no node is not waiting for capacity.
//
// It reported WaitingForCapacity for three and a half hours, which reads as a
// full cluster and sends an operator looking for nodes. Nothing aged it out
// either: readinessDeadlineSeconds starts when a pod RUNS and idleTimeoutSeconds
// when the workload first SERVES, so neither clock had begun.
func unschedulablePod(sel map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Labels: map[string]string{"job-name": "j"}},
		Spec:       corev1.PodSpec{NodeSelector: sel},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  corev1.PodReasonUnschedulable,
				Message: "0/3 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.",
			}},
		},
	}
}

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func assess(t *testing.T, pod *corev1.Pod, nodes ...*corev1.Node) CapacityState {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = computev1alpha1.AddToScheme(s)
	objs := []runtime.Object{pod}
	for _, n := range nodes {
		objs = append(objs, n)
	}
	// The same field index the manager registers. Without it the event read
	// fails, AssessCapacityBySelector takes its best-effort early return, and
	// the test would exercise a path production never uses.
	c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		}).Build()
	got, err := AssessCapacityBySelector(context.Background(), c, c, "ns", map[string]string{"job-name": "j"})
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	return got
}

func TestSelectorMatchingNoNodeIsUnsatisfiableNotWaiting(t *testing.T) {
	got := assess(t,
		unschedulablePod(map[string]string{"workload-location": "home"}),
		node("a", map[string]string{"workload-location": "on-prem"}),
		node("b", map[string]string{"workload-location": "hetzner"}),
	)
	if got.Reason != computev1alpha1.ReasonPlacementUnsatisfiable {
		t.Errorf("reason = %q, want %q — no node carries workload-location=home and none is coming",
			got.Reason, computev1alpha1.ReasonPlacementUnsatisfiable)
	}
	if !got.Terminal {
		t.Error("must be terminal: waiting cannot make a label appear")
	}
	if got.Message == "" {
		t.Error("the scheduler's own message must be kept; it names the mismatch")
	}
}

// The safe direction. A selector that DOES match somewhere may still be Pending
// for taints, resources or topology — all of which resolve — so it must keep
// reporting WaitingForCapacity and must never be failed.
func TestSelectorMatchingSomeNodeKeepsWaiting(t *testing.T) {
	got := assess(t,
		unschedulablePod(map[string]string{"workload-location": "on-prem"}),
		node("a", map[string]string{"workload-location": "on-prem"}),
	)
	if got.Reason != computev1alpha1.ReasonWaitingForCapacity {
		t.Errorf("reason = %q, want WaitingForCapacity — the label exists, so this is resources or taints", got.Reason)
	}
	if got.Terminal {
		t.Error("must not be terminal: a matching node exists and the pod may yet schedule")
	}
}

// No selector at all cannot be unsatisfiable by this test, whatever else is wrong.
func TestPodWithoutSelectorIsNeverUnsatisfiable(t *testing.T) {
	got := assess(t, unschedulablePod(nil), node("a", map[string]string{"x": "y"}))
	if got.Reason == computev1alpha1.ReasonPlacementUnsatisfiable {
		t.Error("a pod with no nodeSelector has no selector to be unsatisfiable")
	}
}
