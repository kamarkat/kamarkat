package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

const jobTestNamespace = "tenant-acme-waypoint"

func jobTestReconciler(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) (*EphemeralJobReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(computev1alpha1.AddToScheme(s))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   jobTestNamespace,
		Labels: map[string]string{"tenant-id": "acme", fleetIDLabel: "waypoint"},
	}}
	// The fleet ConfigMap every fleet namespace carries: the job's storage
	// endpoint, and (ADR-106) the host its storage egress is granted to.
	cfg := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: jobTestNamespace, Name: resolveWorkspaceSyncConfig(jobTestNamespace, "waypoint")},
		Data:       map[string]string{"S3_ENDPOINT_URL": "https://objects.example.test"},
	}
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, ns, cfg)...).
		WithStatusSubresource(&computev1alpha1.EphemeralJob{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	return &EphemeralJobReconciler{Client: c, Scheme: s, APIReader: c, ProvisioningBudget: 6 * time.Minute, NetworkPolicy: NetworkPolicyCilium}, c
}

func jobModeRequest(name string, age time.Duration) *computev1alpha1.EphemeralJob {
	return &computev1alpha1.EphemeralJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         jobTestNamespace,
			UID:               types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: computev1alpha1.EphemeralJobSpec{
			Mode:                    computev1alpha1.ModeJob,
			PlacementClass:          "burst",
			Image:                   "ghcr.io/soloz-io/audio-worker@sha256:" + sha64,
			Output:                  &computev1alpha1.OutputSpec{ObjectPrefix: "app/session/artifacts/audio/"},
			TimeoutSeconds:          600,
			MaxLifetimeSeconds:      28800,
			TTLSecondsAfterFinished: 3600,
		},
	}
}

const sha64 = "763a15dc1eb352ce5005ad3e12dc0efd546182fbae21d594917e792ea967facb"

func envByName(env []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		m[e.Name] = e
	}
	return m
}

// A job that writes a result reads the fleet's storage by reference -- the
// Secret and ConfigMap workspace-sync reads -- and no value enters the CR.
func TestBuildJobReadsStorageByReference(t *testing.T) {
	ej := jobModeRequest("audio-clean", time.Minute)
	r, _ := jobTestReconciler(t, nil, ej)
	p, _ := ResolvePlacement(ej.Spec.PlacementClass)

	job := r.buildJob(context.Background(), ej, jobNameFor(ej), p)
	workloadEnv := job.Spec.Template.Spec.Containers[0].Env
	env := envByName(workloadEnv)

	// Bound once: a second binding path gave Job mode every S3 variable twice.
	bound := map[string]int{}
	for _, e := range workloadEnv {
		bound[e.Name]++
	}
	for name, n := range bound {
		if n > 1 {
			t.Errorf("%s bound %d times in the workload env, want once", name, n)
		}
	}

	for _, tc := range []struct {
		name, object string
		secret       bool
		optional     bool
	}{
		{"S3_ENDPOINT_URL", "waypoint-config", false, false},
		{"S3_BUCKET_NAME", "waypoint-config", false, false},
		{"S3_ACCESS_KEY_ID", "waypoint-sdk-secrets", true, false},
		{"S3_SECRET_ACCESS_KEY", "waypoint-sdk-secrets", true, false},
		{"S3_REGION", "waypoint-config", false, true},
	} {
		e, ok := env[tc.name]
		if !ok {
			t.Fatalf("%s missing from the workload env", tc.name)
		}
		if e.Value != "" || e.ValueFrom == nil {
			t.Fatalf("%s must be a reference, got value %q", tc.name, e.Value)
		}
		var name string
		var optional *bool
		if tc.secret {
			if e.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("%s must come from a Secret", tc.name)
			}
			name, optional = e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Optional
		} else {
			if e.ValueFrom.ConfigMapKeyRef == nil {
				t.Fatalf("%s must come from a ConfigMap", tc.name)
			}
			name, optional = e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Optional
		}
		if name != tc.object {
			t.Errorf("%s reads %q, want %q", tc.name, name, tc.object)
		}
		if optional == nil || *optional != tc.optional {
			t.Errorf("%s optional = %v, want %v", tc.name, optional, tc.optional)
		}
	}
}

// require-cost-labels refuses a tenant Job without app-id; it comes from the
// namespace label, on the Job and its pods.
func TestBuildJobCarriesAppIDFromNamespace(t *testing.T) {
	ej := jobModeRequest("labelled", time.Minute)
	ej.Labels = map[string]string{"app-id": "spoofed"}
	r, _ := jobTestReconciler(t, nil, ej)
	p, _ := ResolvePlacement(ej.Spec.PlacementClass)

	job := r.buildJob(context.Background(), ej, jobNameFor(ej), p)
	for where, labels := range map[string]map[string]string{
		"job": job.Labels, "pod template": job.Spec.Template.Labels,
	} {
		if got := labels["app-id"]; got != "waypoint" {
			t.Errorf("%s app-id = %q, want the namespace's %q", where, got, "waypoint")
		}
		if labels["tenant-id"] == "" && where == "job" {
			t.Errorf("job lost tenant-id")
		}
	}
}

// A Job refused at admission fails the request with the API server's reason,
// once, instead of retrying forever with no phase.
func TestJobRefusedAtAdmissionFailsTheRequest(t *testing.T) {
	ej := jobModeRequest("refused", time.Minute)
	denial := apierrors.NewBadRequest(`admission webhook "validate.kyverno.svc-fail" denied the request: require-cost-labels`)
	funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			return denial
		}
		return c.Create(ctx, obj, opts...)
	}}
	r, c := jobTestReconciler(t, funcs, ej)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ej)})
	if err != nil {
		t.Fatalf("an admission refusal must not be returned for retry: %v", err)
	}
	if res.RequeueAfter > 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}
	var got computev1alpha1.EphemeralJob
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ej), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != computev1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	cond := findCondition(got.Status.Conditions, computev1alpha1.ConditionComplete)
	if cond == nil || cond.Reason != computev1alpha1.ReasonAdmissionRejected {
		t.Fatalf("Complete condition = %+v, want reason %s", cond, computev1alpha1.ReasonAdmissionRejected)
	}
	if got.Status.Message != denial.Error() {
		t.Fatalf("message = %q, want the API server's own", got.Status.Message)
	}
}

// A transient create failure is still retried.
func TestJobTransientCreateErrorIsRetried(t *testing.T) {
	ej := jobModeRequest("transient", time.Minute)
	funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			return apierrors.NewServerTimeout(batchv1.Resource("jobs"), "create", 1)
		}
		return c.Create(ctx, obj, opts...)
	}}
	r, _ := jobTestReconciler(t, funcs, ej)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ej)})
	if !apierrors.IsServerTimeout(err) {
		t.Fatalf("expected the timeout returned for retry, got %v", err)
	}
}

// Job mode honours maxLifetimeSeconds whatever state it is in.
func TestJobModeReapedAtLifetimeBound(t *testing.T) {
	ej := jobModeRequest("stuck", 45*time.Hour)
	r, c := jobTestReconciler(t, nil, ej)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ej)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got computev1alpha1.EphemeralJob
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ej), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != computev1alpha1.PhaseTimedOut {
		t.Fatalf("phase = %q, want TimedOut", got.Status.Phase)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(jobTestNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a request past its lifetime must not create a Job, found %d", len(jobs.Items))
	}
}

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
