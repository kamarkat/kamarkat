/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// NOTE: the emptiness check below is size(x) == 0 on purpose, not a comparison
// against an empty string literal. gofmt normalises doc comments and rewrites a
// pair of ASCII apostrophes into a Unicode right double quote (a legacy godoc
// quoting convention). CEL cannot compile that, and the CRD then fails to install
// with a token recognition error. Because `make build` runs `go fmt`, the
// empty-string-literal spelling is silently reverted on every single build, so it
// can never hold. Keep this expression free of apostrophe pairs.
//
// (This comment is deliberately worded to avoid such a pair itself.)
// HubEnvironmentSpec defines the desired state of HubEnvironment
//
// spec.domain is the sole authoritative base-domain value for hub public
// endpoints (ADR-051, 2026-08-24 addendum). It is validated on its own shape and
// nothing else: the hub has no environment for a domain rule to key off.
//
// spec.environment IS GONE (ADR-111 Revision 7). It declared "the platform
// environment identity for this Hub", which is a property only a hub belonging to
// one environment can have. One shared management hub manages workload clusters in
// dev, stg and prod, so there is no value it could hold, and three validation
// rules keyed off it:
//
//   - two tied spec.domain to it, enforcing env-as-zone (a non-production
//     environment owns a labelled zone, production uses the apex). ADR-051's own
//     Amendment 2026-09-18 had already replaced that scheme with a DECLARED
//     subdomain -- every public host is <service>.<subdomain>.<domain>, and a box
//     that declared environment dev and now declares subdomain dev publishes
//     exactly what it published before. The rules outlived the scheme they
//     enforced.
//   - one pinned secrets.infisical.environmentSlug equal to it, which made the
//     hub's own secret scope a workload environment by construction. That is the
//     coupling Revision 7 removes; the slug is now the management scope, declared
//     rather than derived.
//
// The field had no Go consumer -- those three rules were its only effect -- so
// removing it changes no behaviour beyond lifting the coupling.
type HubEnvironmentSpec struct {
	// BoxID is the logical hub: this box, for as long as it exists (ADR-111 §1).
	//
	// Separate from ClusterName because one field was carrying two lifetimes. The
	// box's identity keys what must survive a cluster being replaced -- the escrow
	// path holding the secret store's master keys and the identity provider's
	// encryption key, and the external-dns owner id that decides which records are
	// this box's to publish. The CLUSTER's identity keys what is replaced with it:
	// the WAL archive prefix, the CAPI object names, the admin kubeconfig and the
	// at-rest encryption key.
	//
	// While the two were one value, a hub could not be replaced without re-keying
	// everything the box owns, which is why ClusterName's comment below had to say
	// it is pinned forever. Declared rather than defaulted from ClusterName: a
	// default would make the first successor inherit its predecessor's cluster name
	// as the box id on one box and not another, and the value is unchangeable once
	// anything has been written under it.
	//
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	BoxID string `json:"boxId,omitempty"`

	// ClusterName is this box's cluster, as CAPI knows it and as the escrow keys
	// cluster-scoped artefacts by (e.g. nutgraf-hub).
	//
	// This is the CLUSTER, not the box: a successor hub carries a new one, while
	// BoxID above stays. Artefacts that must outlive the cluster are keyed by the
	// box; see escrow.Identity.
	//
	// Declared rather than derived from metadata.name. HubEnvironment is a
	// cluster-scoped singleton always named "hub-environment", and that name was
	// being used as the cluster's identity: the admin kubeconfig was looked for at
	// platform-capi/hub-environment-kubeconfig, which CAPI never creates, so it
	// was never escrowed on any box; and the master keys went to the escrow path
	// /hub-operator/hub-environment, a constant, so every box in a shared escrow
	// project overwrote the same entry while `kakt kubeconfig` read
	// /hub-operator/<cluster> and found nothing.
	//
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ClusterName string `json:"clusterName,omitempty"`

	// Domain is the base domain for the Hub cluster (e.g., nutgraf.in)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Domain string `json:"domain"`

	// TLS configuration for the Hub cluster
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`

	// Observability configuration for metrics and monitoring
	// +optional
	Observability *ObservabilityConfig `json:"observability,omitempty"`

	// Database configuration for CNPG cluster and roles
	// +kubebuilder:validation:Required
	Database DatabaseConfig `json:"database"`

	// OAuth configuration for Hydra clients
	// +optional
	OAuth *OAuthConfig `json:"oauth,omitempty"`

	// Secrets configuration for Infisical backup
	// +optional
	Secrets *SecretsConfig `json:"secrets,omitempty"`
}

// TLSConfig defines TLS certificate configuration
type TLSConfig struct {
	// Issuer is the cert-manager ClusterIssuer name
	// +kubebuilder:validation:Required
	Issuer string `json:"issuer"`

	// Email for Let's Encrypt notifications
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=email
	Email string `json:"email"`
}

// ObservabilityConfig defines observability settings
type ObservabilityConfig struct {
	// VictoriaMetrics configuration
	// +optional
	VictoriaMetrics *VictoriaMetricsConfig `json:"victoriaMetrics,omitempty"`

	// GrafanaAlloy configuration
	// +optional
	GrafanaAlloy *GrafanaAlloyConfig `json:"grafanaAlloy,omitempty"`
}

// VictoriaMetricsConfig defines VictoriaMetrics settings
type VictoriaMetricsConfig struct {
	// RetentionPeriod for metrics storage (e.g., "30d", "90d")
	// +kubebuilder:default="30d"
	// +optional
	RetentionPeriod string `json:"retentionPeriod,omitempty"`
}

// GrafanaAlloyConfig defines Grafana Alloy settings
type GrafanaAlloyConfig struct {
	// ScrapeInterval for metrics collection (e.g., "30s", "15s")
	// +kubebuilder:default="30s"
	// +optional
	ScrapeInterval string `json:"scrapeInterval,omitempty"`
}

// DatabaseConfig defines CNPG cluster and role configuration
type DatabaseConfig struct {
	// ClusterRef is the box's BOOTSTRAP cluster: the one Infisical's own database
	// lives on (e.g., infisical-db).
	//
	// It is not a default for roles. Each application owns its own cluster (ADR-112)
	// and declares it on the role, so the only thing a box-level name can mean is the
	// cluster the operator itself depends on: Day-0 waits for it, the generated
	// bootstrap secrets address it, and its CA is the one Infisical is handed.
	//
	// +kubebuilder:validation:Required
	ClusterRef string `json:"clusterRef"`

	// Namespace of the CNPG Cluster
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Roles to be created in the database
	// +optional
	Roles []DatabaseRole `json:"roles,omitempty"`
}

// DatabaseRole defines a database role specification
type DatabaseRole struct {
	// Name of the database role
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Name string `json:"name"`

	// Database name where the role has permissions
	// +kubebuilder:validation:Required
	Database string `json:"database"`

	// ClusterRef is the CNPG Cluster this role lives on. REQUIRED.
	//
	// Named per role because that is the grain reality has: a box runs one cluster per
	// platform application (ADR-112) and a role exists on exactly one of them.
	// Reconciling or rotating it against another cluster either fails or, worse,
	// succeeds against the wrong application's database.
	//
	// Required rather than defaulted for that second case. A role whose cluster is
	// assumed is provisioned somewhere plausible and silently wrong, and the API is
	// where that is cheapest to refuse.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ClusterRef string `json:"clusterRef"`

	// Permissions granted to the role (e.g., SELECT, INSERT, UPDATE, DELETE)
	// +optional
	Permissions []string `json:"permissions,omitempty"`
}

// OAuthConfig defines OAuth client configuration
type OAuthConfig struct {
	// Clients to be registered with Hydra
	// +optional
	Clients []OAuthClient `json:"clients,omitempty"`
}

// OAuthClient defines an OAuth client specification
type OAuthClient struct {
	// ClientID is the unique identifier for the OAuth client
	// +kubebuilder:validation:Required
	ClientID string `json:"clientId"`

	// ClientName is the human-readable name for the OAuth client
	// +kubebuilder:validation:Required
	ClientName string `json:"clientName"`

	// RedirectURIs are the allowed redirect URIs for the client
	// +optional
	RedirectURIs []string `json:"redirectUris,omitempty"`

	// GrantTypes are the allowed OAuth grant types
	// +optional
	GrantTypes []string `json:"grantTypes,omitempty"`

	// ResponseTypes are the allowed OAuth response types
	// +optional
	ResponseTypes []string `json:"responseTypes,omitempty"`
}

// SecretsConfig defines secret management configuration
type SecretsConfig struct {
	// Infisical configuration for secret backup
	// +optional
	Infisical *InfisicalConfig `json:"infisical,omitempty"`
}

// InfisicalConfig defines Infisical project configuration
type InfisicalConfig struct {
	// ProjectSlug is the Infisical project identifier
	// +kubebuilder:default="hub-platform"
	// +optional
	ProjectSlug string `json:"projectSlug,omitempty"`

	// EnvironmentSlug is the Infisical environment holding THIS HUB's credentials
	// -- the management secret scope (ADR-111 Revision 7).
	//
	// Rendered from the box's declared `management.infisicalEnvironmentSlug`. This
	// field is the operator-facing representation of that one input, not a second
	// source of truth, and the hub-operator reads the same value from
	// hub-bootstrap-config's INFISICAL_ENVIRONMENT_SLUG so reader and writer
	// cannot disagree.
	//
	// Required and non-empty, with NO DEFAULT. It defaulted to "prod" while a
	// cross-field rule simultaneously pinned it equal to spec.environment, so the
	// default was unreachable wherever it mattered and misleading everywhere else.
	// The only possible default is a guess at which environment someone else's
	// secrets are in, and guessing wrong is silent: the secrets resolve, from the
	// wrong scope.
	//
	// Not constrained to an environment set. A management scope is not a workload
	// environment -- nothing is promoted into or out of it, and no managed cluster
	// reads it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EnvironmentSlug string `json:"environmentSlug"`
}

// HubEnvironmentStatus defines the observed state of HubEnvironment.
type HubEnvironmentStatus struct {
	// Phase represents the high-level lifecycle phase of the HubEnvironment
	// Valid phases: Provisioning, Available, Degraded, Failed
	// - Provisioning: Controllers are actively converging, pods may be NotReady
	// - Available: Readiness contract met, system is fully operational
	// - Degraded: System was Available, but a dependency dropped or drifted
	// - Failed: Terminal failure requiring manual intervention
	// +kubebuilder:validation:Enum=Provisioning;Available;Degraded;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the current state of the HubEnvironment resource.
	// Standard condition types:
	// - SecretZeroGenerated: Secret Zero bootstrap secrets have been created
	// - SecretsBackedUp: Secrets have been uploaded to Infisical
	// - OAuthClientsRegistered: OAuth clients have been registered with Hydra
	// - Ready: All phases complete successfully
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation of the most recently observed HubEnvironment
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UploadedSecrets tracks which secrets have been uploaded to Infisical
	// This prevents re-uploading secrets that already exist
	// +optional
	UploadedSecrets []string `json:"uploadedSecrets,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// HubEnvironment is the Schema for the hubenvironments API
type HubEnvironment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of HubEnvironment
	// +required
	Spec HubEnvironmentSpec `json:"spec"`

	// status defines the observed state of HubEnvironment
	// +optional
	Status HubEnvironmentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HubEnvironmentList contains a list of HubEnvironment
type HubEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HubEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HubEnvironment{}, &HubEnvironmentList{})
}

