// trigger-ci
package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/infisical"
)

const (
	providerRegistryConfigMap = "hub-operator-provider-registry"
	providerRegistryNamespace = infisical.NamespaceOps

	// compositionGroup identifies the Crossplane Composition the registry names.
	// Read as unstructured so this operator does not take a dependency on
	// Crossplane's Go API for one field.
	compositionGroup   = "apiextensions.crossplane.io"
	compositionVersion = "v1"
	compositionKind    = "Composition"
)

var (
	providerRegistry   map[string]string
	registryLoaded     bool
	providerRegistryMu sync.RWMutex
)

// LoadProviderRegistry reads the provider→composition mapping from a ConfigMap.
//
// Called once, from main, BEFORE the manager starts, which is why it takes a
// client.Reader: the manager's cached client cannot serve a read until its cache
// has started, and this value is needed by the first admission request.
//
// Once is correct rather than a limitation. The Deployment carries
// reloader.stakater.com/auto, so editing the ConfigMap restarts the pod, and the
// restart IS the reload. A watch here would be a second reload mechanism for the
// same fact.
func LoadProviderRegistry(ctx context.Context, c client.Reader) error {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: providerRegistryConfigMap, Namespace: providerRegistryNamespace,
	}, cm); err != nil {
		return fmt.Errorf("failed to load provider registry ConfigMap %s/%s: %w",
			providerRegistryNamespace, providerRegistryConfigMap, err)
	}

	loaded := make(map[string]string, len(cm.Data))
	for provider, composition := range cm.Data {
		if provider != "" && composition != "" {
			loaded[provider] = composition
		}
	}
	if len(loaded) == 0 {
		return fmt.Errorf("provider registry ConfigMap %s/%s declares no provider→composition "+
			"mapping, so no workload cluster could be admitted",
			providerRegistryNamespace, providerRegistryConfigMap)
	}

	providerRegistryMu.Lock()
	defer providerRegistryMu.Unlock()
	providerRegistry = loaded
	registryLoaded = true
	return nil
}

func getComposition(provider string) (string, bool) {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	c, ok := providerRegistry[provider]
	return c, ok
}

func registryIsLoaded() bool {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	return registryLoaded
}

