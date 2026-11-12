package main

import (
	"fmt"
	"os"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/components"
	"github.com/spf13/cobra"
)

func newConfigureGitHubAccessCmd() *cobra.Command {
	var ghcrPAT string
	var orgURL string
	var kubeconfig string

	cmd := &cobra.Command{
		Use:   "configure-github-access",
		Short: "Configure ArgoCD GitHub repository access",
		Long: `Configure ArgoCD GitHub repository access (Secret Zero).

This command injects GitHub credentials that enable ArgoCD to sync manifests:
1. ArgoCD GitHub repository access (GitHub PAT)
2. GHCR pull secret for container images

This is Step 3 in the bootstrap workflow - it must be run BEFORE init-secrets
to allow ArgoCD to create the platform-data namespace.

Prerequisites:
- Hub cluster must be bootstrapped
- Generate GitHub Personal Access Token with repo scope

After running this command:
- ArgoCD will sync and create platform namespaces
- Platform components will be deployed
- You can then run init-secrets (Step 4)

Security Note:
- This is a bootstrap secret (Secret Zero)
- ESO will replace it with Infisical-backed version after deployment`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if ghcrPAT == "" {
				return fmt.Errorf("--ghcr-pat is required")
			}

			// Set default kubeconfig if not provided
			if kubeconfig == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("failed to get home directory: %w", err)
				}
				kubeconfig = fmt.Sprintf("%s/.kube/config", home)
			}

			// Verify kubeconfig exists
			if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
				return fmt.Errorf("kubeconfig not found at %s", kubeconfig)
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigureGitHubAccess(cmd, ghcrPAT, orgURL, kubeconfig)
		},
	}

	cmd.Flags().StringVar(&ghcrPAT, "ghcr-pat", "", "GitHub Personal Access Token (used for both Git and GHCR access)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (default: ~/.kube/config)")
	// Scopes the credential. Required rather than defaulted: a default would be
	// the platform's own organisation, which is what this used to hard-code and
	// which grants a box access to nothing it owns (ADR-062).
	cmd.Flags().StringVar(&orgURL, "org-url", "", "Git organisation the credential is scoped to, e.g. https://github.com/acme")

	cmd.MarkFlagRequired("ghcr-pat")
	cmd.MarkFlagRequired("org-url")

	return cmd
}

func runConfigureGitHubAccess(cmd *cobra.Command, ghcrPAT, orgURL, kubeconfig string) error {
	ctx := cmd.Context()

	fmt.Println("🔐 Configuring GitHub Access (Secret Zero)...")
	fmt.Println("   This enables ArgoCD to sync manifests and create namespaces")

	installer := &components.Installer{
		Kubeconfig: kubeconfig,
	}

	// Step 1: Create ArgoCD GitHub auth secret
	fmt.Println("\n[1/2] Creating ArgoCD GitHub authentication secret...")
	if err := installer.FixArgoCDGitHubAuth(ctx, ghcrPAT, orgURL); err != nil {
		return fmt.Errorf("failed to create ArgoCD GitHub secret: %w", err)
	}

	// Step 2: Create GHCR pull secret
	fmt.Println("\n[2/2] Creating GHCR pull secret...")
	// Extract username from token (GitHub username is not needed for GHCR, use placeholder)
	ghcrUsername := "zero-ops-bot"
	if err := installer.InstallGHCRPullSecret(ctx, ghcrUsername, ghcrPAT); err != nil {
		return fmt.Errorf("failed to create GHCR pull secret: %w", err)
	}

	fmt.Println("\n✅ GitHub access configured!")
	fmt.Println("\nNext steps:")
	fmt.Println("1. Wait for ArgoCD to sync (creates platform namespaces):")
	fmt.Println("   kubectl get applications -n platform-ops")
	fmt.Println("2. Verify platform-data namespace exists:")
	fmt.Println("   kubectl get namespace platform-data")
	fmt.Println("3. Run init-secrets:")
	fmt.Println("   ./bin/kakt init-secrets --cluster <management-cluster>")

	return nil
}
