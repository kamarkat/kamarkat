// Package v1alpha1 contains the SpokeMachineIdentity API.
//
// THE GROUP HAS TO BE DECLARED AS A MARKER, not only in Go. controller-gen reads
// +groupName to resolve a CRD's group, and without it the group came out empty:
// `make manifests` wrote config/crd/bases/_.yaml -- an empty group and an empty
// plural -- instead of updating the real CRD, so every regeneration since has
// silently produced junk beside a file it never touched. The tracked CRD carries
// the correct group because it was corrected by hand afterwards, which is why
// nothing broke at runtime and nobody noticed the generator had stopped working.
//
// +groupName=identity.kamarkat.io
// +kubebuilder:object:generate=true
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "identity.kamarkat.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
