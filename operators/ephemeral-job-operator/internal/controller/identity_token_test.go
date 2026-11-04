package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// A sandbox runs tenant-authored agent code, so it must be able to authenticate
// to a service without holding a shared secret (ADR-052). These pin the four
// properties that make a projected token different from a credential in Env --
// each of which fails by producing a WORKING token with weaker guarantees, which
// no integration test would notice.

func ejWithIdentity(it *computev1alpha1.IdentityToken) *computev1alpha1.EphemeralJob {
	return &computev1alpha1.EphemeralJob{
		Spec: computev1alpha1.EphemeralJobSpec{
			Image:         "example.com/img@sha256:" + strings.Repeat("a", 64),
			IdentityToken: it,
			Sidecars: []corev1.Container{
				{Name: "agent-vault", Image: "example.com/vault@sha256:" + strings.Repeat("b", 64)},
			},
		},
	}
}

func identityVolume(spec corev1.PodSpec) *corev1.Volume {
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == identityTokenVolumeName {
			return &spec.Volumes[i]
		}
	}
	return nil
}

func hasIdentityMount(c corev1.Container) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == identityTokenVolumeName {
			return true
		}
	}
	return false
}

// The audience IS the security boundary. A token minted without one is accepted
// by the Kubernetes API server, which turns an identity into a cluster credential
// inside a sandbox.
func TestIdentityTokenIsAudienceBound(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	ej := ejWithIdentity(&computev1alpha1.IdentityToken{Audience: "waypoint-sdk"})

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})

	v := identityVolume(spec)
	if v == nil || v.Projected == nil || len(v.Projected.Sources) != 1 {
		t.Fatalf("no projected identity volume: %+v", spec.Volumes)
	}
	sat := v.Projected.Sources[0].ServiceAccountToken
	if sat == nil {
		t.Fatal("projection is not a ServiceAccountToken")
	}
	if sat.Audience != "waypoint-sdk" {
		t.Fatalf("audience = %q, want waypoint-sdk", sat.Audience)
	}
	if sat.Path != identityTokenFileName {
		t.Fatalf("path = %q, want %q", sat.Path, identityTokenFileName)
	}
}

// The sidecars must NOT get it. agent-vault proxies outbound calls for
// tenant-authored code; an identity a receiver accepts would make the proxy a
// caller in its own right.
func TestIdentityTokenReachesTheWorkloadAndNotTheSidecars(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	ej := ejWithIdentity(&computev1alpha1.IdentityToken{Audience: "waypoint-sdk"})

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})

	var workload *corev1.Container
	for i := range spec.Containers {
		if spec.Containers[i].Name == "workload" {
			workload = &spec.Containers[i]
		}
	}
	if workload == nil {
		t.Fatal("no workload container")
	}
	if !hasIdentityMount(*workload) {
		t.Fatalf("workload has no identity mount: %+v", workload.VolumeMounts)
	}
	for _, c := range spec.Containers {
		if c.Name == "workload" {
			continue
		}
		if hasIdentityMount(c) {
			t.Fatalf("sidecar %q was given the identity token", c.Name)
		}
	}
}

// The mount is read-only: the kubelet rewrites the file in place on renewal, and
// nothing in the pod should be writing there.
func TestIdentityTokenIsMountedReadOnlyAtThePlatformPath(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	ej := ejWithIdentity(&computev1alpha1.IdentityToken{Audience: "waypoint-sdk"})

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.Name != identityTokenVolumeName {
			continue
		}
		if m.MountPath != IdentityTokenMountPath {
			t.Fatalf("mount path = %q, want %q", m.MountPath, IdentityTokenMountPath)
		}
		if !m.ReadOnly {
			t.Fatal("identity token is mounted writable")
		}
		return
	}
	t.Fatal("identity mount not found")
}

// The automounted token has NO audience and works against the API server. That
// is exactly what must not be inside a sandbox, and a projected volume is not a
// reason to turn automount back on.
func TestIdentityTokenDoesNotReEnableAutomount(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	ej := ejWithIdentity(&computev1alpha1.IdentityToken{Audience: "waypoint-sdk"})

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("automountServiceAccountToken is not false")
	}
}

// 600s is the API server's floor for a projected token, not a preference: asking
// for less is a pod that never starts rather than a shorter-lived credential.
func TestIdentityTokenExpiryDefaultsToTheApiServerFloor(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")

	ej := ejWithIdentity(&computev1alpha1.IdentityToken{Audience: "waypoint-sdk"})
	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})
	got := identityVolume(spec).Projected.Sources[0].ServiceAccountToken.ExpirationSeconds
	if got == nil || *got != identityTokenDefaultExpirySeconds {
		t.Fatalf("default expiry = %v, want %d", got, identityTokenDefaultExpirySeconds)
	}

	longer := int64(1800)
	ej = ejWithIdentity(&computev1alpha1.IdentityToken{Audience: "waypoint-sdk", ExpirationSeconds: &longer})
	spec = r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})
	got = identityVolume(spec).Projected.Sources[0].ServiceAccountToken.ExpirationSeconds
	if got == nil || *got != 1800 {
		t.Fatalf("explicit expiry = %v, want 1800", got)
	}
}

// Absent unless asked for. Every existing sandbox keeps the pod spec it had.
func TestNoIdentityTokenWhenNotRequested(t *testing.T) {
	r := &EphemeralJobReconciler{}
	p, _ := ResolvePlacement("home")
	ej := ejWithIdentity(nil)

	spec := r.buildPodSpec(context.Background(), ej, p, corev1.Container{Name: "workload"})
	if identityVolume(spec) != nil {
		t.Fatal("projected identity volume created without a request")
	}
	for _, c := range spec.Containers {
		if hasIdentityMount(c) {
			t.Fatalf("container %q got an identity mount without a request", c.Name)
		}
	}
}
