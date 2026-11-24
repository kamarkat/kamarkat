package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/openbao"
)

func newOpenBaoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "openbao",
		Short: "Exceptional operations on this box's secret store (ADR-117)",
	}
	cmd.AddCommand(newOpenBaoRecoverCmd())
	return cmd
}

func newOpenBaoRecoverCmd() *cobra.Command {
	var gitopsDir, cluster string
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Bring a restored secret store or application database back to a trusted state",
		Long: `Recover the box's secret store after a database restore (ADR-117 §7, ADR-092).

Run it as the box's administrator, inside ADR-092's recovery mode: after you have
pointed a restored database's descriptor at its database-recovery overlay (and,
for openbao-db, the OpenBao component at openbao-recovery, which keeps
consumers out). It refuses outside recovery mode, and refuses to touch a
restored store that consumers can still reach.

For a restored openbao-db it:
  - delivers the seal configuration that was in force at the restore's recovery
    point, from the escrow's history, and restarts OpenBao onto the restored store;
  - logs in freshly as the recovery identity (never with a restored token);
  - waits for hub-operator to re-apply the box's complete authorization -- policies,
    roles, mounts and every spoke's verification key -- to this incarnation;
  - revokes every token restored with the store, keeping only its own session;
  - re-establishes each application database password through OpenBao and proves
    a login with it, once that application's own database is serving.

For an application database restored alone, it re-establishes that application's
password only.

Every step is recorded in OpenBao, per restored database, only after its effect
is verified. Interrupted, it resumes where it stopped. It exits non-zero until
every database in recovery mode is completely recovered, and then tells you to
settle the descriptors, which restores consumer admission.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			coords, err := hubaccess.Declaration(gitopsDir, cluster)
			if err != nil {
				return err
			}
			access, err := hubaccess.Open(ctx, coords)
			if err != nil {
				return err
			}
			cfg, kubeconfig, cleanup, err := access.Kubernetes(ctx,
				hubaccess.Identity{BoxID: coords.BoxID, ClusterName: coords.ClusterName})
			if err != nil {
				return err
			}
			defer cleanup()
			cs, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return err
			}
			err = openbao.Recover(ctx, openbao.RecoverOptions{
				Live:   openbao.ClusterLive{CS: cs, Kubeconfig: kubeconfig},
				Escrow: access.Escrow(),
				Out:    os.Stdout,
			})
			if errors.Is(err, openbao.ErrRecoveryIncomplete) {
				// Already explained on stdout; the exit status says "not done".
				os.Exit(2)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&gitopsDir, "gitops-dir", ".", "the box's repository")
	cmd.Flags().StringVar(&cluster, "cluster", "", "the management cluster, when the repository declares more than one")
	return cmd
}
