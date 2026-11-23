// +kubebuilder:object:generate=true
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=smi;smids
// +kubebuilder:printcolumn:name="Spoke",type=string,JSONPath=".spec.spokeRef.name"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SpokeMachineIdentity manages a Machine Identity in Infisical for a Spoke cluster.
// It is the declarative representation of a Spoke's infrastructure identity.
// The Spoke Identity Operator reconciles this CR against the Infisical API.
//
// NOTE: This is the initial implementation of the Platform Identity Domain.
// It may evolve to support full identity lifecycle (rotation, revocation, attestation, audit)
// as defined in a future ADR.
type SpokeMachineIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpokeMachineIdentitySpec   `json:"spec,omitempty"`
	Status SpokeMachineIdentityStatus `json:"status,omitempty"`
}

type SpokeMachineIdentitySpec struct {
	// SpokeRef identifies the SpokePool this identity belongs to
	SpokeRef SpokeRef `json:"spokeRef"`

	// Infisical configures the Infisical provider
	Infisical InfisicalConfig `json:"infisical"`

	// SecretName overrides the default CRS wrapper Secret name.
	// Default: "<spokeRef.name>-machine-identity"
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// RotationPolicy defines credential rotation settings
	// +optional
	RotationPolicy *RotationPolicy `json:"rotationPolicy,omitempty"`

	// RevocationPolicy defines credential revocation settings
	// +optional
	RevocationPolicy *RevocationPolicy `json:"revocationPolicy,omitempty"`
}

type SpokeRef struct {
	Name string `json:"name"`

	// Environment is the environment this workload cluster serves -- the slug its
	// ClusterSecretStore selects and every ExternalSecret on it resolves through.
	//
	// Carried on the reference rather than read from the SpokePool, so the identity
	// operator needs no access to another operator's custom resource to know what
	// it is scoping. hub-operator copies it from the SpokePool's `environment`
	// label, which the composition writes from the claim the tenant authored.
	//
	// Required for the cluster's secrets project: Infisical creates dev/staging/
	// prod by default and a cluster serving `stg` would select an environment one
	// word away and find it empty, so the declared slug is created explicitly.
	// +optional
	Environment string `json:"environment,omitempty"`
}

type InfisicalConfig struct {
	// +optional
	OrganizationID string `json:"organizationID,omitempty"`
	// +optional
	ProjectID string `json:"projectID,omitempty"`
	// SecretsProjectID is the Infisical project ID for secret-manager access
	// (e.g. hub-secrets) that the spoke machine identity must be granted
	// access to so the spoke ESO ClusterSecretStore can read tenant secrets.
	// Defaults to the operator's --infisical-secrets-project-id flag when unset.
	// +optional
	SecretsProjectID string `json:"secretsProjectID,omitempty"`

	// SecretsProjectSlug is this cluster's own Infisical secrets project, and it is
	// also the signal that the project is READY to be the cluster's only source of
	// secrets.
	//
	// hub-operator sets it only after it has materialised this cluster's
	// credentials into that project. Until then it is empty and the identity
	// operator leaves the identity's existing memberships alone -- narrowing an
	// identity to a project that does not yet hold its secrets is how a working
	// spoke is broken by a security fix.
	//
	// One field, one writer, and it carries the readiness and the destination
	// together so the two cannot disagree. It is also what removes the slug
	// formula from the identity operator: the component that populated the project
	// names it.
	// +optional
	SecretsProjectSlug string `json:"secretsProjectSlug,omitempty"`
	// +kubebuilder:default="universal-auth"
	// +optional
	AuthMethod string `json:"authMethod,omitempty"`
	// +kubebuilder:default="90d"
	// +optional
	ClientSecretTTL string `json:"clientSecretTTL,omitempty"`
}

type RotationPolicy struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// +kubebuilder:default="60d"
	// +optional
	Interval string `json:"interval,omitempty"`
	// +kubebuilder:default="24h"
	// +optional
	OverlapPeriod string `json:"overlapPeriod,omitempty"`
}

type RevocationPolicy struct {
	// +kubebuilder:default=true
	RevokeOnDelete bool `json:"revokeOnDelete"`
	// +kubebuilder:default="72h"
	// +optional
	GracePeriod string `json:"gracePeriod,omitempty"`
}

type SpokeMachineIdentityStatus struct {
	// IdentityId is the Infisical Machine Identity ID
	// +optional
	IdentityID string `json:"identityID,omitempty"`
	// ClientId is the Infisical Universal Auth client ID
	// +optional
	ClientID string `json:"clientID,omitempty"`
	// ClientSecretIds tracks active client secrets for lifecycle management
	// +optional
	ClientSecretIDs []string `json:"clientSecretIDs,omitempty"`
	// Conditions represent the latest state observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastRotated is when the most recent rotation occurred
	// +optional
	LastRotated *metav1.Time `json:"lastRotated,omitempty"`
	// NextRotation is when the next rotation is due
	// +optional
	NextRotation *metav1.Time `json:"nextRotation,omitempty"`
	// LastAuthorizationInventory is when this identity's Infisical memberships were
	// last READ -- and it is deliberately not the Conditions' observedGeneration.
	//
	// That generation says which version of THIS resource the controller has seen;
	// this says how fresh the state retrieved from INFISICAL is. They move
	// independently: nothing about a spec change refreshes an external read, and a
	// successful read does not mean the spec was reconciled. Reporting one as
	// evidence of the other is how a stale authorization observation gets read as
	// current.
	//
	// What the inventory found is NOT published here. A role list in a cluster
	// object is authorization metadata on the wrong side of the boundary, and
	// nothing in Kubernetes needs it: the condition says whether the read
	// succeeded, the operator's log carries the detail, and the tests carry the
	// assertions.
	// +optional
	LastAuthorizationInventory *metav1.Time `json:"lastAuthorizationInventory,omitempty"`
}

// +kubebuilder:object:root=true

type SpokeMachineIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpokeMachineIdentity `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SpokeMachineIdentity{}, &SpokeMachineIdentityList{})
}
