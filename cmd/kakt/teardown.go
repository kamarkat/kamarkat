package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/teardown"
	"github.com/spf13/cobra"
)

var (
	teardownDNSOnly   bool
	teardownDNSOwner  string
	teardownDNSZone   string
	teardownForce     bool
	teardownConfirm   bool
	teardownManaged   []string
	teardownGitops    string
	teardownInfraOnly string
	teardownDestroy   string
	teardownTimeout   time.Duration
	// teardownKubeconfig is the explicit override, for a box whose kubeconfig was
	// never escrowed -- a bootstrap that failed before hub-operator reconciled.
	teardownKubeconfig string
)

func newTeardownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Destroy a box's management cluster, once the clusters it manages are gone",
		Long: `Destroy a box's management cluster: its cloud infrastructure, its CAPI
resources, the DNS records it published, and this machine's local artifacts for it.

THE MANAGEMENT CLUSTER GOES LAST. It refuses while a cluster it manages is still
there, and it does not take those clusters with it. The management cluster holds
their CAPI objects and the only controller that can deprovision their load
balancers, so a box torn down from the top leaves them running, billing, and
undeletable by the platform. Remove each one first with
` + "`kakt tenant remove-cluster`" + `.

A resource is deleted only when the provider says the cluster owns it:
caph-cluster-<cluster>=owned. Nothing is matched by name, prefix or substring.`,
		PreRunE: validateTeardownFlags,
		RunE:    runTeardown,
	}

	cmd.Flags().StringVar(&clusterName, "name", "", "management cluster name (required)")
	cmd.Flags().BoolVar(&teardownForce, "force", true, "Force deletion of resources (default true)")
	cmd.Flags().BoolVar(&teardownConfirm, "confirm", false, "Confirm deletion (required for safety)")
	cmd.Flags().BoolVar(&debug, "debug", false, "Enable verbose logging")
	// For a box that is already gone and left records behind. external-dns
	// ignores a record whose owner does not match, silently, so an orphan cannot
	// be reclaimed by any later box -- the hostname resolves to a decommissioned
	// address until someone deletes it.
	cmd.Flags().BoolVar(&teardownDNSOnly, "dns-only", false,
		"release this box's DNS records and nothing else")
	cmd.Flags().StringVar(&teardownDNSOwner, "dns-owner", "",
		"external-dns txt-owner-id whose records to release (default: read from the cluster)")
	cmd.Flags().StringVar(&teardownDNSZone, "dns-zone", "",
		"the zone those records live in, e.g. dev.nutgraf.in (default: read from the cluster)")
	// The cluster list normally comes from the management cluster. One that is
	// already gone answers nothing, which is exactly when the clusters it held get
	// stranded: their servers carry their own names, mention the management
	// cluster nowhere, and survive the teardown
	// that was supposed to take them. Naming the pool here is what the box
	// declares, and it outlives the cluster.
	// Where the box's state and kubeconfig actually are. bootstrap writes both
	// under --gitops-dir; teardown resolved them against its own working
	// directory, so a teardown run from anywhere else read an empty cluster and
	// tore down nothing while reporting success.
	cmd.Flags().StringVar(&teardownGitops, "gitops-dir", "",
		"the box's repository, which declares its escrow and its box id")
	// The box's own name. Every cluster it creates derives from the domain
	// (ADR-082), so one prefix names every server it owns -- and unlike the hub's
	// cluster list or the tenant repository, it is still available once both are
	// gone, which is precisely when a teardown needs it.
	cmd.Flags().StringVar(&teardownKubeconfig, "kubeconfig", "",
		"use this kubeconfig instead of reading the escrowed one (for a box that never "+
			"finished bootstrapping; the provider credential still comes from the box's escrow)")
	// --tenant IS GONE, deliberately and loudly: an unknown flag is an error, so a
	// script that passed it stops rather than quietly losing the scope it thought
	// it had. It named the box's prefix, and because a box's clusters share that
	// prefix by construction it made a hub teardown sweep its workload clusters'
	// servers. Ownership is the CAPH label on each resource; the target set is the
	// management cluster plus the clusters this run names.
	cmd.Flags().StringVar(&teardownInfraOnly, "infrastructure-only", "",
		"destroy the provider infrastructure of a box whose Kubernetes API cannot be "+
			"reached, naming the clusters to destroy (comma-separated). The workload "+
			"clusters named here are destroyed first and the management cluster last. "+
			"Kubernetes cleanup is reported as not performed")
	// NAMES, DOES NOT AUTHORIZE. A cluster named here is one this box manages, and
	// a management cluster is torn down only once the clusters it manages are gone
	// -- so this widens the ORDERING GATE, not the target set. To destroy a
	// workload cluster, name it in --infrastructure-only.
	//
	// IT WAS --spoke. The platform says management cluster and workload cluster --
	// the words the tenant commands, the CRDs and kubefirst's own types use -- and
	// the old name survived here alone. Withdrawn rather than aliased, so a script
	// that passes it stops instead of silently losing the ordering evidence it
	// believed it had supplied.
	// Emptying the box in one run, with the platform doing the deleting.
	//
	// The capability `kakt spoke teardown` used to provide, minus what made it
	// dangerous: it deleted a cluster's servers at the provider while the
	// management cluster was alive, so the SpokePool claim was still reconciled
	// and the platform rebuilt the cluster on new servers under a name that is
	// never reused. Here the cluster's own Applications are deleted on the
	// management cluster, which is what destroys it, and the management cluster is
	// touched only once that has finished.
	cmd.Flags().StringVar(&teardownDestroy, "destroy-managed", "",
		"also destroy these workload clusters first, through the management cluster "+
			"(comma-separated). Without it, a box with managed clusters is refused")
	cmd.Flags().DurationVar(&teardownTimeout, "timeout", 30*time.Minute,
		"how long to wait for each workload cluster's own deletion")
	cmd.Flags().StringSliceVar(&teardownManaged, "managed-cluster", nil,
		"workload cluster(s) this box declares, used when the management cluster cannot "+
			"be reached to list them (repeatable)")

	// --name is required for a teardown, which destroys a named cluster. A
	// --dns-only run destroys no cluster and names records by owner instead.
	cmd.MarkFlagRequired("name")

	return cmd
}