func validProviders() []string {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	keys := make([]string, 0, len(providerRegistry))
	for k := range providerRegistry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ──────────────────────────────────────────────────────────────────────────────
// SpokePoolDefaulter — mutating admission webhook
// ──────────────────────────────────────────────────────────────────────────────
//
// Resolves spec.provider to ONE EXACT Composition and writes it to
// spec.crossplane.compositionRef, so the tenant's repository never names a
// Crossplane implementation.
//
// WHY AN EXACT REFERENCE AND NOT A SELECTOR. With no reference set, Crossplane
// lists every Composition matching the selector's labels -- an EMPTY label set
// when no selector is present, which matches all of them -- filters to those
// whose compositeTypeRef is SpokePool, and picks one at random
// (crossplane-runtime v2.4.3, APILabelSelectorResolver.SelectComposition:
// `candidates[random.Intn(len(candidates))]`). A box installs only its own
// provider's composition (03-platform-services-appset.yaml syncs
// manifests/providers/<box provider>), so in practice that single candidate wins
// regardless of what the cluster declared: on a hybrid box, a cluster declaring
// provider: hetzner was composed as hybrid -- burst-worker instead of
// default-worker, no replicas patch, no general capacity, and a home-worker join
// config for hardware nobody declared. Deterministic, silent, and wrong.
//
// A reference removes the choice: SelectComposition returns immediately when one
// is set, and a reference naming a composition the box does not carry is reported
// as missing rather than silently substituted.
//
// WHY spec.crossplane AND NOT spec. The SpokePool XRD is
// apiextensions.crossplane.io/v2 with scope: Cluster, which Crossplane calls a
// modern XR, and a modern XR's machinery fields live under spec.crossplane
// (crossplane-runtime v2.4.3 pkg/xcrd/schemas.go, "Modern XRs nest their
// Crossplane machinery fields under spec.crossplane"; pkg/resource/unstructured/
// composite reads spec.crossplane.compositionRef unless the schema is legacy).
// This wrote spec.compositionSelector until 2026-11-22, which the generated CRD
// does not declare, so the API server pruned it at decode and Crossplane saw no
// selection at all.
//
// NO status WRITE. A mutating response to a create or update of the main resource
// cannot establish status: the generated CRD always carries the status subresource
// (xcrd.genCrdVersion), and the API server deletes status on create and restores
// the old status on update (apiextensions-apiserver customResourceStrategy). The
// two status.resolved* writes that used to be here never persisted once.
//
// +kubebuilder:webhook:path=/mutate-nutgraf-in-v1alpha1-spokepool,mutating=true,failurePolicy=fail,groups=nutgraf.in,resources=spokepools,verbs=create;update,versions=v1alpha1,name=mspokepool.kb.io,admissionReviewVersions=v1,sideEffects=None,matchPolicy=Equivalent

type SpokePoolDefaulter struct{}

var _ admission.Defaulter[*unstructured.Unstructured] = &SpokePoolDefaulter{}

func (d *SpokePoolDefaulter) Default(ctx context.Context, u *unstructured.Unstructured) error {
	provider, found, err := unstructured.NestedString(u.Object, "spec", "provider")
	if err != nil || !found || provider == "" {
		// Nothing to resolve from. The validator refuses it with the reason, and
		// the XRD requires the field; inventing a provider here is the one thing
		// that would make an unprovidered claim provision something.
		return nil
	}

	composition, exists := getComposition(provider)
	if !exists {
		// An unsupported provider is the validator's refusal, not a silent no-op
		// with a reference missing: the XRD's CEL rule refuses the object without
		// one, so this cannot fall through to Crossplane's own selection.
		return nil
	}

	// An established reference is not overwritten. A SpokePool already composed by
	// one Composition must not be switched to another by an edit to its provider:
	// that would replace a running cluster's infrastructure. The validator refuses
	// the conflict and names both sides.
	existing, _, err := unstructured.NestedString(u.Object, "spec", "crossplane", "compositionRef", "name")
	if err == nil && existing != "" && existing != composition {
		return nil
	}

	if err := unstructured.SetNestedField(u.Object, composition,
		"spec", "crossplane", "compositionRef", "name"); err != nil {
		return fmt.Errorf("failed to set spec.crossplane.compositionRef.name: %w", err)
	}

	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// SpokePoolValidator — validating admission webhook
// ──────────────────────────────────────────────────────────────────────────────
//
// Enforces the FINAL state: spec.provider is supported, the registry's
// composition for it exists and composes SpokePool, and the object's reference is
// that composition and nothing else.
//
// It judges the final object deliberately. Mutating admission runs first, so a
// canonical value here is indistinguishable from one the platform just injected --
// and it does not need to be distinguished, because the canonical state is the
// contract. What a validator CANNOT see is an undeclared field: pruning happens
// when the request is decoded, before admission (Kubernetes v1.36
// apiextensions-apiserver customresource_handler.go wraps the request decoder in
// schemaCoercingDecoder), so a tenant's spec.compositionSelector is already gone
// by the time this runs. Legacy paths are caught in the rendered source instead,
// by scripts/validate/preflight/046-spokepool-composition-contract.py.
//
// The validator reads the Composition the registry names. Declared as a marker
// rather than written into config/rbac/role.yaml, because that file is
// controller-gen's output: `make manifests` regenerates it from the markers in
// this tree and discards anything added by hand, which is what happened to this
// rule on its first CI run.
//
// +kubebuilder:rbac:groups=apiextensions.crossplane.io,resources=compositions,verbs=get;list;watch
// +kubebuilder:webhook:path=/validate-nutgraf-in-v1alpha1-spokepool,mutating=false,failurePolicy=fail,groups=nutgraf.in,resources=spokepools,verbs=create;update,versions=v1alpha1,name=vspokepool.kb.io,admissionReviewVersions=v1,sideEffects=None,matchPolicy=Equivalent

type SpokePoolValidator struct {
	// reader reads the named Composition directly from the API server. Uncached:
	// a Composition is read once per SpokePool admission, which is a handful of
	// times in a box's life, and an informer for it would cost memory on every
	// hub for that.
	reader client.Reader
}

// NewSpokePoolValidator returns a validator that resolves Compositions with the
// given reader.
func NewSpokePoolValidator(reader client.Reader) *SpokePoolValidator {
	return &SpokePoolValidator{reader: reader}
}

var _ admission.Validator[*unstructured.Unstructured] = &SpokePoolValidator{}

func (v *SpokePoolValidator) ValidateCreate(ctx context.Context, obj *unstructured.Unstructured) (admission.Warnings, error) {
	return v.validate(ctx, obj, nil)
}

func (v *SpokePoolValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *unstructured.Unstructured) (admission.Warnings, error) {
	return v.validate(ctx, newObj, oldObj)
}

func (v *SpokePoolValidator) ValidateDelete(ctx context.Context, obj *unstructured.Unstructured) (admission.Warnings, error) {
	return nil, nil
}

func (v *SpokePoolValidator) validate(ctx context.Context, u, old *unstructured.Unstructured) (admission.Warnings, error) {
	if !registryIsLoaded() {
		return nil, fmt.Errorf("the provider registry was not loaded, so this cluster's provider "+
			"cannot be resolved to a composition: check %s/%s and hub-operator's startup log",
			providerRegistryNamespace, providerRegistryConfigMap)
	}

	provider, found, err := unstructured.NestedString(u.Object, "spec", "provider")
	if err != nil || !found || strings.TrimSpace(provider) == "" {
		return nil, fmt.Errorf("spec.provider is required: it is what the platform resolves to the " +
			"composition that builds this cluster")
	}

	composition, exists := getComposition(provider)
	if !exists {
		return nil, fmt.Errorf("unsupported provider %q: this box can build %s",
			provider, strings.Join(validProviders(), ", "))
	}

	// The provider is this cluster's infrastructure identity. Changing it means a
	// different machine class, a different control-plane topology and a different
	// storage class, which is a new cluster rather than an edit -- as the XRD's own
	// description says. The XRD carries a CEL transition rule for this so the API
	// server refuses it whether or not this webhook is reachable; the same refusal
	// is made here so the message names both values.
	if old != nil {
		was, had, _ := unstructured.NestedString(old.Object, "spec", "provider")
		if had && was != "" && was != provider {
			return nil, fmt.Errorf("spec.provider cannot change from %q to %q: it selects the "+
				"composition that built this cluster, so changing it would replace the "+
				"cluster's infrastructure. Declare a new cluster and remove this one",
				was, provider)
		}
	}

	if err := v.compositionComposesSpokePool(ctx, u, composition, provider); err != nil {
		return nil, err
	}

	ref, _, err := unstructured.NestedString(u.Object, "spec", "crossplane", "compositionRef", "name")
	if err != nil {
		return nil, fmt.Errorf("reading spec.crossplane.compositionRef.name: %w", err)
	}
	switch {
	case ref == "":
		return nil, fmt.Errorf("spec.crossplane.compositionRef.name is empty: the platform resolves "+
			"provider %q to composition %q and injects it, so an empty reference means this "+
			"object did not pass through hub-operator's mutating webhook", provider, composition)
	case ref != composition:
		return nil, fmt.Errorf("spec.crossplane.compositionRef.name is %q while provider %q resolves "+
			"to %q: the composition is the platform's to choose. Declare the provider you want",
			ref, provider, composition)
	}

	// A selector alongside an exact reference is a second declaration of the same
	// choice. Nothing writes one: Crossplane's resolver returns before touching the
	// selector once a reference is set, and this operator never writes one.
	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "crossplane", "compositionSelector"); found {
		return nil, fmt.Errorf("spec.crossplane.compositionSelector is set alongside " +
			"compositionRef: the platform selects the composition by exact name. Remove it")
	}
	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "crossplane", "compositionRevisionSelector"); found {
		return nil, fmt.Errorf("spec.crossplane.compositionRevisionSelector is set: revisions of the " +
			"platform's composition are not the tenant's to narrow. Remove it")
	}

	// compositionUpdatePolicy, and NOT compositionRevisionRef. Crossplane writes
	// the revision reference itself on every revision change, and under the default
	// Automatic policy it resolves that reference by listing only revisions labelled
	// crossplane.io/composition-name=<the referenced composition>, so a revision
	// reference can never leave the canonical composition. Manual is the one setting
	// that escapes it: it makes Crossplane fetch whatever revision is referenced and
	// compose from that. Refusing Manual closes the bypass without refusing a field
	// Crossplane owns -- refusing that one would block its own update and the XR
	// would never reconcile.
	policy, found, _ := unstructured.NestedString(u.Object, "spec", "crossplane", "compositionUpdatePolicy")
	if found && policy == "Manual" {
		return nil, fmt.Errorf("spec.crossplane.compositionUpdatePolicy: Manual pins this cluster to " +
			"one revision of its composition, so a platform upgrade would not reach it. The " +
			"platform owns the composition's revisions")
	}

	return nil, nil
}

