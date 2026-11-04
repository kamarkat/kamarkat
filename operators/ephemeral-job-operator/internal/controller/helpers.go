package controller

import (
	"context"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

func meta_SetStatusCondition(c *[]metav1.Condition, cond metav1.Condition) {
	meta.SetStatusCondition(c, cond)
}

func meta_IsStatusConditionTrue(c []metav1.Condition, condType string) bool {
	return meta.IsStatusConditionTrue(c, condType)
}

func meta_FindStatusCondition(c []metav1.Condition, condType string) *metav1.Condition {
	return meta.FindStatusCondition(c, condType)
}

func isTerminal(p computev1alpha1.Phase) bool {
	switch p {
	case computev1alpha1.PhaseSucceeded, computev1alpha1.PhaseFailed, computev1alpha1.PhaseTimedOut:
		return true
	}
	return false
}

func jobNameFor(ej *computev1alpha1.EphemeralJob) string {
	return "ej-" + ej.Name
}

// tenantFromNamespace derives the cost-attribution label the tenant ABI
// requires on workloads in tenant-* namespaces.
func tenantFromNamespace(ns string) string {
	return strings.TrimPrefix(ns, "tenant-")
}

// jobFinished maps the Job's terminal conditions onto the EphemeralJob phase.
//
// DeadlineExceeded is reported as TimedOut rather than Failed. The distinction
// is the submitter's: a job that ran out of time may be worth resubmitting with
// a larger budget, and one that exited non-zero is not.
// It also returns the CONDITION'S OWN WORDS. This used to discard them, so a caller
// received status FAILED with an empty message and could not tell a bad input from an
// infrastructure fault -- the one thing it needs in order to decide whether retrying
// makes sense. The controller already phrased the reason for a human
// ("BackoffLimitExceeded: Job has reached the specified backoff limit"); rephrasing it
// here would lose the only thing it is good at.
func jobFinished(job *batchv1.Job) (bool, computev1alpha1.Phase, *int32, string) {
	if job == nil {
		return false, "", nil, ""
	}
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, computev1alpha1.PhaseSucceeded, ptr(int32(0)), conditionDetail(c)
		case batchv1.JobFailed:
			if c.Reason == "DeadlineExceeded" {
				return true, computev1alpha1.PhaseTimedOut, nil, conditionDetail(c)
			}
			return true, computev1alpha1.PhaseFailed, nil, conditionDetail(c)
		}
	}
	return false, "", nil, ""
}

func conditionDetail(c batchv1.JobCondition) string {
	switch {
	case c.Message == "":
		return c.Reason
	case c.Reason == "":
		return c.Message
	default:
		return c.Reason + ": " + c.Message
	}
}

// jobAdmissionRejected detects a Job whose pods the API server refuses to
// admit. At the expected submission volume the common cause is the fleet's
// priority-scoped ResourceQuota being full (ADR-052 §5), which is terminal —
// the pod was never created, so no amount of waiting produces one.
func jobAdmissionRejected(job *batchv1.Job) (reason, message string, rejected bool) {
	if job == nil {
		return "", "", false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == "True" &&
			(c.Reason == "FailedCreate" || strings.Contains(c.Message, "exceeded quota")) {
			return c.Reason, c.Message, true
		}
	}
	return "", "", false
}

// admissionRejectionThreshold is how many identical rejections make a
// rejection structural rather than a race. A pod creation refused because the
// namespace quota was momentarily full can succeed on the next attempt; one
// refused three times in a row is being refused for a reason that retrying
// does not change.
const admissionRejectionThreshold = 3

// jobPodCreationBlocked reports a Job whose pods are being REFUSED at
// admission, which the condition check above cannot see.
//
// A rejected pod creation is not a failed pod. The job controller's Create call
// returns an error, it emits a FailedCreate event and retries — forever. No pod
// is ever created, so status.failed stays 0 and JobFailed is never set. The Job
// reads Running with zero pods and AssessCapacity, finding no pods, reports
// WaitingForCapacity: a job that can never run, described as one waiting for a
// node, held open until something outside deletes it.
//
// The events are the only record, so they are what this reads.
func jobPodCreationBlocked(ctx context.Context, c client.Reader, job *batchv1.Job) (reason, message string, blocked bool) {
	if job == nil {
		return "", "", false
	}
	var events corev1.EventList
	if err := c.List(ctx, &events,
		client.InNamespace(job.Namespace),
		client.MatchingFields{"involvedObject.uid": string(job.UID)},
	); err != nil {
		// Best-effort: this is a diagnosis, not a gate. Failing to read events
		// must not fail the reconcile — the job is left in its current phase
		// and the next pass tries again.
		return "", "", false
	}
	for _, e := range events.Items {
		if e.Reason == "FailedCreate" && e.Count >= admissionRejectionThreshold {
			return e.Reason, e.Message, true
		}
	}
	return "", "", false
}