// ClusterFor reports which CNPG cluster a role belongs to.
//
// A role that names none belongs to the box's shared cluster, which is what every role
// did before ADR-112. Resolved here rather than at each call site so the two paths that
// act on roles -- reconciliation and rotation -- cannot disagree about where a role is.
func (s HubEnvironmentSpec) ClusterFor(role DatabaseRole) string {
	return role.ClusterRef
}

// RolesFor returns the roles that live on one cluster.
func (s HubEnvironmentSpec) RolesFor(cluster string) []DatabaseRole {
	var out []DatabaseRole
	for _, r := range s.Database.Roles {
		if s.ClusterFor(r) == cluster {
			out = append(out, r)
		}
	}
	return out
}

// DatabaseClusters lists every cluster the box runs, in declaration order, without
// duplicates: the bootstrap cluster first, then the cluster each declared role lives on.
//
// The bootstrap cluster is included although no declared role names it. The only role on
// it is Infisical's, and CNPG owns that one through spec.managed.roles (ADR-023 addendum),
// so a list built from roles alone would leave out the cluster the box cannot start
// without.
func (s HubEnvironmentSpec) DatabaseClusters() []string {
	seen := map[string]bool{}
	var out []string
	if s.Database.ClusterRef != "" {
		seen[s.Database.ClusterRef] = true
		out = append(out, s.Database.ClusterRef)
	}
	for _, r := range s.Database.Roles {
		c := s.ClusterFor(r)
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
