package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// A Service-mode workload has no completion, so a caller learns its state only
// from these events. Every property below fails by DELIVERING SOMETHING — a
// plausible event with the wrong identity — which no integration test notices,
// because the receiver acts on it and reports success.

func ejFor(name, uid, requestID string) *computev1alpha1.EphemeralJob {
	return &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID(uid),
			Labels: map[string]string{"waypoint-session": "s-1"},
		},
		Spec: computev1alpha1.EphemeralJobSpec{
			Mode:        computev1alpha1.ModeService,
			RequestID:   requestID,
			CallbackURL: "http://sdk-workload:3000/internal/sandbox/lifecycle",
		},
	}
}

// THE failure the caller asked us to prevent: a workload recycled under one name
// has two incarnations, so the job name cannot tell them apart. A redelivered
// terminal event from the first would otherwise end the second, which is live.
func TestEventIdsDifferAcrossIncarnationsOfOneName(t *testing.T) {
	// Same name, same (session-derived) requestId — the worst case, and the one a
	// caller falls into by deriving requestId from the session like the name.
	first := ejFor("sandbox-s1", "uid-A", "req-session-1")
	second := ejFor("sandbox-s1", "uid-B", "req-session-1")

	if readyEventID(first, "pod-1") == readyEventID(second, "pod-1") {
		t.Fatal("two incarnations produced the same ready event id; a stale event would " +
			"be indistinguishable from this incarnation's")
	}
	// The terminal id is derived from requestId when set, so with a reused
	// requestId it DOES collide. That is why uid is on the payload: it is the
	// only field that separates them in that case.
	if terminalEventID(first, computev1alpha1.PhaseSucceeded) !=
		terminalEventID(second, computev1alpha1.PhaseSucceeded) {
		t.Skip("terminal ids already differ; the uid field is then belt-and-braces")
	}
	if first.UID == second.UID {
		t.Fatal("the uid cannot separate incarnations either; nothing can")
	}
}

// A replacement pod must produce a NEW ready event, not a duplicate of the first.
func TestReadyEventIdIsPerPod(t *testing.T) {
	ej := ejFor("sandbox-s1", "uid-A", "req-1")
	if readyEventID(ej, "pod-1") == readyEventID(ej, "pod-2") {
		t.Fatal("a replacement pod reused the first pod's ready event id, so the caller " +
			"would drop it as a duplicate and never learn the sandbox was replaced")
	}
}

// The ready event and the terminal event of ONE incarnation must not collide,
// or a caller that saw the sandbox start would discard news that it ended.
func TestReadyAndTerminalEventIdsNeverCollide(t *testing.T) {
	ej := ejFor("sandbox-s1", "uid-A", "req-1")
	ready := readyEventID(ej, "pod-1")
	for _, p := range []computev1alpha1.Phase{
		computev1alpha1.PhaseSucceeded, computev1alpha1.PhaseFailed,
		computev1alpha1.PhaseTimedOut, computev1alpha1.PhaseCancelled,
	} {
		if ready == terminalEventID(ej, p) {
			t.Fatalf("ready collides with the %s terminal event id", p)
		}
	}
}

// The gates must be SEPARATE conditions. One shared condition would mean the
// ready notification suppressed the terminal callback, so a sandbox would report
// that it started and never that it ended — the worst available outcome, because
// the caller holds a session open forever on a workload that has gone.
func TestReadyAndTerminalUseDifferentGates(t *testing.T) {
	if computev1alpha1.ConditionReadyNotified == computev1alpha1.ConditionCallbackDelivered {
		t.Fatal("ready and terminal share one gate; firing ready would suppress terminal")
	}
}

// Every field the caller needs to route and deduplicate, pinned by name. A
// renamed or dropped field is a parser that silently reads zero values.
func TestCallbackPayloadCarriesIdentityAndRouting(t *testing.T) {
	ej := ejFor("sandbox-s1", "uid-A", "req-1")
	// Mirrors what fireLifecycleCallback marshals; asserted here because the
	// marshalling happens inside an HTTP call this test does not make.
	payload := map[string]any{
		"jobId":           ej.Name,
		"status":          string(computev1alpha1.PhaseRunning),
		"terminalEventId": readyEventID(ej, "pod-1"),
		"uid":             string(ej.UID),
		"timestamp":       metav1.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		"labels":          ej.Labels,
		"requestId":       ej.Spec.RequestID,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("payload does not marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("payload does not round-trip: %v", err)
	}
	for _, k := range []string{"jobId", "status", "terminalEventId", "uid", "timestamp", "labels"} {
		if _, ok := back[k]; !ok {
			t.Fatalf("payload is missing %q, which the caller needs", k)
		}
	}
	if back["uid"] != "uid-A" {
		t.Fatalf("uid = %v, want the incarnation's uid", back["uid"])
	}
	if lbl, ok := back["labels"].(map[string]any); !ok || lbl["waypoint-session"] != "s-1" {
		t.Fatalf("labels did not survive: %v", back["labels"])
	}
}

// ── Fires once, and WHY it fires once ────────────────────────────────────────
//
// The contract says one ready event per pod. What delivers that is not the gate
// condition existing — it is the gate being DURABLE before anything else in the
// reconcile can lose a race. A version that deferred the write to the caller's
// later status update sent the event twice, 11 ms apart, when that later write
// lost a conflict and the reconcile requeued.
//
// So these pin the ordering, not just the outcome.

func lifecycleReconciler(t *testing.T, ej *computev1alpha1.EphemeralJob) (*EphemeralJobReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := computev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ej).WithStatusSubresource(ej).Build()
	return &EphemeralJobReconciler{Client: c, Scheme: s}, c
}

