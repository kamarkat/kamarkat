package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The admission contract, exercised through the two entry points the API server
// calls. These are the cases the three weeks between the Crossplane v2 migration
// and 2026-11-22 had no answer for; the live admission response and the persisted
// XR are established separately, on a box, because a passing test here says
// nothing about whether the webhook is registered.

const (
	spokePoolAPIVersion = "nutgraf.in/v1alpha1"
	hetznerComposition  = "spokepool-hetzner"
	hybridComposition   = "spokepool-hybrid"
)

func spokePool(provider string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": spokePoolAPIVersion,
		"kind":       "SpokePool",
		"metadata":   map[string]interface{}{"name": "acme-stg-01"},
		"spec": map[string]interface{}{
			"provider":          provider,
			"region":            "hel1",
			"maxTenantCapacity": int64(20),
		},
	}}
	return u
}

// loadedRegistry installs the mapping the ConfigMap carries, through the same
// function main calls, so a test cannot pass against a registry shape the
// operator does not read.
func loadedRegistry(t *testing.T, data map[string]string) {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerRegistryConfigMap,
			Namespace: providerRegistryNamespace,
		},
		Data: data,
	}).Build()
	if err := LoadProviderRegistry(context.Background(), c); err != nil {
		t.Fatalf("LoadProviderRegistry: %v", err)
	}
	t.Cleanup(func() {
		providerRegistryMu.Lock()
		providerRegistry = nil
		registryLoaded = false
		providerRegistryMu.Unlock()
	})
}

func composition(name, composesAPIVersion, composesKind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": compositionGroup + "/" + compositionVersion,
		"kind":       compositionKind,
		"metadata":   map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"compositeTypeRef": map[string]interface{}{
				"apiVersion": composesAPIVersion,
				"kind":       composesKind,
			},
		},
	}}
}

func validatorWith(objs ...*unstructured.Unstructured) *SpokePoolValidator {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: compositionGroup, Version: compositionVersion, Kind: compositionKind,
	}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: compositionGroup, Version: compositionVersion, Kind: compositionKind + "List",
	}, &unstructured.UnstructuredList{})
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return NewSpokePoolValidator(b.Build())
}

func compositionRef(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	ref, _, err := unstructured.NestedString(u.Object, "spec", "crossplane", "compositionRef", "name")
	if err != nil {
		t.Fatalf("reading the reference: %v", err)
	}
	return ref
}

// A declared provider resolves to its own composition, at the path a modern XR
// carries it -- not spec.compositionSelector, which the generated CRD does not
// declare and the API server prunes.
func TestTheDeclaredProviderResolvesToItsOwnComposition(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition, "hybrid": hybridComposition})

	for provider, want := range map[string]string{"hetzner": hetznerComposition, "hybrid": hybridComposition} {
		u := spokePool(provider)
		if err := (&SpokePoolDefaulter{}).Default(context.Background(), u); err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if got := compositionRef(t, u); got != want {
			t.Errorf("provider %q resolved to %q, want %q", provider, got, want)
		}
		if _, found, _ := unstructured.NestedMap(u.Object, "spec", "compositionSelector"); found {
			t.Errorf("provider %q: a legacy spec.compositionSelector was written; a "+
				"Cluster scoped XR carries its machinery under spec.crossplane and the "+
				"API server prunes this one before anything reads it", provider)
		}
		if _, found, _ := unstructured.NestedMap(u.Object, "status"); found {
			t.Errorf("provider %q: status was written, which a mutating response to a "+
				"create or update of the main resource cannot establish", provider)
		}
	}
}

// An unsupported provider is refused, and nothing is resolved for it. Both halves:
// a defaulter that invented a reference would provision something for a provider
// nobody supports.
func TestAnUnsupportedProviderIsRefusedAndResolvesToNothing(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition})

	u := spokePool("aws")
	if err := (&SpokePoolDefaulter{}).Default(context.Background(), u); err != nil {
		t.Fatalf("defaulting: %v", err)
	}
	if got := compositionRef(t, u); got != "" {
		t.Errorf("an unsupported provider resolved to %q", got)
	}

	_, err := validatorWith(composition(hetznerComposition, spokePoolAPIVersion, "SpokePool")).
		ValidateCreate(context.Background(), u)
	if err == nil {
		t.Fatal("an unsupported provider was admitted")
	}
	if !strings.Contains(err.Error(), "hetzner") {
		t.Errorf("the refusal does not say what this box can build: %v", err)
	}
}

