package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/components"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/infisical"
	"github.com/spf13/cobra"
)

var (
	initSecretsKubeconfig  string
	initSecretsEnvironment string
)

func newInitSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "init-secrets",
		Aliases: []string{"bootstrap-secrets"},
		Short:   "Bootstrap Layer 1 immutable infrastructure secrets (Secret Zero)",
		RunE:    runInitSecrets,
	}

	cmd.Flags().StringVar(&initSecretsKubeconfig, "kubeconfig", "", "Path to kubeconfig (default: ~/.kube/config)")
	// This phase writes the ADR-045 bootstrap config into
	// manifests/environments/<slug>/generated/. Without a slug it has no
	// directory to write to, and the shared one it used to write to was
	// inherited by every overlay -- so whichever cluster bootstrapped last
	// decided which Infisical project every environment rendered.
	cmd.Flags().StringVar(&initSecretsEnvironment, "environment", "", "Environment slug the generated artifacts belong to (required)")

	return cmd
}

func runInitSecrets(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if initSecretsKubeconfig == "" {
		home, _ := os.UserHomeDir()
		initSecretsKubeconfig = filepath.Join(home, ".kube", "config")
	}

	if initSecretsEnvironment == "" {
		return fmt.Errorf("--environment is required: this phase writes generated artifacts into " +
			"manifests/environments/<slug>/generated/ and cannot choose the slug for you")
	}

	installer := &components.Installer{
		Kubeconfig:      initSecretsKubeconfig,
		EnvironmentSlug: initSecretsEnvironment,
	}

	accessInfo := infisical.GetAccessInfo(ctx)
	defer printManualActions(accessInfo)

	return installer.RunInitSecrets(ctx)
}