// compositionComposesSpokePool establishes that the registry's composition exists
// on this box and composes THIS composite type.
//
// Both halves matter. A mapping to a composition the box does not carry is the
// hybrid-box case: the composition is delivered per provider, and a reference to
// an absent one would sit unresolved with the cluster never built. A composition
// whose compositeTypeRef is some other kind would be selected and compose nothing
// that belongs to a SpokePool.
func (v *SpokePoolValidator) compositionComposesSpokePool(ctx context.Context, u *unstructured.Unstructured, composition, provider string) error {
	comp := &unstructured.Unstructured{}
	comp.SetGroupVersionKind(schema.GroupVersionKind{
		Group: compositionGroup, Version: compositionVersion, Kind: compositionKind,
	})
	if err := v.reader.Get(ctx, types.NamespacedName{Name: composition}, comp); err != nil {
		return fmt.Errorf("provider %q resolves to composition %q, which this box cannot read: %w. "+
			"A box carries the compositions of the providers it can build workload clusters on",
			provider, composition, err)
	}

	wantAPIVersion, wantKind := u.GroupVersionKind().ToAPIVersionAndKind()
	gotAPIVersion, _, _ := unstructured.NestedString(comp.Object, "spec", "compositeTypeRef", "apiVersion")
	gotKind, _, _ := unstructured.NestedString(comp.Object, "spec", "compositeTypeRef", "kind")
	if gotAPIVersion != wantAPIVersion || gotKind != wantKind {
		return fmt.Errorf("composition %q composes %s %s, not %s %s, so it cannot build this cluster",
			composition, gotAPIVersion, gotKind, wantAPIVersion, wantKind)
	}

	return nil
}
