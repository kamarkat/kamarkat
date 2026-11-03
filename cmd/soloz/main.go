package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "soloz",
		Short: "Create and maintain a tenant's platform",
		// Named for the platform rather than for one of its clusters. "hub" is a
		// cluster role in this architecture -- a hub has spokes -- so a binary
		// called hub read as though it managed that one cluster, when what it
		// creates is the whole box: the control plane, its identity and trust,
		// and the repository the tenant reconciles from.
		Long: "Create and maintain the platform a tenant runs: scaffold the tenant's\n" +
			"repository, bootstrap the control plane it declares, and manage the\n" +
			"bundle version it is pinned to.",
	}

	rootCmd.AddCommand(newBootstrapCmd())
	rootCmd.AddCommand(newTeardownCmd())
	rootCmd.AddCommand(newSpokeCmd())
	rootCmd.AddCommand(newConfigureGitHubAccessCmd())
	rootCmd.AddCommand(newConfigureTailscaleCmd())
	rootCmd.AddCommand(newInitSecretsCmd())
	rootCmd.AddCommand(newConfigureESOCmd())
	rootCmd.AddCommand(newReseedCmd())
	rootCmd.AddCommand(newVerifyCmd())
	rootCmd.AddCommand(newKubeconfigCmd())
	rootCmd.AddCommand(newTenantCmd())
	rootCmd.AddCommand(newBundleVersionCmd())
	rootCmd.AddCommand(newEscrowCmd())
	rootCmd.AddCommand(newEncryptionCmd())
	rootCmd.AddCommand(newProposalCmd())
	rootCmd.AddCommand(newSupportCmd())
	rootCmd.AddCommand(newFleetCmd())
	rootCmd.AddCommand(newReleaseCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