// The registry's composition has to exist on this box. This is the hybrid-box
// case: the cells are delivered per the BOX's provider, so a mapping can name a
// composition the box has never seen, and a reference to it would leave the
// cluster undeclared forever rather than failing.
func TestAMappingToAnAbsentCompositionIsRefused(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition})

	u := spokePool("hetzner")
	if err := (&SpokePoolDefaulter{}).Default(context.Background(), u); err != nil {
		t.Fatalf("defaulting: %v", err)
	}
	if _, err := validatorWith().ValidateCreate(context.Background(), u); err == nil {
		t.Fatal("a reference to a composition this box does not carry was admitted")
	}
}

func TestACompositionForAnotherCompositeTypeIsRefused(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition})

	u := spokePool("hetzner")
	if err := (&SpokePoolDefaulter{}).Default(context.Background(), u); err != nil {
		t.Fatalf("defaulting: %v", err)
	}
	v := validatorWith(composition(hetznerComposition, spokePoolAPIVersion, "AINativeSaaS"))
	if _, err := v.ValidateCreate(context.Background(), u); err == nil {
		t.Fatal("a composition composing another kind was accepted as this cluster's builder")
	}
}

// A canonical object stays admissible. Crossplane updates the XR itself on every
// revision change, so an admission that refused its own injected result would
// stop the cluster reconciling.
func TestRepeatedAdmissionOfACanonicalSpokePoolIsAccepted(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition})
	v := validatorWith(composition(hetznerComposition, spokePoolAPIVersion, "SpokePool"))
	d := &SpokePoolDefaulter{}

	u := spokePool("hetzner")
	for i := 0; i < 3; i++ {
		if err := d.Default(context.Background(), u); err != nil {
			t.Fatalf("pass %d defaulting: %v", i, err)
		}
		if _, err := v.ValidateUpdate(context.Background(), u.DeepCopy(), u); err != nil {
			t.Fatalf("pass %d: a canonical SpokePool was refused: %v", i, err)
		}
		if got := compositionRef(t, u); got != hetznerComposition {
			t.Fatalf("pass %d: the reference became %q", i, got)
		}
	}

	// Crossplane's own write: it sets the revision reference and updates the XR.
	// Refusing a populated revision reference would block that update, and under
	// the default Automatic policy the revision is already constrained to this
	// composition's own revisions.
	if err := unstructured.SetNestedField(u.Object, hetznerComposition+"-a1b2c3d",
		"spec", "crossplane", "compositionRevisionRef", "name"); err != nil {
		t.Fatalf("setting the revision reference: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), u.DeepCopy(), u); err != nil {
		t.Fatalf("Crossplane's own revision reference was refused, so the XR could "+
			"never reconcile: %v", err)
	}
}

// A reference the platform did not choose is refused rather than overwritten. The
// defaulter leaves it, so the validator can name both sides -- silently rewriting
// it would replace a running cluster's infrastructure.
func TestAConflictingReferenceIsRefusedAndNotRewritten(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition, "hybrid": hybridComposition})

	u := spokePool("hetzner")
	if err := unstructured.SetNestedField(u.Object, hybridComposition,
		"spec", "crossplane", "compositionRef", "name"); err != nil {
		t.Fatalf("setting the reference: %v", err)
	}
	if err := (&SpokePoolDefaulter{}).Default(context.Background(), u); err != nil {
		t.Fatalf("defaulting: %v", err)
	}
	if got := compositionRef(t, u); got != hybridComposition {
		t.Errorf("an established reference was rewritten to %q; the refusal is the "+
			"validator's, so that it can say what was asked for and what was declared", got)
	}
	v := validatorWith(composition(hetznerComposition, spokePoolAPIVersion, "SpokePool"))
	_, err := v.ValidateCreate(context.Background(), u)
	if err == nil {
		t.Fatal("a composition the provider does not resolve to was admitted")
	}
	if !strings.Contains(err.Error(), hybridComposition) || !strings.Contains(err.Error(), hetznerComposition) {
		t.Errorf("the refusal names neither side: %v", err)
	}
}

func TestTheProviderCannotChangeOnAProvisionedSpokePool(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition, "hybrid": hybridComposition})
	v := validatorWith(
		composition(hetznerComposition, spokePoolAPIVersion, "SpokePool"),
		composition(hybridComposition, spokePoolAPIVersion, "SpokePool"),
	)
	d := &SpokePoolDefaulter{}

	was := spokePool("hybrid")
	if err := d.Default(context.Background(), was); err != nil {
		t.Fatalf("defaulting: %v", err)
	}
	now := spokePool("hetzner")
	if err := d.Default(context.Background(), now); err != nil {
		t.Fatalf("defaulting: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), was, now); err == nil {
		t.Fatal("the provider was changed on an existing SpokePool, which replaces a " +
			"running cluster's infrastructure")
	}
}

