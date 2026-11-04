package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

func boundsTestReconciler(t *testing.T, initObjs ...client.Object) (*EphemeralJobReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(computev1alpha1.AddToScheme(s))

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(initObjs...).
		WithStatusSubresource(&computev1alpha1.EphemeralJob{}, &corev1.Pod{}).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		}).Build()

	return &EphemeralJobReconciler{
		Client:             c,
		Scheme:             s,
		APIReader:          c,
		ProvisioningBudget: 6 * time.Minute,
	}, c
}

func burstNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				WorkloadLocationKey: WorkloadLocationValue,
			},
		},
	}
}

func serviceJobFixture(name, uid string, age time.Duration, maxLifetime int32) *computev1alpha1.EphemeralJob {
	created := metav1.NewTime(time.Now().Add(-age))
	return &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "test-ns",
			UID:               types.UID(uid),
			CreationTimestamp: created,
		},
		Spec: computev1alpha1.EphemeralJobSpec{
			Mode:                    computev1alpha1.ModeService,
			PlacementClass:          "burst",
			IdleTimeoutSeconds:      300,
			MaxLifetimeSeconds:      maxLifetime,
			TTLSecondsAfterFinished: 3600,
		},
	}
}

// ── Task A: Lifetime Bound ───────────────────────────────────────────────────

// 1. Waiting service job is finished at its lifetime bound.
func TestServiceJobFinishedAtLifetimeBound(t *testing.T) {
	// Job created 10 hours ago with 8-hour maxLifetimeSeconds.
	// Capacity is not ready (pod is pending).
	ej := serviceJobFixture("sandbox-lifetime-expired", "uid-lt-1", 10*time.Hour, 28800)
	node := burstNode("burst-node-1")

	// Pre-create the pending pod owned by this EphemeralJob.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-lifetime-expired-pod",
			Namespace: ej.Namespace,
			Labels: map[string]string{
				labelJobUID: string(ej.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, c := boundsTestReconciler(t, ej, node, pod)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("expected terminal result without requeue, got: %+v", res)
	}

	var updated computev1alpha1.EphemeralJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name}, &updated); err != nil {
		t.Fatalf("Get EphemeralJob: %v", err)
	}

	if updated.Status.Phase != computev1alpha1.PhaseSucceeded {
		t.Fatalf("Phase = %q, want %q", updated.Status.Phase, computev1alpha1.PhaseSucceeded)
	}
	if updated.Status.CompletionTime == nil {
		t.Fatal("CompletionTime is nil, want set for TTL reaping")
	}
	if !strings.Contains(updated.Status.Message, "reached the 28800s maximum lifetime") {
		t.Fatalf("Message = %q, want to contain maximum lifetime notice", updated.Status.Message)
	}

	condComplete := meta_FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionComplete)
	if condComplete == nil || condComplete.Status != metav1.ConditionTrue {
		t.Fatalf("Condition Complete = %+v, want Status=True", condComplete)
	}

	// Verify pod was cleaned up upon reaching lifetime bound.
	var remainingPod corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &remainingPod)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("pod was not deleted at lifetime bound: err = %v", err)
	}
}

// 2. Waiting service job inside its lifetime keeps waiting.
func TestServiceJobInsideLifetimeKeepsWaiting(t *testing.T) {
	// Job created 1 hour ago with 8-hour maxLifetimeSeconds.
	ej := serviceJobFixture("sandbox-lifetime-active", "uid-lt-2", 1*time.Hour, 28800)
	node := burstNode("burst-node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-lifetime-active-pod",
			Namespace: ej.Namespace,
			Labels: map[string]string{
				labelJobUID: string(ej.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, c := boundsTestReconciler(t, ej, node, pod)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected RequeueAfter > 0 while waiting for capacity, got: %+v", res)
	}

	var updated computev1alpha1.EphemeralJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name}, &updated); err != nil {
		t.Fatalf("Get EphemeralJob: %v", err)
	}

	if updated.Status.Phase != computev1alpha1.PhaseProvisioning {
		t.Fatalf("Phase = %q, want %q", updated.Status.Phase, computev1alpha1.PhaseProvisioning)
	}
	if updated.Status.CompletionTime != nil {
		t.Fatal("CompletionTime is set, but job should still be waiting")
	}

	condCap := meta_FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionCapacity)
	if condCap == nil || condCap.Reason != computev1alpha1.ReasonWaitingForCapacity {
		t.Fatalf("Condition Capacity = %+v, want Reason=WaitingForCapacity", condCap)
	}

	// Pod must not be deleted while inside lifetime.
	var remainingPod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &remainingPod); err != nil {
		t.Fatalf("pod was unexpectedly deleted: %v", err)
	}
}

