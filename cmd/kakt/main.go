package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the whole command tree.
//
// Separate from main so a test can construct it. Cobra registers flags when a
// command is built, and a duplicate flag name PANICS at that moment -- which is
// every invocation of this binary, not just the command that owns the flag. A
// compile passes, the tests pass, and `kakt anything` dies on startup.
func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "kakt",
		Short: "Create and maintain a tenant's platform",
		// Named for the platform rather than for one of its clusters. "hub" is a
		// cluster role in this architecture -- a hub holds workload clusters -- so a binary
		// called hub read as though it managed that one cluster, when what it
		// creates is the whole box: the control plane, its identity and trust,
		// and the repository the tenant reconciles from.
		Long: "Create and maintain the platform a tenant runs: scaffold the tenant's\n" +
			"repository, bootstrap the control plane it declares, and manage the\n" +
			"bundle version it is pinned to.",
	}

	// Authentication first, because every command below that reaches a hub now
	// depends on it rather than on files this machine happens to have.
	rootCmd.AddCommand(newLoginCmd())
	rootCmd.AddCommand(newWhoamiCmd())
	rootCmd.AddCommand(newLogoutCmd())
	rootCmd.AddCommand(newBootstrapCmd())
	rootCmd.AddCommand(newTeardownCmd())
	rootCmd.AddCommand(newClusterCmd())
	rootCmd.AddCommand(newConfigureGitHubAccessCmd())
	rootCmd.AddCommand(newConfigureTailscaleCmd())
	rootCmd.AddCommand(newInitSecretsCmd())
	rootCmd.AddCommand(newConfigureESOCmd())
	rootCmd.AddCommand(newReseedCmd())
	rootCmd.AddCommand(newVerifyCmd())
	rootCmd.AddCommand(newKubeconfigCmd())
	rootCmd.AddCommand(newBoxCmd())
	rootCmd.AddCommand(newTenantCmd())
	rootCmd.AddCommand(newBundleVersionCmd())
	rootCmd.AddCommand(newEscrowCmd())
	rootCmd.AddCommand(newEncryptionCmd())
	rootCmd.AddCommand(newProposalCmd())
	rootCmd.AddCommand(newSupportCmd())
	rootCmd.AddCommand(newAppCmd())
	rootCmd.AddCommand(newReleaseCmd())

	return rootCmd
}
