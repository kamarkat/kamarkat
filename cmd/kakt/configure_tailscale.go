package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/components"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"github.com/spf13/cobra"
)

func newConfigureTailscaleCmd() *cobra.Command {
	var authkey string
	var hostname string
	var kubeconfig string

	cmd := &cobra.Command{
		Use:   "configure-tailscale",
		Short: "Configure Tailscale credentials for hybrid workload cluster control-plane nodes",
		Long: `Configure Tailscale credentials (Secret Zero) for the hybrid provider cell.

This command injects the Tailscale auth key + CP node hostname used by the
shared ClusterClass Tailscale pre-kubeadm hook (contentFrom.secret → the
tailscale-hybrid-psk Secret in platform-capi). It is part of the hybrid
bootstrap flow and must run before a hybrid workload cluster is provisioned.

Values are read from this operator's own store -- ~/.kamarkat/tailscale/{authkey,hostname},
or $KAMARKAT_HOME -- so they are found from any directory and survive a checkout
being deleted.

After running this command:
- hub-operator uploads tailscale-hybrid-psk to Infisical (CLISecretMappings)
- ESO syncs it back, so ArgoCD cannot reset it to empty
- Hybrid workload cluster CP nodes join the tailnet before kubeadm init`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Resolve defaults from k8-secrets (gitignored) before validation.
			if authkey == "" {
				authkey = opstore.ValueOrEnv("TS_AUTHKEY", "tailscale/authkey")
			}
			if hostname == "" {
				hostname = opstore.Value("tailscale/hostname")
			}

			if authkey == "" {
				return opstore.Missing("Tailscale auth key", "tailscale/authkey")
			}
			if hostname == "" {
				return opstore.Missing("tailnet hostname for the control-plane node", "tailscale/hostname")
			}

			// Set default kubeconfig if not provided
			if kubeconfig == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("failed to get home directory: %w", err)
				}
				kubeconfig = filepath.Join(home, ".kube", "config")
			}

			// Verify kubeconfig exists
			if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
				return fmt.Errorf("kubeconfig not found at %s", kubeconfig)
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigureTailscale(cmd, authkey, hostname, kubeconfig)
		},
	}

	cmd.Flags().StringVar(&authkey, "authkey", "", "Tailscale auth key (default: ~/.kamarkat/tailscale/authkey)")
	cmd.Flags().StringVar(&hostname, "hostname", "", "workload cluster CP tailnet hostname (default: ~/.kamarkat/tailscale/hostname)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (default: ~/.kube/config)")

	return cmd
}

func runConfigureTailscale(cmd *cobra.Command, authkey, hostname, kubeconfig string) error {
	ctx := cmd.Context()

	fmt.Println("🔐 Configuring Tailscale credentials (Secret Zero)...")
	fmt.Printf("   Hostname: %s\n", hostname)

	installer := &components.Installer{
		Kubeconfig: kubeconfig,
	}

	if err := installer.InstallTailscalePSK(ctx, authkey, hostname); err != nil {
		return fmt.Errorf("failed to create tailscale-hybrid-psk secret: %w", err)
	}

	fmt.Println("\n✅ Tailscale credentials configured!")
	fmt.Println("\nNext steps:")
	fmt.Println("1. hub-operator uploads tailscale-hybrid-psk to Infisical on next reconcile")
	fmt.Println("2. ESO syncs it back (tailscale-authkey / tailscale-hostname)")
	fmt.Println("3. Delete any stuck hybrid cluster so CAPI recreates the CP with the pre-kubeadm join")
	fmt.Println("4. Run on-prem-join.sh on home nodes once that cluster's CP is Ready")

	return nil
}