// 3. Finished callback is sent to callbackURL for a job that never scheduled.
func TestServiceJobFinishedCallbackSentWhenNeverScheduled(t *testing.T) {
	var mu sync.Mutex
	var callbacks []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		callbacks = append(callbacks, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ej := serviceJobFixture("sandbox-cb-never-scheduled", "uid-lt-3", 10*time.Hour, 28800)
	ej.Spec.CallbackURL = srv.URL
	node := burstNode("burst-node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-cb-never-scheduled-pod",
			Namespace: ej.Namespace,
			Labels: map[string]string{
				labelJobUID: string(ej.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, c := boundsTestReconciler(t, ej, node, pod)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(callbacks) != 1 {
		t.Fatalf("got %d callbacks, want 1 terminal callback", len(callbacks))
	}
	cb := callbacks[0]
	if cb["jobId"] != ej.Name {
		t.Fatalf("callback jobId = %v, want %q", cb["jobId"], ej.Name)
	}
	if cb["status"] != string(computev1alpha1.PhaseSucceeded) {
		t.Fatalf("callback status = %v, want %q", cb["status"], computev1alpha1.PhaseSucceeded)
	}

	var updated computev1alpha1.EphemeralJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name}, &updated); err != nil {
		t.Fatalf("Get EphemeralJob: %v", err)
	}
	if !meta_IsStatusConditionTrue(updated.Status.Conditions, computev1alpha1.ConditionCallbackDelivered) {
		t.Fatal("ConditionCallbackDelivered not set after callback delivered")
	}
}

// ── Task B: Capacity Wait Bound ──────────────────────────────────────────────

// 1. Timeout fires after capacityWaitSeconds.
func TestCapacityWaitTimeoutFiresAfterWaitSeconds(t *testing.T) {
	waitLimit := int32(180) // 3 minutes
	// Job created 5 minutes ago, exceeding 3-minute capacityWaitSeconds.
	ej := serviceJobFixture("sandbox-cap-timeout", "uid-cap-1", 5*time.Minute, 28800)
	ej.Spec.CapacityWaitSeconds = &waitLimit
	node := burstNode("burst-node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-cap-timeout-pod",
			Namespace: ej.Namespace,
			Labels: map[string]string{
				labelJobUID: string(ej.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, c := boundsTestReconciler(t, ej, node, pod)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("expected terminal result without requeue, got: %+v", res)
	}

	var updated computev1alpha1.EphemeralJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name}, &updated); err != nil {
		t.Fatalf("Get EphemeralJob: %v", err)
	}

	if updated.Status.Phase != computev1alpha1.PhaseTimedOut {
		t.Fatalf("Phase = %q, want %q", updated.Status.Phase, computev1alpha1.PhaseTimedOut)
	}
	if updated.Status.CompletionTime == nil {
		t.Fatal("CompletionTime is nil, want set for TTL reaping")
	}
	if !strings.Contains(updated.Status.Message, "capacity wait deadline exceeded") {
		t.Fatalf("Message = %q, want capacity wait deadline exceeded message", updated.Status.Message)
	}

	condCap := meta_FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionCapacity)
	if condCap == nil || condCap.Status != metav1.ConditionFalse || condCap.Reason != computev1alpha1.ReasonCapacityTimeout {
		t.Fatalf("Condition CapacityAvailable = %+v, want Status=False Reason=CapacityTimeout", condCap)
	}

	condComp := meta_FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionComplete)
	if condComp == nil || condComp.Status != metav1.ConditionFalse || condComp.Reason != computev1alpha1.ReasonCapacityTimeout {
		t.Fatalf("Condition Complete = %+v, want Status=False Reason=CapacityTimeout", condComp)
	}

	// Verify pod was deleted on timeout.
	var remainingPod corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &remainingPod)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("pod was not deleted on capacity timeout: err = %v", err)
	}
}