func validateTeardownFlags(cmd *cobra.Command, args []string) error {
	if clusterName == "" {
		return fmt.Errorf("--name is required")
	}

	if !teardownConfirm {
		return fmt.Errorf("teardown requires --confirm flag for safety")
	}

	return nil
}

func runTeardown(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if teardownDNSOnly {
		fmt.Printf("🧹 Releasing DNS records owned by %q in %s\n", teardownDNSOwner, teardownDNSZone)
	} else {
		fmt.Printf("🗑️  Tearing down Hub Cluster: %s\n", clusterName)
	}

	// Hub access and the provider credential, resolved BEFORE anything is
	// destroyed and from the box's own declaration rather than from this machine.
	//
	// Skipped for --dns-only, which touches no cluster and no server: it exists for
	// a box that is already gone, and requiring an escrow read to delete a DNS
	// record would make the recovery path depend on the thing being recovered from.
	// The explicit scope of a recovery run: the clusters named on the flag, and
	// nothing a prefix might also match.
	var infraOnly bool
	var declaredScope []string
	scopeFrom := func(raw string) {
		for _, name := range strings.Split(raw, ",") {
			if n := strings.TrimSpace(name); n != "" && n != clusterName {
				declaredScope = append(declaredScope, n)
			}
		}
	}
	if raw := strings.TrimSpace(teardownInfraOnly); raw != "" {
		infraOnly = true
		scopeFrom(raw)
	}
	if raw := strings.TrimSpace(teardownDestroy); raw != "" {
		if infraOnly {
			return fmt.Errorf("--destroy-managed and --infrastructure-only both name the " +
				"clusters to destroy. --infrastructure-only is the recovery form, where the " +
				"platform cannot be asked to delete anything; pass one of them")
		}
		scopeFrom(raw)
	}

	var access teardown.Orchestrator
	if !teardownDNSOnly {
		kubeconfig, token, cleanup, err := resolveHubAccess(ctx, teardownGitops, clusterName,
			teardownKubeconfig, infraOnly)
		if err != nil {
			return err
		}
		defer cleanup()
		access.Kubeconfig = kubeconfig
		access.ProviderToken = token
	}

	orchestrator := &teardown.Orchestrator{
		Kubeconfig:            access.Kubeconfig,
		ProviderToken:         access.ProviderToken,
		InfrastructureOnly:    infraOnly,
		ManagedDestroyTimeout: teardownTimeout,
		ClusterName:           clusterName,
		Force:                 teardownForce,
		Debug:                 debug,
		DNSOnly:               teardownDNSOnly,
		GitopsDir:             teardownGitops,

		// What this run destroys, in order: the named workload clusters, then the
		// management cluster. These names were parsed and then DROPPED -- the
		// orchestrator never received them, so `--infrastructure-only hub,workload`
		// silently destroyed only the hub while reporting both as the scope.
		Scope: declaredScope,

		Managed: teardownManaged,
		DNS: teardown.DNSOverride{
			Owner: teardownDNSOwner,
			Zone:  teardownDNSZone,
		},
	}

	if err := orchestrator.Run(ctx); err != nil {
		return fmt.Errorf("teardown failed: %w", err)
	}

	// Says what happened, not what the command is called. A --dns-only run tore
	// down no cluster, and announcing that it did is how a report stops being
	// evidence -- the same defect as a bootstrap banner listing gates it no
	// longer runs.
	if teardownDNSOnly {
		n := orchestrator.ReleasedDNSRecords
		if n == 0 {
			fmt.Printf("\n✓ No records in %s are owned by '%s'. Nothing to release, and no cluster was torn down.\n",
				teardownDNSZone, teardownDNSOwner)
		} else {
			fmt.Printf("\n✓ Released %d DNS record(s) owned by '%s'. No cluster was torn down.\n", n, teardownDNSOwner)
			fmt.Println("  Resolvers may serve the old answers until their TTL expires.")
		}
	} else {
		fmt.Printf("\n✓ Cluster '%s' successfully torn down\n", clusterName)
	}
	return nil
}

