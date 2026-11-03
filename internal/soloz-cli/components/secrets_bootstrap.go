package components

import (
	"context"
	"fmt"

	"github.com/soloz-io/zero-ops/internal/soloz-cli/infisical"
)

// GenerateLocalSecrets generates all static Kubernetes Secrets that must exist
// before the data-plane workloads (CNPG, Redis) and Infisical pods can boot.
// This runs between B01 and B02 so that every Secret referenced by Helm chart
// manifests is present in etcd when the kubelet pulls the container images.
//
// Creates:
//   - infisical-secrets (ENCRYPTION_KEY, AUTH_SECRET, REDIS_URL — no DB_ROOT_CERT yet)
//   - infisical-redis-credentials
//   - infisical-db-credentials (CNPG Secret Zero — initdb reads this immediately)
//   - infisical-postgres-connection (Infisical DB credentials — prevents
//     CreateContainerConfigError deadlock in B03)
//
// Called by: orchestrator PhaseGenerateLocalSecrets (between B01 and B02)
func (i *Installer) GenerateLocalSecrets(ctx context.Context) error {
	fmt.Println("Generating local bootstrap secrets (Phase B01→B02)...")

	if err := i.GenerateInfisicalCryptoSecrets(ctx); err != nil {
		return fmt.Errorf("failed to generate infisical crypto secrets: %w", err)
	}

	if _, err := i.InstallPostgresConnectionSecret(ctx); err != nil {
		return fmt.Errorf("failed to install postgres connection secret: %w", err)
	}

	fmt.Println("✓ Local bootstrap secrets ready")
	return nil
}

// BootstrapInfisicalAPI bootstraps the Infisical REST API layer after the
// Infisical workload is deployed and healthy. It creates the Org, Project,
// Machine Identity, stores the infisical-auth Secret, uploads Layer 1+2
// credentials to the Infisical vault, and restarts workloads if needed.
//
// This MUST run after B03 — by then Infisical pods are Running, and the
// CNPG CA certificate was already injected (PhaseInjectCACert).
//
//	Step 1: WaitForInfisicalHealth (data layer + Infisical pods)
//	Step 2: InstallInfisicalAuthFromInfisical (Org, Project, Machine Identity)
//	Step 3: InstallPlatformDatabaseCredentials (layer 2 creds via Infisical API)
//
// Port-forward lifecycle is managed internally for local clusters.
//
// Called by: orchestrator PhaseBootstrapInfisicalAPI (after B03)
func (i *Installer) BootstrapInfisicalAPI(ctx context.Context) error {
	fmt.Println("Bootstrapping Infisical API (Phase post-B03)...")

	// Step 1: Wait for Infisical to become healthy
	if err := i.WaitForInfisicalHealth(ctx); err != nil {
		return fmt.Errorf("Infisical health check failed: %w", err)
	}

	// Step 2: Bootstrap Infisical (Org, Project, Machine Identity)
	if _, err := i.InstallInfisicalAuthFromInfisical(ctx); err != nil {
		return fmt.Errorf("failed to bootstrap Infisical: %w", err)
	}

	// Establish Infisical API access via port-forward
	pf := infisical.NewPortForwardManager(
		"platform-security", "infisical-standalone-infisical",
		"8081", "8080", i.Kubeconfig,
	)
	var pfStarted bool
	if err := pf.EnsureAPIAccess(ctx); err != nil {
		return fmt.Errorf("failed to establish Infisical API access: %w", err)
	}
	pfStarted = true

	// Step 3: Store platform database credentials in Infisical
	changed3, err := i.InstallPlatformDatabaseCredentials(ctx)
	if err != nil {
		if pfStarted {
			pf.Stop()
		}
		return fmt.Errorf("failed to install platform database credentials: %w", err)
	}

	if pfStarted {
		pf.Stop()
	}

	// Restart workloads if secrets changed
	if changed3 {
		if err := i.RestartPlatformWorkloads(ctx); err != nil {
			return fmt.Errorf("failed to restart workloads: %w", err)
		}
		fmt.Println("✅ Bootstrap complete! New secrets injected and pods rolling out.")
	} else {
		fmt.Println("✅ Bootstrap complete! All dependencies already exist. No pods were restarted.")
	}

	return nil
}

// RunInitSecrets executes the full init-secrets pipeline in one call for the
// standalone `hub init-secrets` CLI command. Unlike the phased orchestrator
// flow (which runs GenerateLocalSecrets / inject-ca-cert / BootstrapInfisicalAPI
// across three separate boundary phases), this runs everything sequentially.
//
// The pipeline order guarantees that every K8s Secret exists before the workload
// that consumes it is deployed, preventing CreateContainerConfigError deadlocks.
//
//	Step 1: GenerateInfisicalCryptoSecrets (crypto keys + infisical-db-credentials)
//	Step 2: InstallPostgresConnectionSecret (infisical-db-credentials + connection)
//	Step 3: UpdateInfisicalSecretsWithCNPGCert (wait for CNPG → inject DB_ROOT_CERT)
//	Step 4: WaitForInfisicalHealth (data layer + Infisical pods)
//	Step 5: InstallInfisicalAuthFromInfisical (Org, Project, Machine Identity)
//	Step 6: InstallPlatformDatabaseCredentials
func (i *Installer) RunInitSecrets(ctx context.Context) error {
	fmt.Println("Bootstrapping Layer 1 Infrastructure Secrets...")
	fmt.Println("  (Note: Existing secrets are treated as immutable and will not be overwritten)")

	// Step 1: Crypto keys + static secrets
	if err := i.GenerateLocalSecrets(ctx); err != nil {
		return fmt.Errorf("failed to generate local secrets: %w", err)
	}

	// Step 2: Wait for CNPG and inject DB_ROOT_CERT
	if err := i.UpdateInfisicalSecretsWithCNPGCert(ctx); err != nil {
		return fmt.Errorf("failed to inject CNPG CA certificate: %w", err)
	}

	// Steps 3-6: Wait for Infisical health, bootstrap API, store credentials
	return i.BootstrapInfisicalAPI(ctx)
}
