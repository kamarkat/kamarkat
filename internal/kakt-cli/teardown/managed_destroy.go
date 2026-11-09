package teardown

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// destroyManagedClusters destroys the named workload clusters THROUGH THE
// PLATFORM, before the management cluster is touched.
//
// Deleting a workload cluster's servers at the provider is not a way to remove
// it while its management cluster is alive: the cluster exists because a
// SpokePool claim says so, ArgoCD reconciles that claim with selfHeal, and the
// platform rebuilds the cluster on new servers under a name whose secret path
// and backup prefix are never reused. `kakt spoke teardown` did exactly that and
// has been withdrawn; this is what replaces it, and the difference is that the
// controllers that own the cluster do the deleting.
//
// THE ROOT APPLICATION GOES FIRST, for the same reason `kakt tenant
// remove-cluster` refuses while the declaration is still in git: the management
// cluster's root Application applies registry/clusters/<mgmt>/, so a per-cluster
// Application deleted under it is recreated on the next pass -- and a recreated
// infrastructure Application builds a NEW cluster under a name that must not be
// used again. Here the whole box is going, so the root is removed rather than
// waited on. kubefirst does the same thing in the same order (its registry
// Application, deleted with cascade, before any cloud teardown:
// providers/civo/delete.go).
//
// Then each cluster's own Applications, in the order the platform requires:
// <cluster>-generated before <cluster>-infrastructure, because deleting the
// infrastructure Application destroys the cluster and the other would then have
// nowhere to delete from.
//
// Waited on, with a timeout. The point of going through the platform is that the
// in-cluster controllers deprovision what only they can -- the load balancers the
// CCM owns above all -- and that takes as long as it takes. A failure to finish
// is returned, not logged: the management cluster must not be destroyed on the
// assumption that this worked.
func (o *Orchestrator) destroyManagedClusters(ctx context.Context, clusters []string, timeout time.Duration) error {
	baseArgs, ok := o.resolveKubectlBaseArgs()
	if !ok {
		return fmt.Errorf("destroying %s needs the management cluster's API, and there is no "+
			"Kubernetes access", strings.Join(clusters, ", "))
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	root := o.ClusterName + "-root"
	fmt.Printf("\n[teardown] Removing %s, so nothing recreates what is deleted next.\n", root)
	o.forceDeleteApplication(ctx, baseArgs, root)

	for _, cluster := range clusters {
		removal := tenant.ClusterRemoval{MgmtCluster: o.ClusterName, Name: cluster}
		fmt.Printf("\n[teardown] Destroying %s through the management cluster.\n", cluster)
		for _, app := range removal.Applications() {
			present, err := o.applicationPresent(ctx, baseArgs, app)
			if err != nil {
				return fmt.Errorf("read Application %s on %s: %w", app, o.ClusterName, err)
			}
			if !present {
				fmt.Printf("[teardown]   · %s: already absent\n", app)
				continue
			}
			fmt.Printf("[teardown]   deleting %s (up to %v)\n", app, timeout)
			args := append(append([]string{}, baseArgs...),
				"delete", "application", app, "-n", constants.NamespaceOps,
				"--wait", fmt.Sprintf("--timeout=%s", timeout))
			if out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("delete Application %s: %w\n%s\n\n"+
					"%s was NOT destroyed, so %s has not been touched: it is the only thing "+
					"that can still delete it", app, err, out, cluster, o.ClusterName)
			}
			fmt.Printf("[teardown]   ✓ %s deleted\n", app)
		}
	}
	return nil
}

// applicationPresent reports whether the management cluster still holds an
// Application, distinguishing absence from a failed read.
//
// Absence is not an error: a re-run of a teardown that got this far lands here,
// and "already absent" is the honest thing to print. A failed READ is an error,
// because treating it as absence is how a cluster gets left running by a command
// that reported it gone.
func (o *Orchestrator) applicationPresent(ctx context.Context, baseArgs []string, app string) (bool, error) {
	args := append(append([]string{}, baseArgs...),
		"get", "application", app, "-n", constants.NamespaceOps, "--ignore-not-found", "-o", "name")
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// forceDeleteApplication removes an Application without waiting on it.
//
// Finalizers stripped first: ArgoCD's resources-finalizer turns the delete into a
// cascading delete of everything the Application manages, and for the ROOT that
// is the box's entire object graph -- including the per-cluster Applications this
// function is clearing the way for, deleted in whatever order ArgoCD chooses
// rather than the order the platform requires. Stripping it makes this delete
// what it is meant to be: the root stops reconciling, and nothing else happens.
func (o *Orchestrator) forceDeleteApplication(ctx context.Context, baseArgs []string, app string) {
	patch := append(append([]string{}, baseArgs...),
		"patch", "application", app, "-n", constants.NamespaceOps,
		"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
	_ = exec.CommandContext(ctx, "kubectl", patch...).Run()

	del := append(append([]string{}, baseArgs...),
		"delete", "application", app, "-n", constants.NamespaceOps,
		"--ignore-not-found", "--wait=false")
	if out, err := exec.CommandContext(ctx, "kubectl", del...).CombinedOutput(); err != nil {
		// Reported, not fatal. A box whose root Application is already gone --
		// a repeated teardown, or one scaffolded before root.yaml existed -- is
		// in the state this step is trying to reach.
		fmt.Printf("[teardown]   note: %s not deleted (%v); if it is still reconciling, "+
			"the deletes below may be undone\n", app, err)
		if o.Debug {
			fmt.Printf("[DEBUG] %s\n", out)
		}
	}
}