// 2. None fires before capacityWaitSeconds.
func TestCapacityWaitTimeoutDoesNotFireBeforeWaitSeconds(t *testing.T) {
	waitLimit := int32(300) // 5 minutes
	// Job created 1 minute ago, well within 5-minute capacityWaitSeconds.
	ej := serviceJobFixture("sandbox-cap-active", "uid-cap-2", 1*time.Minute, 28800)
	ej.Spec.CapacityWaitSeconds = &waitLimit
	node := burstNode("burst-node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-cap-active-pod",
			Namespace: ej.Namespace,
			Labels: map[string]string{
				labelJobUID: string(ej.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, c := boundsTestReconciler(t, ej, node, pod)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected RequeueAfter > 0, got: %+v", res)
	}

	var updated computev1alpha1.EphemeralJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name}, &updated); err != nil {
		t.Fatalf("Get EphemeralJob: %v", err)
	}

	if updated.Status.Phase != computev1alpha1.PhaseProvisioning {
		t.Fatalf("Phase = %q, want %q", updated.Status.Phase, computev1alpha1.PhaseProvisioning)
	}
	if updated.Status.CompletionTime != nil {
		t.Fatal("CompletionTime is set, but job should still be waiting")
	}

	condCap := meta_FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionCapacity)
	if condCap == nil || condCap.Reason != computev1alpha1.ReasonWaitingForCapacity {
		t.Fatalf("Condition CapacityAvailable = %+v, want Reason=WaitingForCapacity", condCap)
	}

	// Pod must still exist.
	var remainingPod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &remainingPod); err != nil {
		t.Fatalf("pod was unexpectedly deleted: %v", err)
	}
}

// 3. None fires once capacity is ready (cap.Ready == true), even if past capacityWaitSeconds.
func TestCapacityWaitTimeoutDoesNotFireOnceCapacityReady(t *testing.T) {
	waitLimit := int32(60) // 1 minute
	// Job created 10 minutes ago, far beyond capacityWaitSeconds.
	ej := serviceJobFixture("sandbox-cap-ready", "uid-cap-3", 10*time.Minute, 28800)
	ej.Spec.CapacityWaitSeconds = &waitLimit
	node := burstNode("burst-node-1")

	// Pod is Running and scheduled, meaning capacity IS ready.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-cap-ready-pod",
			Namespace: ej.Namespace,
			Labels: map[string]string{
				labelJobUID: string(ej.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: workloadContainerName, Ready: true},
			},
		},
	}

	r, c := boundsTestReconciler(t, ej, node, pod)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	var updated computev1alpha1.EphemeralJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name}, &updated); err != nil {
		t.Fatalf("Get EphemeralJob: %v", err)
	}

	// Must NOT be TimedOut. Because capacity is ready, the capacity wait clock is not evaluated.
	if updated.Status.Phase == computev1alpha1.PhaseTimedOut {
		t.Fatalf("Phase = %q, capacity timeout must not fire when capacity is ready", updated.Status.Phase)
	}
	if updated.Status.Phase != computev1alpha1.PhaseRunning {
		t.Fatalf("Phase = %q, want %q", updated.Status.Phase, computev1alpha1.PhaseRunning)
	}
}

