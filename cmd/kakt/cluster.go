package main

import "github.com/spf13/cobra"

// newClusterCmd groups the operations performed ON a cluster from the hub that
// holds it, as opposed to the declarations that bring one into being.
//
// WHAT THIS REPLACES. These two lived under `kakt spoke`, beside a
// `spoke teardown` that deleted a cluster's servers straight through the
// provider API. That command is gone: it could not tell the management cluster
// it had done so, so used while the box was alive it deleted servers whose
// SpokePool claim was still declared and still reconciled -- and the platform
// rebuilt the cluster, on new servers, under a name whose secret path and backup
// prefix are never reused. Removing a cluster is `kakt tenant remove-cluster`,
// which takes the declaration out of git first and lets the platform do the
// deleting; the infrastructure of a box whose API is gone is
// `kakt teardown --infrastructure-only`, which refuses to take a management
// cluster while a cluster it manages is still there.
//
// "spoke" is not platform vocabulary any more. A box has a management cluster
// and workload clusters -- the words the CRDs, the tenant commands and
// kubefirst's own types use (mgmt | workload).
func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Operate on a workload cluster from the hub that holds it",
		Long: `Recovery operations on a cluster, performed against the management cluster
holding its Cluster API objects.

To add or remove a cluster, use kakt tenant add-cluster and
kakt tenant remove-cluster: a cluster exists because a declaration in the
tenant's repository says so, and it is removed by that declaration leaving.`,
	}

	cmd.AddCommand(newClusterRestorePKICmd())
	cmd.AddCommand(newClusterMatchInventoryCmd())

	return cmd
}
