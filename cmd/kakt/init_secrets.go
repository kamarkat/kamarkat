package main

import (
	"os"
	"path/filepath"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/bootstrap"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/components"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/infisical"
	"github.com/spf13/cobra"
)

var (
	initSecretsKubeconfig string
)

func newInitSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "init-secrets",
		Aliases: []string{"bootstrap-secrets"},
		Short:   "Bootstrap Layer 1 immutable infrastructure secrets (Secret Zero)",
		RunE:    runInitSecrets,
	}

	cmd.Flags().StringVar(&initSecretsKubeconfig, "kubeconfig", "", "Path to kubeconfig (default: ~/.kube/config)")
	// No --environment. This phase writes the ADR-045 bootstrap config into the
	// hub overlay's generated/ directory, and since ADR-111 Revision 7 there is
	// exactly one hub overlay -- so there is nothing for a caller to choose and
	// nothing to get wrong. The shared directory it used to write to was
	// inherited by every overlay, so whichever cluster bootstrapped last decided
	// which Infisical project every environment rendered; one overlay removes
	// that by removing the second overlay, not by naming the right one.

	return cmd
}

func runInitSecrets(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if initSecretsKubeconfig == "" {
		home, _ := os.UserHomeDir()
		initSecretsKubeconfig = filepath.Join(home, ".kube", "config")
	}

	installer := &components.Installer{
		Kubeconfig: initSecretsKubeconfig,
		OverlayDir: bootstrap.HubOverlayDir,
	}

	accessInfo := infisical.GetAccessInfo(ctx)
	defer printManualActions(accessInfo)

	return installer.RunInitSecrets(ctx)
}
