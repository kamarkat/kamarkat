package main

import (
	"context"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"os"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
	"github.com/spf13/cobra"
)

// kakt verify — does this hub actually serve, and by when?
//
// Bootstrap already reports success when its phases complete, which is not the
// same claim. Everything after the last phase is convergence: ArgoCD pulling
// charts, ESO waiting on a secret store another controller is still writing,
// operators reconciling in no particular order. A tenant watching a workflow go
// green has no way to tell that apart from a platform that is up.
//
// Bounded on purpose. "Not converged in fifteen minutes" is an answer a tenant
// can act on; a check that waits forever is a hung workflow, and one that
// samples once at the end of bootstrap reports failures that fix themselves a
// minute later.
var verifyGitops string

func newVerifyCmd() *cobra.Command {
	var (
		clusterName string
		kubeconfig  string
		timeout     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Wait for a bootstrapped hub to converge, within a fixed budget",
		Long: "Polls the invariants a hub must satisfy before it is serving -- nodes\n" +
			"Ready, secret stores valid, ExternalSecrets synced, Applications Synced\n" +
			"and Healthy, pods Running -- until they all hold or the budget elapses.\n\n" +
			"Reports which invariant is outstanding and what it is waiting on, so a\n" +
			"cluster that is still settling reads differently from one that is stuck.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if kubeconfig == "" {
				if clusterName == "" {
					return fmt.Errorf("--cluster or --kubeconfig is required: there is " +
						"no default hub to verify")
				}
				// Read with your own identity, from the box's escrow -- or from
				// Day-0's working copy while a bootstrap is still running.
				resolved, cleanup, err := escrowKubeconfig(cmd.Context(), verifyGitops, clusterName)
				if err != nil {
					return err
				}
				defer cleanup()
				kubeconfig = resolved
			} else if _, err := os.Stat(kubeconfig); err != nil {
				return fmt.Errorf("no kubeconfig at %s: %w", kubeconfig, err)
			}

			fmt.Printf("[verify] watching %s for up to %v\n", kubeconfig, timeout)
			w := &health.HealthWaiter{
				Checkers: health.ConvergedCheckers(),
				Interval: 15 * time.Second,
				Timeout:  timeout,
				OnCheckPass: func(c health.HealthChecker) {
					fmt.Printf("[verify] ✓ %s\n", c.Name())
				},
			}
			if err := w.Wait(cmd.Context(), kubeconfig); err != nil {
				return fmt.Errorf("the hub did not converge within %v: %w", timeout, err)
			}
			fmt.Println("[verify] ✓ hub converged")
			return nil
		},
	}

	cmd.Flags().StringVar(&clusterName, "cluster", "", "cluster to verify; its kubeconfig is read from the box's escrow with your own identity")
	cmd.Flags().StringVar(&verifyGitops, "gitops-dir", ".", "the box's repository, which declares its escrow")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to the hub kubeconfig, when it is not where Day-0 puts it")
	// Fifteen minutes because that is what convergence has actually taken: images
	// pull, ESO retries on its own interval, and ArgoCD's reconciliation is not
	// instant. Shorter budgets reported failure on clusters that were fine.
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long to wait before reporting what is still outstanding")
	return cmd
}

// escrowKubeconfig resolves a management cluster's kubeconfig the way an operator
// reaches a box: `kakt login`, then the escrowed copy, with their own identity.
//
// The normal path, and the only one that works from a machine that never
// bootstrapped the box. Day-0's working copy under ~/.kamarkat/run/ is tried
// first because during a bootstrap it is the only one that exists yet -- the
// escrow copy is written once the cluster is up -- and it is a file this CLI put
// there, not a credential discovered in a checkout.
//
// The caller owns the cleanup: holding cluster-admin is not something to leave on
// a workstation, and the one thing that knows when the command is finished is the
// command.
func escrowKubeconfig(ctx context.Context, gitopsDir, cluster string) (path string, cleanup func(), err error) {
	cleanup = func() {}
	if working, perr := opstore.KubeconfigPath(cluster); perr == nil {
		if _, serr := os.Stat(working); serr == nil {
			return working, cleanup, nil
		}
	}
	coords, err := hubaccess.Declaration(gitopsDir, cluster)
	if err != nil {
		return "", cleanup, err
	}
	access, err := hubaccess.Open(ctx, coords)
	if err != nil {
		return "", cleanup, err
	}
	_, p, remove, err := access.Kubernetes(ctx, hubaccess.Identity{
		BoxID: coords.BoxID, ClusterName: coords.ClusterName,
	})
	if err != nil {
		return "", cleanup, err
	}
	return p, remove, nil
}
