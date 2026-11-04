package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// A Job-mode workload's object storage is bound in buildPodSpec, by
// withOutputStorageEnv, the one path every pod of ours is built through.

// jobRefusedAtAdmission reports whether creating the Job was refused by the API
// server's admission chain rather than failing transiently.
//
// A validating webhook's denial (Kyverno) arrives as 400 -- the API server
// raises any denial below 400 to 400 -- Pod Security and RBAC as 403, schema
// validation as 422. None of them changes on retry.
func jobRefusedAtAdmission(err error) bool {
	return apierrors.IsBadRequest(err) || apierrors.IsForbidden(err) || apierrors.IsInvalid(err)
}

// deleteJob removes the request's Job, if it has one, with its pods.
func (r *EphemeralJobReconciler) deleteJob(ctx context.Context, ej *computev1alpha1.EphemeralJob) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ej.Namespace, Name: jobNameFor(ej)}}
	policy := metav1.DeletePropagationBackground
	return client.IgnoreNotFound(r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}))
}
