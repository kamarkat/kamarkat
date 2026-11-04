package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// A job that writes its own result is told where (spec.output) and must also be
// able to write there. Without these it ran, did its work, and failed on its
// first S3 call: KeyError: 'S3_ENDPOINT_URL'.

func outputJob(env map[string]string) *computev1alpha1.EphemeralJob {
	ej := &computev1alpha1.EphemeralJob{}
	ej.Namespace = "tenant-nutgraf-waypoint"
	ej.Spec.Image = "example.com/img@sha256:" + strings.Repeat("a", 64)
	ej.Spec.Env = env
	ej.Spec.Output = &computev1alpha1.OutputSpec{ObjectPrefix: "app/s1/artifacts/audio/run-1/"}
	return ej
}

func envVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

func TestAJobWithAnOutputGetsTheFleetBucketByReference(t *testing.T) {
	env := withOutputStorageEnv(outputJob(nil), nil, "waypoint")

	for _, c := range []struct {
		name, object string
		secret       bool
	}{
		{"S3_ENDPOINT_URL", "waypoint-config", false},
		{"S3_BUCKET_NAME", "waypoint-config", false},
		{"S3_ACCESS_KEY_ID", "waypoint-sdk-secrets", true},
		{"S3_SECRET_ACCESS_KEY", "waypoint-sdk-secrets", true},
	} {
		v := envVar(env, c.name)
		if v == nil {
			t.Fatalf("%s not bound: the workload cannot reach the bucket spec.output names", c.name)
		}
		if v.Value != "" || v.ValueFrom == nil {
			t.Fatalf("%s carries a literal: it must be a reference, never the value in the pod spec", c.name)
		}
		if c.secret {
			ref := v.ValueFrom.SecretKeyRef
			if ref == nil || ref.Name != c.object || ref.Key != c.name {
				t.Errorf("%s: want key %s of Secret %s, got %+v", c.name, c.name, c.object, v.ValueFrom)
				continue
			}
			if ref.Optional == nil || *ref.Optional {
				t.Errorf("%s is optional: a missing credential must stop the pod, not fail the upload", c.name)
			}
		} else {
			ref := v.ValueFrom.ConfigMapKeyRef
			if ref == nil || ref.Name != c.object || ref.Key != c.name {
				t.Errorf("%s: want key %s of ConfigMap %s, got %+v", c.name, c.name, c.object, v.ValueFrom)
				continue
			}
			if ref.Optional == nil || *ref.Optional {
				t.Errorf("%s is optional: a missing location must stop the pod, not fail the upload", c.name)
			}
		}
	}
	if region := envVar(env, "S3_REGION"); region == nil || region.ValueFrom.ConfigMapKeyRef == nil ||
		region.ValueFrom.ConfigMapKeyRef.Optional == nil || !*region.ValueFrom.ConfigMapKeyRef.Optional {
		t.Errorf("S3_REGION must be bound and optional: fleets declare none")
	}
}

func TestAJobWithNoOutputGetsNoBucket(t *testing.T) {
	ej := outputJob(nil)
	ej.Spec.Output = nil
	if env := withOutputStorageEnv(ej, nil, "waypoint"); len(env) != 0 {
		t.Errorf("a job with no output was given bucket access: %v", env)
	}
}

func TestAStatedBucketVariableKeepsTheRequestsValue(t *testing.T) {
	ej := outputJob(map[string]string{"S3_BUCKET_NAME": "other-bucket"})
	env := withOutputStorageEnv(ej, []corev1.EnvVar{{Name: "S3_BUCKET_NAME", Value: "other-bucket"}}, "waypoint")
	count := 0
	for _, e := range env {
		if e.Name == "S3_BUCKET_NAME" {
			count++
		}
	}
	if count != 1 || envVar(env, "S3_BUCKET_NAME").Value != "other-bucket" {
		t.Errorf("the request's S3_BUCKET_NAME was duplicated or overridden: %v", env)
	}
}

// The binding happens on the path every pod of ours is built through, so it
// reaches the workload container of an actual pod spec.
func TestThePodSpecsWorkloadHasTheBucket(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	ej := outputJob(nil)
	spec := r.buildPodSpec(context.Background(), ej, p, r.buildWorkloadContainer(ej))
	for _, c := range spec.Containers {
		if c.Name != workloadContainerName {
			continue
		}
		if envVar(c.Env, "S3_ENDPOINT_URL") == nil || envVar(c.Env, envJobOutput) == nil {
			t.Fatalf("workload container lacks the bucket or its output location: %v", c.Env)
		}
		return
	}
	t.Fatalf("no %q container in the pod spec", workloadContainerName)
}
