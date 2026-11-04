package health

// This file defines the BootstrapReadinessCheck interface (declared in
// waiter.go) implementations for each platform service layer. These
// are the canonical phase-readiness profiles used by bootstrap
// orchestrators and the init-secrets command.
//
// Why pre-defined phases?
//   - A new check (e.g., a new dependency) means adding one struct
//     to the appropriate phase, not editing multiple wait loops.
//   - Phase order encodes inter-service dependencies (CNPG →
//     PgBouncer → Infisical) so callers don't have to know which
//     checks depend on which.

import (
	"context"
	"errors"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
)

// DataLayerReadiness is the readiness profile for the data layer
// (PostgreSQL via CloudNativePG). External services that depend on
// PostgreSQL connectivity should wait for this phase to pass before
// they start (or restart) their own pods.
//
// Order encodes dependencies:
//  1. CNPG cluster: primary instance is ready
//  2. PgBouncer pooler: connection-brokering layer is available
//
// Add new data-layer dependencies here, in the order they must be
// ready before downstream services start.
type DataLayerReadiness struct {
	// CNPGClusterName is the name of the CloudNativePG Cluster CR. REQUIRED.
	//
	// There is no shared cluster to fall back to: ADR-112 gives each application its
	// own. Any default here would name a cluster nothing creates, making the wait
	// indefinite with a message naming a CR nobody declared -- so the caller says
	// which cluster it means.
	CNPGClusterName string
	// PoolerName is the name of the CNPG Pooler CR. Optional.
	//
	// Empty means the cluster has no Pooler and none is waited for. ADR-112 §2
	// provisions one only where an application needs it, and none of the
	// application-owned clusters does.
	PoolerName string
	// OwnerApplication is the ArgoCD Application that creates this cluster, in
	// NamespaceOps. Optional.
	//
	// Naming it lets the check distinguish "not created yet" from "nothing is going to
	// create it": an Application ArgoCD reports Synced and Healthy while managing no
	// resources renders nothing, and waiting out a timeout ends where it began. Left
	// empty the message degrades to "not created yet"; the wait itself is unaffected.
	OwnerApplication string
	// Namespace is the namespace both CRs live in.
	// Default: platform-data.
	Namespace string
}

// PhaseName returns the phase identifier.
func (d *DataLayerReadiness) PhaseName() string { return "Data Layer" }

// Checkers returns the ordered list of HealthChecker instances.
func (d *DataLayerReadiness) Checkers() []HealthChecker {
	// Refused rather than defaulted. Checkers cannot return an error, so the
	// misconfiguration becomes a check that fails with its reason -- which is louder
	// than waiting out a timeout against a cluster the caller never named.
	if d.CNPGClusterName == "" {
		return []HealthChecker{newMisconfigured(
			"DataLayerReadiness.CNPGClusterName is empty. Each application owns its own " +
				"cluster (ADR-112) and there is none to assume; name the one to wait for.")}
	}

	namespace := d.Namespace
	if namespace == "" {
		namespace = constants.NamespaceData
	}

	cnpg := NewCNPGClusterHealth(d.CNPGClusterName, namespace)
	if d.OwnerApplication != "" {
		cnpg.OwnerApplication = d.OwnerApplication
		cnpg.OwnerNamespace = constants.NamespaceOps
	}

	checkers := []HealthChecker{cnpg}
	if d.PoolerName != "" {
		checkers = append(checkers, NewPgBouncerPoolerHealth(d.PoolerName, namespace))
	}
	return checkers
}

// InfisicalReadiness is the readiness profile for the Infisical
// platform service. It depends on the data layer being ready first —
// this is the contract that closes the CNPG boot race observed during
// Day-0 choreography (see RCA: Infisical pods crash-looped on
// ENOTFOUND platform-db-pooler because the Pooler service didn't
// exist when Infisical started).
//
// Callers (Cobra commands, orchestrators) compose phases:
//
//	health.HealthWaiter{}.Wait(ctx, kubeconfig)  // for the data layer
//	health.HealthWaiter{}.Wait(ctx, kubeconfig)  // for Infisical
type InfisicalReadiness struct {
	// Namespace where Infisical is deployed. Default: platform-security.
	Namespace string
	// WorkloadName is the StatefulSet/Deployment name.
	// Default: "infisical-standalone-infisical".
	WorkloadName string
}

// PhaseName returns the phase identifier.
func (i *InfisicalReadiness) PhaseName() string { return "Infisical Platform" }

// Checkers returns the ordered list of HealthChecker instances.
func (i *InfisicalReadiness) Checkers() []HealthChecker {
	ns := i.Namespace
	if ns == "" {
		ns = constants.NamespaceSecurity
	}
	name := i.WorkloadName
	if name == "" {
		name = "infisical-standalone-infisical"
	}
	return []HealthChecker{
		NewWorkloadHealth(name, ns),
	}
}

// CAPIProvidersReadiness is the readiness profile for the CAPI core
// providers on a bootstrap cluster. Used by the pivot orchestrator
// after the move to verify providers came up cleanly on the
// management cluster.
type CAPIProvidersReadiness struct {
	Namespace string
	// OSType is retained for future node-OS support; today ubuntu is the only
	// supported value and therefore always yields the kubeadm providers (ADR-001).
	OSType string
}

// PhaseName returns the phase identifier.
func (c *CAPIProvidersReadiness) PhaseName() string { return "CAPI Providers" }

// Checkers returns the ordered list of HealthChecker instances.
func (c *CAPIProvidersReadiness) Checkers() []HealthChecker {
	ns := c.Namespace
	if ns == "" {
		ns = constants.NamespaceCAPI
	}
	return []HealthChecker{
		NewCAPIResourceReadyHealth("CoreProvider", "cluster-api", ns),
		NewCAPIResourceReadyHealth("BootstrapProvider", "kubeadm", ns),
		NewCAPIResourceReadyHealth("ControlPlaneProvider", "kubeadm", ns),
		NewCAPIResourceReadyHealth("InfrastructureProvider", "hetzner", ns),
	}
}

// OperatorsReadiness is the readiness profile for the platform
// operators on the hub cluster. Used by the bootstrap orchestrator
// to gate the platform-deploy phase.
type OperatorsReadiness struct{}

// PhaseName returns the phase identifier.
func (o *OperatorsReadiness) PhaseName() string { return "Platform Operators" }

// Checkers returns the ordered list of HealthChecker instances.
func (o *OperatorsReadiness) Checkers() []HealthChecker {
	return []HealthChecker{
		// Webhooks and CRDs come up before pods are fully ready.
		NewValidatingWebhookHealth("capi", "cert-manager", "cnpg", "externalsecret"),
		NewCRDRegisteredHealth(
			"externalsecrets.external-secrets.io",
			"clusters.postgresql.cnpg.io",
		),
		// Pods being Running is the final pre-condition.
		NewOperatorPodsHealth("cnpg-system", "app.kubernetes.io/name=cloudnative-pg"),
		NewOperatorPodsHealth("platform-ops", "app.kubernetes.io/name=external-secrets"),
	}
}

// misconfigured is a HealthChecker that always fails with the reason it was built.
//
// Checkers() cannot return an error, and the alternatives were both worse: defaulting a
// required value puts the wait on a cluster the caller never named, and returning no
// checkers at all makes an unconfigured phase report ready. This fails on the first
// evaluation and says what to fix.
type misconfigured struct{ reason string }

func newMisconfigured(reason string) HealthChecker { return &misconfigured{reason: reason} }

func (m *misconfigured) Name() string { return "configuration" }

func (m *misconfigured) Check(context.Context, string) error { return errors.New(m.reason) }
