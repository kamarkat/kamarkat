package controller

import (
	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// withOutputStorageEnv gives a workload that writes its own result
// (spec.output) the platform bucket to write it to.
//
// spec.output names WHERE (objectPrefix, EPHEMERAL_JOB_OUTPUT) and never the
// credentials (OutputSpec). The workload needs both, and it had no way to get
// the second: its submitter used to copy the S3 values, secret key included,
// into spec.env, which stored them in plaintext in the CR and the submitter's
// database. That copy was removed on the understanding that the operator binds
// them, and nothing did — every job with an output failed on its first S3 call
// (KeyError: 'S3_ENDPOINT_URL').
//
// They are bound by reference, from the same two fleet objects the
// workspace-sync sidecar reads (resolveWorkspaceSyncSecret/Config): location
// from the ConfigMap, credentials from the Secret, key by key so no other value
// the fleet keeps there reaches the workload.
//
// The four the workload cannot run without are REQUIRED references. A missing
// object or key is a pod the kubelet refuses to start
// (CreateContainerConfigError), which podStartupBlocker reports with the name,
// instead of a workload that starts, does the work and fails at the upload.
// S3_REGION stays optional: fleets declare none, and an S3-compatible endpoint
// that ignores region needs none.
//
// A name the request states in spec.env keeps the request's value, as for
// EPHEMERAL_JOB_INPUT/OUTPUT: what a caller states wins over what the platform
// derives.
func withOutputStorageEnv(ej *computev1alpha1.EphemeralJob, env []corev1.EnvVar, fleetID string) []corev1.EnvVar {
	if ej.Spec.Output == nil {
		return env
	}
	secretName := resolveWorkspaceSyncSecret(ej.Namespace, fleetID)
	configName := resolveWorkspaceSyncConfig(ej.Namespace, fleetID)
	fromConfig := func(key string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{Name: key, ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: configName},
				Key:                  key,
				Optional:             ptr(optional),
			},
		}}
	}
	fromSecret := func(key string) corev1.EnvVar {
		return corev1.EnvVar{Name: key, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             ptr(false),
			},
		}}
	}
	for _, v := range []corev1.EnvVar{
		fromConfig("S3_ENDPOINT_URL", false),
		fromConfig("S3_BUCKET_NAME", false),
		fromSecret("S3_ACCESS_KEY_ID"),
		fromSecret("S3_SECRET_ACCESS_KEY"),
		fromConfig("S3_REGION", true),
	} {
		if _, stated := ej.Spec.Env[v.Name]; stated {
			continue
		}
		env = append(env, v)
	}
	return env
}
