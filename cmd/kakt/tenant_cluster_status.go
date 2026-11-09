package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/kubectl"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/teardown"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// newClusterStatusCmd answers "is it actually gone yet", by observation.
//
// READ-ONLY, and that is the point of it existing at all. Removing a workload
// cluster is a commit (`remove-cluster`, then push), and a commit returns at once
// while the cluster takes minutes to unwind. The withdrawn `--destroy` step
// waited and reported; nothing replaces the waiting, so this replaces the
// reporting -- and it reports what it SAW, never what the repository says should
// have happened. "Removed from git" is not "destroyed", and a status command that
// conflated them would be the same defect as a teardown that read a missing
// kubeconfig as "already gone".
//
// Four sources, in increasing order of authority about existence: the working
// tree, the management cluster's Applications, the claim and Cluster API object,
// and the provider's own inventory. The last is the only one that can say the
// servers are gone, which is why `removed` is not reported without it.
func newClusterStatusCmd() *cobra.Command {
	var (
		name       string
		mgmt       string
		gitopsDir  string
		kubeconfig string
	)

	cmd := &cobra.Command{
		Use:          "cluster-status",
		Short:        "Report what is actually left of a workload cluster",
		SilenceUsage: true,
		Long: `Report the observed state of a workload cluster: declared, deleting, removed,
failed or unknown.

Nothing here changes anything. It reads the declaration in your working tree, the
two Applications the management cluster holds for the cluster, its SpokePool claim
and Cluster API object, and the provider's inventory for resources carrying the
cluster's ownership label -- and prints all of it before the verdict, so the
verdict can be checked.

removed is reported only when the provider has been asked and reports nothing
owned by the cluster. A declaration removed from git is not a destroyed cluster.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			removal := tenant.ClusterRemoval{GitopsDir: gitopsDir, MgmtCluster: mgmt, Name: name}

			o := tenant.Observation{
				Cluster:      name,
				Declared:     removal.Declared(),
				Applications: map[string]tenant.Presence{},
				Claim:        tenant.Unknown,
				CAPICluster:  tenant.Unknown,
			}

			// Hub access the same way every other command gets it: the box's
			// declaration names its escrow, and the kubeconfig and provider
			// credential are read with the operator's own identity. Unreachable is
			// reported as unknown rather than guessed around -- see Classify.
			kube, token, cleanup, err := resolveHubAccess(ctx, gitopsDir, mgmt, kubeconfig, false)
			if err != nil {
				fmt.Printf("the management cluster could not be reached: %v\n", err)
			} else {
				defer cleanup()
				for _, app := range removal.Applications() {
					o.Applications[app] = observeApplication(ctx, kube, "application", app)
				}
				o.Claim = observeApplication(ctx, kube, "spokepool", name)
				o.CAPICluster = observeClusterAPIObject(ctx, kube, name, &o)
			}

			if strings.TrimSpace(token) != "" {
				inv, invErr := teardown.ProviderInventory(ctx, token, []string{name})
				if invErr != nil {
					fmt.Printf("the provider inventory could not be read: %v\n", invErr)
				} else {
					o.ProviderQueried = true
					o.ProviderOwned = teardown.OwnedByCluster(inv.Resources(), []string{name})[name]
				}
			}

			state, why := tenant.Classify(o)
			o.Render(os.Stdout, state, why)

			// Non-zero on the one state that needs someone to act. deleting is
			// progress and declared is steady state; unknown is reported, not
			// failed, because an unreadable cluster is not a broken one.
			if state == tenant.ClusterFailed {
				return fmt.Errorf("%s needs attention", name)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the workload cluster to report on (required)")
	f.StringVar(&mgmt, "mgmt-cluster", "", "the management cluster that holds it (required)")
	f.StringVar(&gitopsDir, "gitops-dir", ".", "the tenant repository")
	f.StringVar(&kubeconfig, "kubeconfig", "",
		"use this kubeconfig instead of reading the escrowed one")
	for _, required := range []string{"name", "mgmt-cluster"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

// observeApplication reports what the management cluster says about one object in
// platform-ops.
//
// A FAILED READ IS Unknown, not Absent. Treating "kubectl could not tell me" as
// "it is gone" is how a cluster gets reported removed while it runs, and this
// command exists to not do that.
func observeApplication(ctx context.Context, kubeconfig, kind, name string) tenant.Presence {
	k := &kubectl.Executor{}
	out, err := k.Output(ctx, "--kubeconfig", kubeconfig, "-n", constants.NamespaceOps,
		"get", kind, name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return tenant.Unknown
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		return tenant.ApplicationPresence(false, "")
	}
	var obj struct {
		Metadata struct {
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return tenant.Unknown
	}
	return tenant.ApplicationPresence(true, obj.Metadata.DeletionTimestamp)
}

// observeClusterAPIObject reports on the Cluster API Cluster, and records the
// conditions it is failing on.
//
// The conditions are carried into the observation verbatim rather than
// summarised: a deletion that is stuck is stuck on something nameable, and the
// name is the whole value of asking.
func observeClusterAPIObject(ctx context.Context, kubeconfig, name string, o *tenant.Observation) tenant.Presence {
	k := &kubectl.Executor{}
	out, err := k.Output(ctx, "--kubeconfig", kubeconfig, "-A",
		"get", "clusters.cluster.x-k8s.io", "--field-selector", "metadata.name="+name,
		"-o", "json")
	if err != nil {
		return tenant.Unknown
	}
	var list struct {
		Items []struct {
			Metadata struct {
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return tenant.Unknown
	}
	if len(list.Items) == 0 {
		return tenant.ApplicationPresence(false, "")
	}
	item := list.Items[0]
	for _, c := range item.Status.Conditions {
		if c.Status == "False" && c.Reason != "" {
			o.Failures = append(o.Failures,
				fmt.Sprintf("Cluster %s: %s is False (%s) %s", name, c.Type, c.Reason, c.Message))
		}
	}
	return tenant.ApplicationPresence(true, item.Metadata.DeletionTimestamp)
}