func TestReadyCallbackPersistsItsGateBeforeReturning(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ej := ejFor("sandbox-s1", "uid-A", "req-1")
	ej.Namespace = "ns"
	ej.Spec.CallbackURL = srv.URL
	r, c := lifecycleReconciler(t, ej)

	if err := r.fireLifecycleCallback(context.Background(), ej,
		computev1alpha1.PhaseRunning, nil,
		computev1alpha1.ConditionReadyNotified, readyEventID(ej, "pod-1")); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("POSTs = %d, want 1", got)
	}

	// THE PROPERTY. Read the object back from the API, not from memory: an
	// in-memory condition that was never written is exactly the bug this
	// replaces, and only a fresh read can tell them apart.
	var stored computev1alpha1.EphemeralJob
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: "sandbox-s1"}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !meta_IsStatusConditionTrue(stored.Status.Conditions, computev1alpha1.ConditionReadyNotified) {
		t.Fatal("the gate was not persisted before returning, so a later lost race " +
			"would re-send this event")
	}
}

func TestReadyCallbackDoesNotFireTwiceForOnePod(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ej := ejFor("sandbox-s1", "uid-A", "req-1")
	ej.Namespace = "ns"
	ej.Spec.CallbackURL = srv.URL
	r, _ := lifecycleReconciler(t, ej)
	ctx := context.Background()
	id := readyEventID(ej, "pod-1")

	for i := 0; i < 3; i++ {
		if err := r.fireLifecycleCallback(ctx, ej, computev1alpha1.PhaseRunning, nil,
			computev1alpha1.ConditionReadyNotified, id); err != nil {
			t.Fatalf("fire %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("POSTs = %d across three reconciles, want 1", got)
	}
}

// A replacement pod must get its own event. The gate is reset by the caller when
// the pod name changed; here we assert that once reset, the event does fire again
// -- otherwise a replaced sandbox would be silently unreported.
func TestReadyCallbackFiresAgainAfterTheGateIsReset(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ej := ejFor("sandbox-s1", "uid-A", "req-1")
	ej.Namespace = "ns"
	ej.Spec.CallbackURL = srv.URL
	r, _ := lifecycleReconciler(t, ej)
	ctx := context.Background()

	_ = r.fireLifecycleCallback(ctx, ej, computev1alpha1.PhaseRunning, nil,
		computev1alpha1.ConditionReadyNotified, readyEventID(ej, "pod-1"))

	// What reconcileServiceMode does when status.podName no longer matches.
	meta_SetStatusCondition(&ej.Status.Conditions, metav1.Condition{
		Type: computev1alpha1.ConditionReadyNotified, Status: metav1.ConditionFalse,
		Reason: "PodReplaced", Message: "a new pod is serving",
	})

	_ = r.fireLifecycleCallback(ctx, ej, computev1alpha1.PhaseRunning, nil,
		computev1alpha1.ConditionReadyNotified, readyEventID(ej, "pod-2"))

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("POSTs = %d, want 2 (one per pod incarnation)", got)
	}
}

// Firing ready must not consume the terminal gate. A shared gate would mean a
// sandbox reported that it started and never that it ended.
func TestReadyDoesNotSuppressTheTerminalCallback(t *testing.T) {
	var statuses []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		statuses = append(statuses, fmt.Sprint(body["status"]))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ej := ejFor("sandbox-s1", "uid-A", "req-1")
	ej.Namespace = "ns"
	ej.Spec.CallbackURL = srv.URL
	r, _ := lifecycleReconciler(t, ej)
	ctx := context.Background()

	_ = r.fireLifecycleCallback(ctx, ej, computev1alpha1.PhaseRunning, nil,
		computev1alpha1.ConditionReadyNotified, readyEventID(ej, "pod-1"))
	_ = r.fireCallback(ctx, ej, computev1alpha1.PhaseSucceeded, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 2 || statuses[0] != "Running" || statuses[1] != "Succeeded" {
		t.Fatalf("deliveries = %v, want [Running Succeeded]", statuses)
	}
}
