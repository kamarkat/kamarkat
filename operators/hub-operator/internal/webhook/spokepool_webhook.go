// trigger-ci
package webhook

import (
	"context"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/infisical"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	providerRegistryConfigMap = "hub-operator-provider-registry"
	providerRegistryNamespace = infisical.NamespaceOps
)

var (
	providerRegistry   map[string]string
	providerRegistryMu sync.RWMutex
)

// LoadProviderRegistry reads the provider→composition mapping from a ConfigMap.
// Call once at startup before registering webhooks.
func LoadProviderRegistry(ctx context.Context, c client.Client) error {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: providerRegistryConfigMap, Namespace: providerRegistryNamespace,
	}, cm); err != nil {
		return fmt.Errorf("failed to load provider registry ConfigMap %s/%s: %w",
			providerRegistryNamespace, providerRegistryConfigMap, err)
	}

	providerRegistryMu.Lock()
	defer providerRegistryMu.Unlock()
	providerRegistry = make(map[string]string, len(cm.Data))
	for provider, composition := range cm.Data {
		if provider != "" && composition != "" {
			providerRegistry[provider] = composition
		}
	}
	return nil
}

func getComposition(provider string) (string, bool) {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	c, ok := providerRegistry[provider]
	return c, ok
}

func validProviders() []string {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	keys := make([]string, 0, len(providerRegistry))
	for k := range providerRegistry {
		keys = append(keys, k)
	}
	return keys
}

// ──────────────────────────────────────────────────────────────────────────────
// SpokePoolDefaulter — mutating admission webhook
// ──────────────────────────────────────────────────────────────────────────────
//
// Injects compositionSelector.matchLabels based on spec.provider so that
// fleet-registry never exposes Crossplane implementation details.
//
// Also rejects user-supplied compositionSelector — the platform owns this field.
//
// +kubebuilder:webhook:path=/mutate-nutgraf-in-v1alpha1-spokepool,mutating=true,failurePolicy=fail,groups=nutgraf.in,resources=spokepools,verbs=create;update,versions=v1alpha1,name=mspokepool.kb.io,admissionReviewVersions=v1,sideEffects=None,matchPolicy=Equivalent

type SpokePoolDefaulter struct{}

var _ ctrlwebhook.CustomDefaulter = &SpokePoolDefaulter{}

func (d *SpokePoolDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
	}

	provider, found, err := unstructured.NestedString(u.Object, "spec", "provider")
	if err != nil || !found || provider == "" {
		return nil
	}

	composition, exists := getComposition(provider)
	if !exists {
		return nil
	}

	compositionLabels := map[string]interface{}{
		"provider":     provider,
		"cluster-type": "spoke-pool",
	}
	compositionRef := map[string]interface{}{
		"matchLabels": compositionLabels,
	}

	if err := unstructured.SetNestedField(u.Object, compositionRef, "spec", "compositionSelector"); err != nil {
		return fmt.Errorf("failed to set compositionSelector: %w", err)
	}

	if err := unstructured.SetNestedField(u.Object, provider, "status", "resolvedProvider"); err != nil {
		return fmt.Errorf("failed to set resolvedProvider: %w", err)
	}
	if err := unstructured.SetNestedField(u.Object, composition, "status", "resolvedComposition"); err != nil {
		return fmt.Errorf("failed to set resolvedComposition: %w", err)
	}

	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// SpokePoolValidator — validating admission webhook
// ──────────────────────────────────────────────────────────────────────────────
//
// Rejects user-supplied compositionSelector (platform-owned field) and
// validates spec.provider against the registered provider catalog.
//
// +kubebuilder:webhook:path=/validate-nutgraf-in-v1alpha1-spokepool,mutating=false,failurePolicy=fail,groups=nutgraf.in,resources=spokepools,verbs=create;update,versions=v1alpha1,name=vspokepool.kb.io,admissionReviewVersions=v1,sideEffects=None,matchPolicy=Equivalent

type SpokePoolValidator struct{}

var _ ctrlwebhook.CustomValidator = &SpokePoolValidator{}

func (v *SpokePoolValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *SpokePoolValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *SpokePoolValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *SpokePoolValidator) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
	}

	provider, found, err := unstructured.NestedString(u.Object, "spec", "provider")
	if err != nil || !found || provider == "" {
		return nil, fmt.Errorf("spec.provider is required")
	}

	if _, exists := getComposition(provider); !exists {
		valid := validProviders()
		return nil, fmt.Errorf("unsupported provider %q: valid providers are [%s]",
			provider, strings.Join(valid, ", "))
	}

	return nil, nil
}