// 4. Callback carries reason CapacityTimeout.
func TestCapacityWaitTimeoutCallbackCarriesReason(t *testing.T) {
	var mu sync.Mutex
	var callbacks []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		callbacks = append(callbacks, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	waitLimit := int32(60) // 1 minute
	ej := serviceJobFixture("sandbox-cb-cap-timeout", "uid-cap-4", 5*time.Minute, 28800)
	ej.Spec.CapacityWaitSeconds = &waitLimit
	ej.Spec.CallbackURL = srv.URL
	node := burstNode("burst-node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-cb-cap-timeout-pod",
			Namespace: ej.Namespace,
			Labels: map[string]string{
				labelJobUID: string(ej.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, c := boundsTestReconciler(t, ej, node, pod)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(callbacks) != 1 {
		t.Fatalf("got %d callbacks, want 1 terminal callback", len(callbacks))
	}
	cb := callbacks[0]
	if cb["jobId"] != ej.Name {
		t.Fatalf("callback jobId = %v, want %q", cb["jobId"], ej.Name)
	}
	if cb["status"] != string(computev1alpha1.PhaseTimedOut) {
		t.Fatalf("callback status = %v, want %q", cb["status"], computev1alpha1.PhaseTimedOut)
	}
	if cb["reason"] != computev1alpha1.ReasonCapacityTimeout {
		t.Fatalf("callback reason = %v, want %q", cb["reason"], computev1alpha1.ReasonCapacityTimeout)
	}

	var updated computev1alpha1.EphemeralJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: ej.Namespace, Name: ej.Name}, &updated); err != nil {
		t.Fatalf("Get EphemeralJob: %v", err)
	}
	if !meta_IsStatusConditionTrue(updated.Status.Conditions, computev1alpha1.ConditionCallbackDelivered) {
		t.Fatal("ConditionCallbackDelivered not set after callback delivered")
	}
}

// ── Task C: Cluster Status Verification (kubectl get ephemeraljobs output) ────

func TestClusterStatusReport(t *testing.T) {
	ctx := context.Background()

	// 1. Task A Fixture: Terminated by Lifetime Bound
	ejA := serviceJobFixture("sandbox-task-a", "uid-task-a", 10*time.Hour, 28800)
	nodeA := burstNode("burst-node-a")
	podA := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-task-a-pod",
			Namespace: ejA.Namespace,
			Labels:    map[string]string{labelJobUID: string(ejA.UID)},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	rA, cA := boundsTestReconciler(t, ejA, nodeA, podA)
	_, err := rA.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ejA.Namespace, Name: ejA.Name},
	})
	if err != nil {
		t.Fatalf("Task A Reconcile: %v", err)
	}
	var storedA computev1alpha1.EphemeralJob
	if err := cA.Get(ctx, types.NamespacedName{Namespace: ejA.Namespace, Name: ejA.Name}, &storedA); err != nil {
		t.Fatalf("Get Task A: %v", err)
	}

	phaseA := string(storedA.Status.Phase)
	capCondA := meta_FindStatusCondition(storedA.Status.Conditions, computev1alpha1.ConditionCapacity)
	capacityA := ""
	if capCondA != nil {
		capacityA = capCondA.Reason
	}
	messageA := storedA.Status.Message

	if phaseA != "Succeeded" {
		t.Errorf("Task A Phase = %q, want %q", phaseA, "Succeeded")
	}
	if capacityA != "WaitingForCapacity" {
		t.Errorf("Task A Capacity = %q, want %q", capacityA, "WaitingForCapacity")
	}
	if !strings.Contains(messageA, "reached the 28800s maximum lifetime") {
		t.Errorf("Task A Message = %q, want maximum lifetime", messageA)
	}

	// 2. Task B Fixture: Terminated by Capacity Wait Bound
	waitLimit := int32(180)
	ejB := serviceJobFixture("sandbox-task-b", "uid-task-b", 5*time.Minute, 28800)
	ejB.Spec.CapacityWaitSeconds = &waitLimit
	nodeB := burstNode("burst-node-b")
	podB := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-task-b-pod",
			Namespace: ejB.Namespace,
			Labels:    map[string]string{labelJobUID: string(ejB.UID)},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{WorkloadLocationKey: WorkloadLocationValue},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	rB, cB := boundsTestReconciler(t, ejB, nodeB, podB)
	_, err = rB.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ejB.Namespace, Name: ejB.Name},
	})
	if err != nil {
		t.Fatalf("Task B Reconcile: %v", err)
	}
	var storedB computev1alpha1.EphemeralJob
	if err := cB.Get(ctx, types.NamespacedName{Namespace: ejB.Namespace, Name: ejB.Name}, &storedB); err != nil {
		t.Fatalf("Get Task B: %v", err)
	}

	phaseB := string(storedB.Status.Phase)
	capCondB := meta_FindStatusCondition(storedB.Status.Conditions, computev1alpha1.ConditionCapacity)
	capacityB := ""
	if capCondB != nil {
		capacityB = capCondB.Reason
	}
	messageB := storedB.Status.Message

	if phaseB != "TimedOut" {
		t.Errorf("Task B Phase = %q, want %q", phaseB, "TimedOut")
	}
	if capacityB != "CapacityTimeout" {
		t.Errorf("Task B Capacity = %q, want %q", capacityB, "CapacityTimeout")
	}
	if !strings.Contains(messageB, "capacity wait deadline exceeded") {
		t.Errorf("Task B Message = %q, want capacity wait deadline exceeded", messageB)
	}

	t.Logf("\nSimulated `kubectl get ephemeraljobs` table columns:\n"+
		"NAME             PHASE       CAPACITY            MESSAGE\n"+
		"%-16s %-11s %-19s %s\n"+
		"%-16s %-11s %-19s %s\n",
		storedA.Name, phaseA, capacityA, messageA,
		storedB.Name, phaseB, capacityB, messageB)
}
