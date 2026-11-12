package main

import (
	"fmt"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/bootstrap"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/components"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/infisical"
	"github.com/spf13/cobra"
)

var (
	initSecretsKubeconfig string
	initSecretsCluster    string
	initSecretsGitops     string
)

func newInitSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "init-secrets",
		Aliases: []string{"bootstrap-secrets"},
		Short:   "Bootstrap Layer 1 immutable infrastructure secrets (Secret Zero)",
		RunE:    runInitSecrets,
	}

	cmd.Flags().StringVar(&initSecretsKubeconfig, "kubeconfig", "",
		"kubeconfig for the cluster (default: read from the box's escrow with your own identity)")
	cmd.Flags().StringVar(&initSecretsCluster, "cluster", "", "the management cluster to write into")
	cmd.Flags().StringVar(&initSecretsGitops, "gitops-dir", ".", "the box's repository, which declares its escrow")
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

	// The box's escrow, with the operator's own identity -- or Day-0's working
	// copy while a bootstrap is still running.
	//
	// It used to default to ~/.kube/config, which is whatever cluster the
	// workstation last pointed at: Secret Zero written into the wrong cluster is
	// a failure nothing reports, because every write succeeds.
	if initSecretsKubeconfig == "" {
		if strings.TrimSpace(initSecretsCluster) == "" {
			return fmt.Errorf("--cluster or --kubeconfig is required: there is no default " +
				"cluster to write Secret Zero into, and the one this workstation last used " +
				"is not an answer")
		}
		resolved, cleanup, err := escrowKubeconfig(ctx, initSecretsGitops, initSecretsCluster)
		if err != nil {
			return err
		}
		defer cleanup()
		initSecretsKubeconfig = resolved
	}

	installer := &components.Installer{
		Kubeconfig: initSecretsKubeconfig,
		OverlayDir: bootstrap.HubOverlayDir,
	}

	accessInfo := infisical.GetAccessInfo(ctx)
	defer printManualActions(accessInfo)

	return installer.RunInitSecrets(ctx)
}
