package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

func envValue(container corev1.Container, name string) (string, bool) {
	for _, e := range container.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func jobWithInput(raw string, env map[string]string) *computev1alpha1.EphemeralJob {
	ej := &computev1alpha1.EphemeralJob{}
	ej.Spec.Image = "example:dev"
	ej.Spec.Env = env
	if raw != "" {
		ej.Spec.Input = &runtime.RawExtension{Raw: []byte(raw)}
	}
	return ej
}

// A workload that is given no input cannot do the work it was asked to do, and
// says so by failing — after the platform has already paid for the capacity.
// spec.input exists to prevent that, so it has to reach the container.
func TestWorkloadInputReachesTheContainer(t *testing.T) {
	r := &EphemeralJobReconciler{}
	raw := `{"s3_url":"https://example.test/request.json"}`

	got, ok := envValue(r.buildWorkloadContainer(jobWithInput(raw, nil)), envJobInput)
	if !ok {
		t.Fatalf("%s missing: spec.input was accepted and then dropped", envJobInput)
	}
	if got != raw {
		t.Errorf("input was reshaped in transit: got %q, want %q", got, raw)
	}
}

// The operator passes input through; it never reads inside it. Anything else
// would make the platform a party to each workload's private payload shape.
func TestWorkloadInputIsOpaque(t *testing.T) {
	r := &EphemeralJobReconciler{}
	for _, raw := range []string{`{"anything":{"nested":[1,2,3]}}`, `"a bare string"`, `[]`} {
		got, ok := envValue(r.buildWorkloadContainer(jobWithInput(raw, nil)), envJobInput)
		if !ok || got != raw {
			t.Errorf("input %s did not survive: got %q (present=%v)", raw, got, ok)
		}
	}
}

func TestStatedEnvWinsOverDerivedInput(t *testing.T) {
	r := &EphemeralJobReconciler{}
	c := r.buildWorkloadContainer(jobWithInput(`{"derived":true}`, map[string]string{envJobInput: "stated"}))

	count := 0
	for _, e := range c.Env {
		if e.Name == envJobInput {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%s appears %d times; a duplicate env var is resolved by position, not intent", envJobInput, count)
	}
	if got, _ := envValue(c, envJobInput); got != "stated" {
		t.Errorf("derived value overrode the stated one: got %q", got)
	}
}

// A workload writes its own result, so it has to be told where. Nothing carried
// spec.output to the container, and results landed at the bucket root.
func TestWorkloadOutputReachesTheContainer(t *testing.T) {
	r := &EphemeralJobReconciler{}
	ej := jobWithInput(`{"s3_url":"https://example.test/request.json"}`, nil)
	ej.Spec.Output = &computev1alpha1.OutputSpec{ObjectPrefix: "sessions/s1/"}

	got, ok := envValue(r.buildWorkloadContainer(ej), envJobOutput)
	if !ok {
		t.Fatalf("%s missing: spec.output was accepted and then dropped", envJobOutput)
	}
	if !strings.Contains(got, "sessions/s1/") {
		t.Errorf("output prefix did not survive: got %q", got)
	}
}

func TestNoOutputAddsNoVariable(t *testing.T) {
	r := &EphemeralJobReconciler{}
	if _, ok := envValue(r.buildWorkloadContainer(jobWithInput(`{}`, nil)), envJobOutput); ok {
		t.Errorf("%s set for a job that declared no output", envJobOutput)
	}
}

func TestNoInputAddsNoVariable(t *testing.T) {
	r := &EphemeralJobReconciler{}
	if _, ok := envValue(r.buildWorkloadContainer(jobWithInput("", nil)), envJobInput); ok {
		t.Errorf("%s set for a job with no input; empty is not the same as absent", envJobInput)
	}
}
