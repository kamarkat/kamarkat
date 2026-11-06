package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/kubectl"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// newRemoveWorkloadClusterCmd is add-cluster's inverse.
//
// Two invocations, not one, because the management cluster's root Application
// self-heals: an Application deleted while its file is still in git comes back
// on the next pass, and the cluster with it. So the declaration is removed and
// pushed first, and only then is anything deleted on the management cluster --
// which the second invocation confirms from the root's own view of git before
// it deletes.
func newRemoveWorkloadClusterCmd() *cobra.Command {
	var removal tenant.ClusterRemoval
	var destroy bool
	var kubeconfig string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:          "remove-cluster",
		Short:        "Remove a workload cluster: its declaration, then the cluster",
		SilenceUsage: true,
		Long: `Remove a workload cluster from a tenant's box, in two steps.

Without --destroy, it checks that nothing still names the cluster -- no
application, and not the box's own burst pool -- and deletes the declaration from
the repository: registry/clusters/<name>/, and the cluster's Applications in the
management cluster's directory, <name>-infrastructure.yaml and
<name>-generated.yaml. Nothing is destroyed yet -- the management cluster never
deletes a cluster because its declaration disappeared. Commit and push.

With --destroy, once the management cluster has synced that commit, it deletes
the cluster's Applications on the management cluster: <name>-generated, then
<name>-infrastructure, which destroys the cluster and its nodes. It refuses while
the management cluster still reads the declaration from git, and while it cannot
tell, because an Application deleted under either condition is recreated -- and a
recreated infrastructure Application builds a new cluster under a name that is
not meant to be used again.

The cluster's secrets and backups stay behind, and its name is never reused.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !destroy {
				removed, err := removal.RemoveDeclaration()
				if err != nil {
					return err
				}
				fmt.Printf("✓ declaration of %q removed\n", removal.Name)
				for _, p := range removed {
					fmt.Printf("  %s\n", p)
				}
				fmt.Println("\nNothing is destroyed yet. Commit and push, wait for the management cluster")
				fmt.Printf("to sync, then run this again with --destroy to destroy %s.\n", removal.Name)
				return nil
			}
			return destroyWorkloadCluster(cmd, removal, kubeconfig, timeout)
		},
	}
	f := cmd.Flags()
	f.StringVar(&removal.GitopsDir, "gitops-dir", ".", "the tenant repository")
	f.StringVar(&removal.MgmtCluster, "mgmt-cluster", "", "the management cluster that created this one (required)")
	f.StringVar(&removal.Name, "name", "", "the workload cluster to remove (required)")
	f.BoolVar(&destroy, "destroy", false,
		"destroy the cluster on the management cluster, once its declaration is removed and pushed")
	f.StringVar(&kubeconfig, "kubeconfig", "",
		"the management cluster's kubeconfig (default: k8-secrets/kubeconfig/<mgmt-cluster>.kubeconfig in --gitops-dir)")
	f.DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for each Application to be deleted")
	for _, required := range []string{"mgmt-cluster", "name"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

// destroyWorkloadCluster deletes the cluster's Applications on the management
// cluster, after confirming the root no longer applies its declaration.
func destroyWorkloadCluster(cmd *cobra.Command, r tenant.ClusterRemoval, kubeconfig string, timeout time.Duration) error {
	if r.Declared() {
		return fmt.Errorf("%q is still declared in %s: run without --destroy first, then commit "+
			"and push.\n\nDeleting its Applications while the declaration is in git does not "+
			"hold: the management cluster's root Application recreates them, and the cluster "+
			"with them", r.Name, r.GitopsDir)
	}
	if kubeconfig == "" {
		kubeconfig = filepath.Join(r.GitopsDir, "k8-secrets", "kubeconfig", r.MgmtCluster+".kubeconfig")
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		return fmt.Errorf("no kubeconfig at %s: %w.\nPass --kubeconfig with the management "+
			"cluster's", kubeconfig, err)
	}

	ctx := cmd.Context()
	k := &kubectl.Executor{}
	base := []string{"--kubeconfig", kubeconfig, "-n", "platform-ops"}

	// The root's own view of git. prune: false means a file removed from git
	// stays applied and is reported requiresPruning; an entry WITHOUT that flag
	// is one the root still reads from git, and would recreate.
	out, err := k.Output(ctx, append(base, "get", "application", r.RootApplication(), "-o", "json")...)
	if err != nil {
		return fmt.Errorf("read %s on the management cluster: %v\n%s", r.RootApplication(), err, out)
	}
	var root struct {
		Status struct {
			Resources []struct {
				Kind            string `json:"kind"`
				Name            string `json:"name"`
				RequiresPruning bool   `json:"requiresPruning"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		return fmt.Errorf("parse %s: %w", r.RootApplication(), err)
	}
	// The decision belongs to the library, which is where it can be tested
	// against a stale root without a cluster to stale.
	observed := make([]tenant.RootResource, 0, len(root.Status.Resources))
	for _, res := range root.Status.Resources {
		observed = append(observed, tenant.RootResource{
			Kind: res.Kind, Name: res.Name, RequiresPruning: res.RequiresPruning,
		})
	}
	if err := r.DestroyBlocked(observed); err != nil {
		return err
	}

	var deleted, absent int
	for _, app := range r.Applications() {
		out, err := k.Output(ctx, append(base, "get", "application", app, "--ignore-not-found", "-o", "name")...)
		if err != nil {
			return fmt.Errorf("read %s: %v\n%s", app, err, out)
		}
		if strings.TrimSpace(string(out)) == "" {
			fmt.Printf("  · %s: already absent\n", app)
			absent++
			continue
		}
		fmt.Printf("  deleting %s (up to %v)\n", app, timeout)
		out, err = k.Output(ctx, append(base, "delete", "application", app,
			"--wait", fmt.Sprintf("--timeout=%s", timeout))...)
		if err != nil {
			return fmt.Errorf("delete %s: %v\n%s", app, err, out)
		}
		fmt.Printf("  ✓ %s deleted\n", app)
		deleted++
	}

	// Nothing deleted is not a failure when every Application is verifiably gone:
	// that is what a re-run of a destroy that succeeded looks like, and it was
	// reported as "nothing was destroyed. Check the name, and --kubeconfig is the
	// management cluster's" -- blaming the arguments for a cluster that is
	// already gone. A wrong name or a wrong box reads the same way, so the state
	// is named rather than diagnosed.
	if deleted == 0 && absent == len(r.Applications()) {
		fmt.Printf("\n✓ %s already destroyed: the management cluster holds no Applications for it\n", r.Name)
		fmt.Println("  (if you expected otherwise, check --name and that --kubeconfig is this box's)")
		return nil
	}

	fmt.Printf("\n✓ %s destroyed\n", r.Name)
	fmt.Println("\nStill there, until you delete them:")
	fmt.Printf("  secrets  /spoke-pool/%s/ in the box's secret store\n", r.Name)
	fmt.Printf("  backups  the %s/ prefix in your object storage\n", r.Name)
	fmt.Printf("\nThe name %s is not reused: its secret path and backup prefix outlive it.\n", r.Name)
	return nil
}
