package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// A failed job is surfaced to a person and to the agent that started it, and
// neither can act on "FAILED" alone. Every case below is a reason that WAS
// available at the moment of failure and used to be discarded — which is the worst
// shape of this bug, because the pod is deleted with the Job and nothing can
// recover it afterwards.

func failedJob(reason, msg string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: reason, Message: msg,
		}}},
	}
}

// The reported case: the controller gave up, said why, and the caller got nothing.
func TestJobFinishedReturnsTheConditionsOwnWords(t *testing.T) {
	done, phase, _, detail := jobFinished(
		failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit"))

	if !done || phase != computev1alpha1.PhaseFailed {
		t.Fatalf("done=%v phase=%v, want true/Failed", done, phase)
	}
	if detail == "" {
		t.Fatal("no detail: the caller receives FAILED with nothing after the colon, " +
			"which is the bug this closes")
	}
	for _, want := range []string{"BackoffLimitExceeded", "backoff limit"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q omits %q", detail, want)
		}
	}
}

// A timeout keeps its own phase AND gains a reason. The phase distinction is the
// submitter's — a job out of time may be worth resubmitting with a larger budget —
// so losing it would be worse than losing the message.
func TestTimedOutKeepsItsPhaseAndGainsAReason(t *testing.T) {
	done, phase, _, detail := jobFinished(failedJob("DeadlineExceeded", "Job was active longer than specified deadline"))
	if !done || phase != computev1alpha1.PhaseTimedOut {
		t.Fatalf("phase = %v, want TimedOut", phase)
	}
	if !strings.Contains(detail, "DeadlineExceeded") {
		t.Fatalf("detail %q omits the reason", detail)
	}
}

// Success must not acquire a failure message.
func TestSuccessReportsExitZeroAndNoFailureText(t *testing.T) {
	job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "",
	}}}}
	done, phase, exit, _ := jobFinished(job)
	if !done || phase != computev1alpha1.PhaseSucceeded {
		t.Fatalf("phase = %v, want Succeeded", phase)
	}
	if exit == nil || *exit != 0 {
		t.Fatalf("exit = %v, want 0", exit)
	}
}

func TestConditionDetailJoinsReasonAndMessageAndNeitherAlone(t *testing.T) {
	for _, tc := range []struct{ reason, msg, want string }{
		{"BackoffLimitExceeded", "ran out", "BackoffLimitExceeded: ran out"},
		{"BackoffLimitExceeded", "", "BackoffLimitExceeded"},
		{"", "ran out", "ran out"},
		{"", "", ""},
	} {
		got := conditionDetail(batchv1.JobCondition{Reason: tc.reason, Message: tc.msg})
		if got != tc.want {
			t.Fatalf("reason=%q msg=%q -> %q, want %q", tc.reason, tc.msg, got, tc.want)
		}
	}
}

// A nil clientset must cost the log tail and nothing else. The operator runs
// without one under test, and a failure to read a log must not become a failure to
// report the failure.
func TestLogTailIsAbsentNotFatalWithoutAClientset(t *testing.T) {
	r := &EphemeralJobReconciler{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	if got := r.containerLogTail(context.Background(), pod); got != "" {
		t.Fatalf("tail = %q, want empty", got)
	}
}

// The bounds are the reason this read is safe inside Reconcile. A workload that
// logged a gigabyte must not put it in a status field.
func TestLogTailIsBounded(t *testing.T) {
	if logTailBytes <= 0 || logTailBytes > 8192 {
		t.Fatalf("logTailBytes = %d: a status field is not a log store", logTailBytes)
	}
	if logTailLines <= 0 || logTailLines > 200 {
		t.Fatalf("logTailLines = %d", logTailLines)
	}
	if logTailTimeout <= 0 {
		t.Fatal("an unbounded log read stalls the work queue for every other job")
	}
}

func terminatedPod(containers ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: containers}}
}

func terminated(name string, exit int32, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: exit, Reason: reason, Message: message,
	}}}
}

// The workload said why it failed; that is the job's failure, not the kubelet's
// "Error". It is what reaches the person and the agent that started the job.
func TestAFailedWorkloadsOwnMessageIsTheFailure(t *testing.T) {
	pod := terminatedPod(
		terminated("agent-vault", 0, "Completed", ""),
		terminated("workload", 1, "Error", "1.4 s of speech in 5.0 s of audio; at least 3 s is needed\n"),
	)
	if got := podTerminationMessage(pod); got != "1.4 s of speech in 5.0 s of audio; at least 3 s is needed" {
		t.Fatalf("got %q, want the workload's own message", got)
	}
}

// A workload that wrote nothing still fails with the kubelet's reason.
func TestWithoutAMessageTheKubeletsReasonStands(t *testing.T) {
	pod := terminatedPod(terminated("workload", 137, "OOMKilled", ""))
	if got := podTerminationMessage(pod); got != "container workload terminated: OOMKilled" {
		t.Fatalf("got %q", got)
	}
}

// A container that exited cleanly is not why the pod failed, whatever it wrote.
func TestASuccessfulContainersMessageIsNotTheFailure(t *testing.T) {
	pod := terminatedPod(
		terminated("agent-vault", 0, "Completed", "shutting down"),
		terminated("workload", 1, "Error", ""),
	)
	if got := podTerminationMessage(pod); strings.Contains(got, "shutting down") {
		t.Fatalf("got %q: a clean exit's message reported as the failure", got)
	}
}
