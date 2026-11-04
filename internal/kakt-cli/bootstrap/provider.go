package bootstrap

import (
	"context"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/capi"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/preflight"
)

// ──────────────────────────────────────────────────────────────────────────────
// Provider — uniform 12-phase bootstrap pipeline (ADR-036 §1)
// ──────────────────────────────────────────────────────────────────────────────
//
// Every provider implements the same interface. The orchestrator calls methods
// in a fixed linear sequence with zero branching on provider type.
//
//	Phase   Method                        Cloud / Hybrid
//	─────   ──────                        ─────────────
//	1       PreflightValidators()         cloud credentials (+kind for bootstrap)
//	2       KindConfigPath()              "" (default)
//	3       ProvisionDayZero()            secret+CSI+wait
//	4       CAPIProviders()+OnCAPIInit()  cloud providers
//	5       ProvisionManagementCluster()  provision VMs
//	6       PivotMove()                   clusterctl pivot
//	7       PivotReady()                  wait reconcile
//	8       (orchestrator cleanup)        delete kind
//	9       ClusterClassPaths()           cloud / hybrid clusterclass
//	10      OnPlatformPreReqs()           no-op (reserved)
//	11      (orchestrator ArgoCD+apps)    identical
//	12      Finalize()                    extract CAPI secret
//
// The orchestrator is the single source of truth for phases 8 and 11 — these
// are not provider methods because they are identical across all providers.
type Provider interface {
	// ── Identity ──────────────────────────────────────────────────────────
	Name() string
	Capabilities() CapabilityContract

	// ── Phase 1: Preflight ────────────────────────────────────────────────
	PreflightValidators() []preflight.Validator

	// ── Phase 2: Bootstrap Cluster (kind) ─────────────────────────────────
	// KindConfigPath returns a kind config YAML path, or "" for defaults.
	KindConfigPath() string

	// ── Phase 3: Day-0 Infrastructure ─────────────────────────────────────
	// Applied BEFORE CAPI and any workload scheduling. This is the first
	// opportunity to configure the cluster for provider-specific primitives.
	//
	// Cloud:   creates cloud credentials secret, installs CSI driver, waits
	//          for StorageClass to be dynamically provisioned.
	ProvisionDayZero(ctx context.Context, kubeconfig string) error

	// ── Phase 4: CAPI Initialization ──────────────────────────────────────
	CAPIProviders() []capi.CAPIProvider
	OnCAPIInit(ctx context.Context, kubeconfig, kubeContext, namespace string) error

	// ── Phase 5: Management Cluster Provisioning ──────────────────────────
	// Cloud provisions VMs via CAPI and waits for ready.
	ProvisionManagementCluster(ctx context.Context, cfg *ProvisionConfig) error

	// ── Phase 6: Pivot Move ───────────────────────────────────────────────
	// Returns the management cluster kubeconfig path.
	// Cloud retrieves the CAPI kubeconfig, installs the operator on the
	// management cluster, and executes clusterctl move.
	PivotMove(ctx context.Context, cfg *PivotConfig) (string, error)

	// ── Phase 7: Pivot Ready ──────────────────────────────────────────────
	// Cloud waits for CAPI reconciliation after pivot.
	PivotReady(ctx context.Context, mgmtKubeconfig string) error

	// ── Phase 9: ClusterClass ─────────────────────────────────────────────
	ClusterClassPaths() []string

	// ── Phase 10: Platform Pre-Requisites ─────────────────────────────────
	// Final provider-specific setup before ArgoCD bootstrap apps are applied.
	// Reserved for future use (currently no-op for all providers).
	OnPlatformPreReqs(ctx context.Context, kubeconfig string) error

	// ── Phase 12: Finalize ────────────────────────────────────────────────
	// Persists the management kubeconfig and returns its path.
	// Cloud extracts from CAPI secret.
	Finalize(ctx context.Context, cfg *FinalizeConfig) (string, error)

	// ── Webhook validation ────────────────────────────────────────────────
	// OperatorWebhookPatterns returns additional webhook name substrings that
	// must be present before data-plane workloads (Phase 11 gating).
	// Returns ["caph"] for Hetzner provider.
	OperatorWebhookPatterns() []string
}

// ──────────────────────────────────────────────────────────────────────────────
// Config types passed between orchestrator and provider phases
// ──────────────────────────────────────────────────────────────────────────────

// ProvisionConfig carries state from the orchestrator into Phase 5.
type ProvisionConfig struct {
	ClusterName         string
	BootstrapKubeconfig string
	BootstrapContext    string
	Debug               bool

	// GitopsDir is a checkout of the tenant's own repository when Day-0 runs from
	// one (ADR-072), so the topology it provisions can be recorded there and
	// changed afterwards. Empty for the platform's own box.
	GitopsDir string

	// OIDCIssuerURL and OIDCClientID name the identity provider the API server
	// accepts tokens from (ADR-076). Empty on a first build, because the issuer
	// runs on the cluster being created and does not exist yet.
	//
	// Carried so a REBUILD of an environment whose issuer is already running can
	// declare them at creation, instead of provisioning without them and then
	// replacing the control plane to add them. The ClusterClass patch is
	// conditioned on both, so supplying one is refused rather than half-applied.
	OIDCIssuerURL string
	OIDCClientID  string

	// Encryption selects how this box encrypts Secrets at rest.
	//
	// Carried through Day-0 rather than applied afterwards because the provider
	// configuration is control-plane FILE content: CAPI reads it while the management
	// cluster is being created, and changing it later replaces the control plane.
	// Empty Encryption means secretbox, which is what every box has had.
	Encryption cluster.EncryptionMode
}

// PivotConfig carries state from the orchestrator into Phase 6.
type PivotConfig struct {
	ClusterName         string
	BootstrapKubeconfig string
	BootstrapContext    string
	Debug               bool
	Encryption          cluster.EncryptionMode
}

// FinalizeConfig carries state from the orchestrator into Phase 12.
type FinalizeConfig struct {
	MgmtKubeconfig  string
	ClusterName     string
	Namespace       string
	MergeKubeconfig bool
	Debug           bool
}

// ──────────────────────────────────────────────────────────────────────────────
// Capability Contract (ADR-036 §5)
// ──────────────────────────────────────────────────────────────────────────────

type CapabilityContract struct {
	Version      string
	StorageClass string
	BlockStorage bool
	LoadBalancer bool
	GPU          bool
	MaxNodes     int
}
