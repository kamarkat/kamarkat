package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// newKubeconfigCmd retrieves a cluster's admin kubeconfig from its escrow.
//
// This is break-glass, not the way in. Day-to-day access is OIDC through the box's
// own identity provider, where a person authenticates as themselves and can be
// revoked as themselves (ADR-076). This exists for when that cannot be used: the
// identity provider is down, the cluster is broken, or nobody is left who can
// log in.
//
// It reads from the escrow rather than from the cluster deliberately. A command
// that fetched the kubeconfig from the cluster would need cluster access to
// produce cluster access, which is no use in every case this is for.
func newKubeconfigCmd() *cobra.Command {
	var clusterName string
	var boxID string
	var out string

	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Retrieve a cluster's admin kubeconfig from its escrow (break-glass)",
		Long: `Retrieve a cluster's admin kubeconfig from the escrow.

This is break-glass access. Normal access is through your own identity provider,
where each person authenticates as themselves. Use this when that is not available:
the identity provider is down, the cluster is broken, or nobody can log in.

The escrow is the Infisical you supplied at scaffold time, reached with the same
four values the cluster uses:

  INFISICAL_ESCROW_URL         usually https://app.infisical.com
  INFISICAL_ESCROW_PROJECT_ID
  INFISICAL_ESCROW_CLIENT_ID
  INFISICAL_ESCROW_CLIENT_SECRET

The credential it returns is cluster-admin and is not scoped to a person. Prefer
the identity provider wherever it is working, and treat what this prints as
something to use and discard.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(clusterName) == "" {
				return fmt.Errorf("--cluster is required: the escrow holds one kubeconfig per cluster")
			}
			if strings.TrimSpace(boxID) == "" {
				return fmt.Errorf("--box is required: a cluster's artefacts live under the box " +
					"that owns it, so the box cannot be inferred from the cluster name (ADR-111)")
			}

			// An in-cluster URL is not passed: there is no cluster to compare
			// against here, and the guard that refuses one exists to stop a box
			// escrowing into itself, which is a write-side mistake.
			store, err := escrow.NewEscrowClient(cmd.Context(), "")
			if err != nil {
				return fmt.Errorf("%w.\n\n"+
					"Set the four INFISICAL_ESCROW_* values for the Infisical this box\n"+
					"escrows to. They are the same ones on the cluster's repository.", err)
			}

			payload, err := store.RestoreArtifact(cmd.Context(), escrow.ForCluster(boxID, clusterName), escrow.ArtifactKubeconfig)
			if err != nil {
				return fmt.Errorf("could not read the escrow: %w", err)
			}
			if payload == "" {
				return fmt.Errorf("the escrow holds no kubeconfig for %q.\n\n"+
					"Either the cluster name is wrong, or hub-operator has not yet escrowed\n"+
					"one -- it writes on reconcile, so a cluster that never finished\n"+
					"bootstrapping may have none.", clusterName)
			}

			if out == "" {
				fmt.Print(payload)
				return nil
			}
			// 0600: this is cluster-admin, and the default umask would leave it
			// readable by anyone on the machine.
			if err := os.WriteFile(out, []byte(payload), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", out, err)
			}
			fmt.Fprintf(os.Stderr, "wrote %s (mode 0600)\n", out)
			fmt.Fprintf(os.Stderr, "This is cluster-admin and is not tied to a person. "+
				"Use it, then remove it.\n")
			return nil
		},
	}

	cmd.Flags().StringVar(&clusterName, "cluster", "", "the cluster whose kubeconfig to retrieve (required)")
	cmd.Flags().StringVar(&boxID, "box", "", "the logical hub the escrow is keyed by; box-scoped artefacts live under it (required)")
	cmd.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	return cmd
}