// A selector beside the exact reference, and a pinned revision policy: two ways
// to take the choice back from the platform.
func TestSelectorsAndAPinnedRevisionPolicyAreRefused(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition})
	v := validatorWith(composition(hetznerComposition, spokePoolAPIVersion, "SpokePool"))

	for _, tc := range []struct {
		name string
		set  func(*unstructured.Unstructured)
	}{
		{"compositionSelector", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"provider": "hetzner"},
				"spec", "crossplane", "compositionSelector", "matchLabels")
		}},
		{"compositionRevisionSelector", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"channel": "edge"},
				"spec", "crossplane", "compositionRevisionSelector", "matchLabels")
		}},
		{"compositionUpdatePolicy: Manual", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "Manual",
				"spec", "crossplane", "compositionUpdatePolicy")
		}},
	} {
		u := spokePool("hetzner")
		if err := (&SpokePoolDefaulter{}).Default(context.Background(), u); err != nil {
			t.Fatalf("%s: defaulting: %v", tc.name, err)
		}
		tc.set(u)
		if _, err := v.ValidateCreate(context.Background(), u); err == nil {
			t.Errorf("%s was admitted alongside the platform's exact reference", tc.name)
		}
	}
}

// Without the registry nothing is admitted, and the refusal says where to look.
// The alternative is an operator that serves admission and resolves every
// provider to nothing.
func TestWithoutTheRegistryNothingIsAdmitted(t *testing.T) {
	providerRegistryMu.Lock()
	providerRegistry = nil
	registryLoaded = false
	providerRegistryMu.Unlock()

	u := spokePool("hetzner")
	_, err := validatorWith().ValidateCreate(context.Background(), u)
	if err == nil {
		t.Fatal("a SpokePool was admitted with no provider registry loaded")
	}
	if !strings.Contains(err.Error(), providerRegistryConfigMap) {
		t.Errorf("the refusal does not name the registry: %v", err)
	}
}

func TestAnEmptyRegistryIsNotLoaded(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerRegistryConfigMap,
			Namespace: providerRegistryNamespace,
		},
	}).Build()
	if err := LoadProviderRegistry(context.Background(), c); err == nil {
		t.Fatal("a registry declaring no mapping loaded, so the operator would start " +
			"and refuse every workload cluster with a message about itself")
	}
}

// The handlers serve a real AdmissionRequest for a type NO Go scheme knows.
//
// This is the layer that broke on box 7 and that no test above reaches. A
// SpokePool is an unstructured XR whose CRD Crossplane generates from the XRD, so
// it is in no scheme -- and ctrl.NewWebhookManagedBy(...).Complete() ends in a
// conversion check that resolves the type THROUGH the scheme
// (conversion.objectGVKs keeps only GVKs in scheme.AllKnownTypes()). It found
// nothing, returned "error retrieving gvks for object", main exited, and the pod
// crash-looped 22 times while the previous ReplicaSet -- which registers no
// webhook -- stayed the only ready pod behind the Service. Everything above
// still passed.
//
// So this drives the registration main uses, against an EMPTY scheme, and asserts
// a patch comes back. A registration that cannot decode the request, or that
// refuses the type, fails here instead of in a rollout.
func TestTheRegisteredHandlersServeAnUnregisteredType(t *testing.T) {
	loadedRegistry(t, map[string]string{"hetzner": hetznerComposition})

	raw, err := json.Marshal(spokePool("hetzner").Object)
	if err != nil {
		t.Fatalf("marshalling the request object: %v", err)
	}
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "nutgraf.in", Version: "v1alpha1", Kind: "SpokePool"},
		Object:    runtime.RawExtension{Raw: raw},
	}}

	// An empty scheme, deliberately: the manager's scheme does not know this type
	// either, and a test that registered it would be testing a cluster we do not
	// ship.
	mutate := admission.WithDefaulter(runtime.NewScheme(), &SpokePoolDefaulter{})
	resp := mutate.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("the mutating handler refused the request: %v", resp.Result)
	}
	patch, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatalf("marshalling the patch: %v", err)
	}
	if !strings.Contains(string(patch), hetznerComposition) {
		t.Errorf("the mutating handler returned no composition reference; patch was %s", patch)
	}

	validate := admission.WithValidator(runtime.NewScheme(),
		NewSpokePoolValidator(validatorWith(composition(hetznerComposition, spokePoolAPIVersion, "SpokePool")).reader))
	// The request as the API server would send it after mutation: the platform's
	// reference is in place.
	admitted := spokePool("hetzner")
	if err := (&SpokePoolDefaulter{}).Default(context.Background(), admitted); err != nil {
		t.Fatalf("defaulting: %v", err)
	}
	if raw, err = json.Marshal(admitted.Object); err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	req.Object = runtime.RawExtension{Raw: raw}
	if resp := validate.Handle(context.Background(), req); !resp.Allowed {
		t.Fatalf("the validating handler refused a canonical SpokePool: %v", resp.Result)
	}
}
