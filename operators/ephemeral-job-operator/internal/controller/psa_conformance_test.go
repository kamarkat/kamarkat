package controller

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// Emits a workspace-bearing pod exactly as the reconciler builds one, so it can
// be judged by a real API server rather than by this package's own assertions.
//
// The unit tests beside this one assert each Restricted requirement in turn,
// which is necessary and not sufficient: §14.2 shipped believing the sidecar's
// privilege was contained, and no assertion here would have disagreed. Only the
// admission controller in a namespace that enforces restricted settles it.
//
//	PSA_DUMP=/tmp/pod.yaml go test ./internal/controller/ -run PSAConformance
//	kubectl -n <a restricted namespace> apply --dry-run=server -f /tmp/pod.yaml
//
// Expected: "pod/psa-probe created (server dry run)". Anything else names the
// container and the rule, which is the error this whole amendment exists to
// stop discovering in production.
func TestPSAConformanceDump(t *testing.T) {
	out := os.Getenv("PSA_DUMP")
	if out == "" {
		t.Skip("set PSA_DUMP=<path> to emit the pod")
	}
	r := &EphemeralJobReconciler{}
	ej := &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{Name: "psa-probe", Namespace: "tenant-nutgraf-waypoint"},
		Spec: computev1alpha1.EphemeralJobSpec{
			Image: "ghcr.io/example/harness@sha256:" +
				"0000000000000000000000000000000000000000000000000000000000000000",
			WorkspacePersistence: &computev1alpha1.WorkspacePersistenceSpec{
				WorkspaceID:       "psa-probe",
				AppID:             "psa-probe",
				SharedWorkspaceID: "globals",
			},
		},
	}
	placement, _ := ResolvePlacement("home")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "psa-probe",
		Namespace: "tenant-nutgraf-waypoint",
		Labels: map[string]string{
			"tenant-id": "nutgraf", "app-id": "waypoint", "cost-center": "platform",
		},
	}}
	// The workload container as the reconciler actually builds it
	// (ephemeraljob_controller.go), not a bare stub -- otherwise the dump
	// fails PSA on the harness's own omissions rather than on the sidecars.
	pod.Spec = r.buildPodSpec(context.Background(), ej, placement, corev1.Container{
		Name:  "workload",
		Image: ej.Spec.Image,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			RunAsNonRoot:             ptr(true),
			ReadOnlyRootFilesystem:   ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	})
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	p := pod
	b, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}
