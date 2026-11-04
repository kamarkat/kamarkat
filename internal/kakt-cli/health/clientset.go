package health

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// isNotFound reports whether the error indicates a missing resource.
func isNotFound(err error) bool {
	return k8serrors.IsNotFound(err)
}

// ClientsetChecker is a base helper for checkers that need typed access
// to the Kubernetes API. It encapsulates kubeconfig loading and client
// construction so concrete checkers can focus on the resource they're
// inspecting.
type ClientsetChecker struct {
	// checkName is the human-readable identifier for this check. The
	// Name() method returns it; the field is unexported so the public
	// contract is always the method.
	checkName string
	// Inspect is invoked with a fresh clientset on every poll. It must
	// return nil when the resource is healthy, or an error describing
	// the current state.
	Inspect func(ctx context.Context, clientset kubernetes.Interface) error
}

// Name returns the checker's identifier.
func (c *ClientsetChecker) Name() string { return c.checkName }

// Check builds a clientset and runs Inspect.
func (c *ClientsetChecker) Check(ctx context.Context, kubeconfig string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("%s: failed to load kubeconfig: %w", c.checkName, err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("%s: failed to create kubernetes client: %w", c.checkName, err)
	}
	return c.Inspect(ctx, clientset)
}

// WorkloadHealth is a HealthChecker that waits for a Deployment or
// StatefulSet to have at least one Available replica. It tries StatefulSet
// first (for legacy charts) and falls back to Deployment (for current
// helm-chart-managed workloads).
//
// Per ADR-022 (Stable-but-Not-Ready Application Semantics), we do NOT
// require the full desired replica count to be Ready — that creates a
// fatal bottleneck when ArgoCD is mid-reconciliation (e.g., the multi-
// source race where the chart briefly renders with upstream defaults
// before our valueFiles resolve). The only question this check needs
// to answer is: "Is at least one pod Available so the REST API can be
// reached?"
type WorkloadHealth struct {
	// WorkloadName is the workload's metadata.name.
	WorkloadName string
	// Namespace is the workload's metadata.namespace.
	Namespace string
}

// NewWorkloadHealth returns a checker for the given workload.
func NewWorkloadHealth(name, namespace string) *WorkloadHealth {
	return &WorkloadHealth{WorkloadName: name, Namespace: namespace}
}

// Name returns the checker identifier.
func (w *WorkloadHealth) Name() string {
	return fmt.Sprintf("workload %s/%s", w.Namespace, w.WorkloadName)
}

// Check probes the workload for Available replicas.
func (w *WorkloadHealth) Check(ctx context.Context, kubeconfig string) error {
	checker := &ClientsetChecker{
		checkName: w.Name(),
		Inspect: func(ctx context.Context, cs kubernetes.Interface) error {
			sts, err := cs.AppsV1().StatefulSets(w.Namespace).Get(ctx, w.WorkloadName, metav1.GetOptions{})
			if err == nil {
				if sts.Status.ReadyReplicas > 0 {
					return nil
				}
				return fmt.Errorf("StatefulSet %d/%d replicas ready",
					sts.Status.ReadyReplicas, sts.Status.Replicas)
			}
			if !isNotFound(err) {
				return fmt.Errorf("get StatefulSet: %w", err)
			}
			dep, err := cs.AppsV1().Deployments(w.Namespace).Get(ctx, w.WorkloadName, metav1.GetOptions{})
			if err != nil {
				if isNotFound(err) {
					return fmt.Errorf("workload not found")
				}
				return fmt.Errorf("get Deployment: %w", err)
			}
			if dep.Status.AvailableReplicas > 0 {
				return nil
			}
			return fmt.Errorf("Deployment %d/%d replicas available",
				dep.Status.AvailableReplicas, dep.Status.Replicas)
		},
	}
	return checker.Check(ctx, kubeconfig)
}

// SecretKeyHealth is a HealthChecker that waits for a Secret to exist
// and contain the required non-empty data keys.
type SecretKeyHealth struct {
	Namespace    string
	SecretName   string
	RequiredKeys []string
}

// Name returns the checker identifier.
func (s *SecretKeyHealth) Name() string {
	return fmt.Sprintf("secret %s/%s", s.Namespace, s.SecretName)
}

// Check probes the Secret.
func (s *SecretKeyHealth) Check(ctx context.Context, kubeconfig string) error {
	checker := &ClientsetChecker{
		checkName: s.Name(),
		Inspect: func(ctx context.Context, cs kubernetes.Interface) error {
			secret, err := cs.CoreV1().Secrets(s.Namespace).Get(ctx, s.SecretName, metav1.GetOptions{})
			if err != nil {
				if isNotFound(err) {
					return fmt.Errorf("secret not found")
				}
				return fmt.Errorf("get secret: %w", err)
			}
			for _, k := range s.RequiredKeys {
				if len(secret.Data[k]) == 0 {
					return fmt.Errorf("required key %q missing or empty", k)
				}
			}
			return nil
		},
	}
	return checker.Check(ctx, kubeconfig)
}

// NewSecretKeyHealth is a constructor that returns a SecretKeyHealth
// configured to require all listed keys.
func NewSecretKeyHealth(namespace, name string, keys ...string) *SecretKeyHealth {
	return &SecretKeyHealth{Namespace: namespace, SecretName: name, RequiredKeys: keys}
}