// resolveHubAccess gets a hub's admin kubeconfig and its provider credential
// without either of them having to be on this machine already.
//
// The order is deliberate. An explicitly passed --kubeconfig wins, because an
// operator naming a file is making a statement -- it is the supported way in for
// a box that never finished bootstrapping and therefore escrowed nothing.
// Otherwise the box's declaration says which escrow holds its artefacts, and the
// escrow is read with whoever is authenticated: the developer's own session from
// `kakt login`, or the machine identity in the environment when this is CI.
//
// Nothing is guessed. There is no search of .state, no k8-secrets directory, and
// no kind context -- the four sources that made a hub operable from exactly one
// laptop and failed open when it was the wrong one.
func resolveHubAccess(ctx context.Context, gitopsDir, clusterName, explicitKubeconfig string, infraOnly bool) (kubeconfig, providerToken string, cleanup func(), err error) {
	cleanup = func() {}

	// The provider credential is read from the environment first for the same
	// reason the machine identity is: a pipeline that exports it must behave the
	// way it always has, and an operator overriding it is being explicit.
	// NOT from the environment. A token exported in a shell authenticates
	// perfectly against a DIFFERENT provider project, where this box owns nothing
	// -- so a teardown would delete nothing and report success. The box's own
	// credential is the only one that can answer what the box owns.
	providerToken = ""

	if infraOnly && strings.TrimSpace(explicitKubeconfig) != "" {
		return "", "", cleanup, fmt.Errorf("--kubeconfig and --infrastructure-only ask for " +
			"opposite things: one supplies Kubernetes access, the other says there is none")
	}
	if strings.TrimSpace(explicitKubeconfig) != "" {
		if _, statErr := os.Stat(explicitKubeconfig); statErr != nil {
			return "", "", cleanup, fmt.Errorf("--kubeconfig %s: %w", explicitKubeconfig, statErr)
		}
		if providerToken == "" {
			return "", "", cleanup, fmt.Errorf("--kubeconfig names a file, but there is no provider " +
				"credential: drop --kubeconfig so both are read from the box's escrow, which " +
				"is where this box's own credential lives")
		}
		return explicitKubeconfig, providerToken, cleanup, nil
	}

	coords, err := hubaccess.Declaration(gitopsDir, clusterName)
	if err != nil {
		return "", "", cleanup, err
	}
	access, err := hubaccess.Open(ctx, coords)
	if err != nil {
		return "", "", cleanup, err
	}
	id := hubaccess.Identity{BoxID: coords.BoxID, ClusterName: coords.ClusterName}

	// A recovery run asks for no Kubernetes access and must not acquire any: it
	// has already been told the API cannot be reached, and the orchestrator
	// refuses the mode if it turns out it can.
	if !infraOnly {
		_, path, removeKubeconfig, kubeErr := access.Kubernetes(ctx, id)
		if kubeErr != nil {
			return "", "", cleanup, kubeErr
		}
		cleanup = removeKubeconfig
		kubeconfig = path
	}

	if providerToken == "" {
		providerToken, err = access.Provider(ctx, id, "hetzner")
		if err != nil {
			cleanup()
			return "", "", func() {}, err
		}
	}
	return kubeconfig, providerToken, cleanup, nil
}
