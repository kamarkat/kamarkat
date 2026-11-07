package teardown

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// Orchestrator manages the forceful teardown process
type Orchestrator struct {
	ClusterName string
	Force       bool
	Debug       bool

	// DNS states the box's external-dns identity when the cluster cannot be asked
	// for it. Empty on a normal teardown, which reads it from the live cluster.
	DNS DNSOverride

	// DNSOnly releases the records and touches nothing else. For a box that is
	// already gone and left records behind: there is no cluster to tear down, and
	// the records are the only thing remaining.
	DNSOnly bool

	// ReleasedDNSRecords is how many records the run actually released, so the
	// caller can report the outcome instead of the intent.
	ReleasedDNSRecords int

	// InfrastructureOnly destroys the provider resources of a box whose Kubernetes
	// API cannot be reached, and says so in the result.
	//
	// EXPLICIT, never inferred. A missing kubeconfig used to mean "carry on without
	// the cluster", so an operator who simply ran this from the wrong directory got
	// the infrastructure-only path and a success line -- the Kubernetes half of the
	// teardown skipped, the load balancers never drained, the DNS records never
	// released, and nothing saying so. The two are different operations with
	// different outcomes, so the caller chooses which one it is asking for.
	InfrastructureOnly bool

	// Outstanding names the cleanup this run could not perform, so a caller can
	// report incompleteness rather than success.
	Outstanding []string

	// Kubeconfig is the hub's admin kubeconfig, RESOLVED BY THE CALLER.
	//
	// Supplied rather than searched for. It used to be looked up here, in four
	// places in order -- a path recorded in .state, k8-secrets/kubeconfig/, a
	// kubeconfig beside the repository, and finally a kind context -- every one of
	// them an artefact of the machine that ran bootstrap. So a hub could only be
	// torn down from that laptop, and the last candidate failed OPEN: on a cloud
	// box `--context=kind-<cluster>` resolves to nothing, every kubectl read came
	// back empty, and the teardown destroyed the servers while skipping the drain,
	// the finalizer strip and the DNS release.
	//
	// cmd/kakt resolves it through hubaccess, which reads the escrowed kubeconfig
	// with the operator's own identity and writes it to a temporary file for the
	// life of the command. An operator who means to use a particular file passes
	// --kubeconfig, which is a statement rather than a search.
	Kubeconfig string

	// ProviderToken is the cloud credential, also resolved by the caller.
	//
	// Read from the escrow for the same reason: it lived in HCLOUD_TOKEN or in
	// k8-secrets/hetzner/token, so the one machine that could delete the servers
	// was the one that created them.
	ProviderToken string

	// GitopsDir is the tenant repository this box was bootstrapped from, and is
	// where its state and kubeconfig actually live.
	//
	// Without it teardown resolved both against its own working directory while
	// bootstrap wrote them under --gitops-dir, so a teardown launched from
	// anywhere else found no state, fell through to --context=kind-<cluster>,
	// and every kubectl call reached localhost:8080. Nothing failed: the cluster
	// simply answered nothing. It listed no spokes and left their servers
	// running, read no DNS ownership and left the records orphaned, and reported
	// a completed teardown -- the box was reachable the whole time.
	GitopsDir string

	// TenantPrefix IS GONE. It named the box's prefix so that servers could be
	// matched by it, and a box's clusters share that prefix by construction, so it
	// made a hub teardown sweep its workload clusters' servers too. Ownership is
	// the CAPH label and the target set is explicit; there is nothing a prefix can
	// add that is not either already known or a guess.

	// Spokes names the spoke clusters this box declares, for when the hub cannot
	// be asked for them.
	//
	// spokeClusterNames reads the list from the running hub, which is the right
	// source while there is one and returns nothing once there is not -- and a
	// hub that is already gone is exactly when its spokes get stranded. The
	// servers carry the SPOKE's name and label and mention the hub nowhere, so a
	// teardown scoped to the hub walks past them, and reportOrphanedSpokes
	// correctly refuses to delete on a label that proves only "some cluster".
	//
	// A name supplied here is different evidence: it comes from what this box
	// DECLARES, which survives the cluster. It is the same lookup local-e2e and
	// the tenant workflow use to know which pool a box has.
	Spokes []string
}

// Run executes immediate forceful deletion of Kubernetes CAPI resources, Hetzner Cloud infra, Kind/Docker, and local state.
func (o *Orchestrator) Run(ctx context.Context) error {
	// Records only. Everything below this assumes a cluster to dismantle; a box
	// whose records outlived it has none, and running the rest would report
	// failures for infrastructure that is correctly absent.
	if o.DNSOnly {
		own, ok := o.resolveDNSOwnership(ctx)
		if !ok {
			return fmt.Errorf("cannot release DNS records: no owner, zone or token.\n\n" +
				"For a box that still exists these are read from it. For one that does not,\n" +
				"state them: --dns-owner, --dns-zone, and HETZNER_DNS_API_TOKEN in the environment.")
		}
		o.ReleasedDNSRecords = o.releaseDNSRecords(ctx, own)
		return nil
	}

	if o.Debug {
		fmt.Println("[DEBUG] Teardown.Run() started")
		fmt.Printf("[DEBUG] ClusterName: %s, Force: %v\n", o.ClusterName, o.Force)
	}

	fmt.Printf("\n[teardown] Starting forceful teardown of cluster '%s'...\n", o.ClusterName)

	// Step 1: Let the CCM deprovision its own load balancers while it is still
	// alive. Must run before anything else is destroyed — see the function comment.
	// Ask the hub which spokes it owns while it can still answer. Everything
	// below this line destroys, and after it nothing knows.
	// Whether the box can be reached at all, established once and reported.
	// Unreachable is not "nothing to do": every read below returns empty, and a
	// teardown that destroys nothing and says it succeeded is worse than one
	// that fails, because the servers keep billing and nobody is told.
	_, reachable := o.resolveKubectlBaseArgs()
	switch {
	case reachable && o.InfrastructureOnly:
		// Asked for the recovery path on a box that can be read. Refused, because
		// the full teardown is available and the recovery path leaves work behind:
		// load balancers the CCM would have deprovisioned, finalizers, and the DNS
		// records only the cluster can tell us the owner of.
		return fmt.Errorf("--infrastructure-only was given, but this box's Kubernetes API " +
			"IS reachable.\n\nRun the normal teardown so the cluster's own cleanup happens " +
			"first: drop the flag. The recovery path is for a box whose API cannot be " +
			"reached, and it leaves DNS records and load balancers behind by construction")
	case !reachable && !o.InfrastructureOnly:
		// Refused rather than downgraded. This is the case the brief calls out and
		// the one that used to pass silently.
		return fmt.Errorf("no Kubernetes access to '%s', so the cluster's own cleanup "+
			"cannot run.\n\n"+
			"That is a different operation from a normal teardown, and this command will "+
			"not quietly become it: the load balancers the CCM owns, the finalizers and "+
			"the DNS records this box published all need the API.\n\n"+
			"Either make the API reachable -- `kakt login` and an escrowed kubeconfig, or "+
			"--kubeconfig <path> -- or ask for the recovery path explicitly:\n\n"+
			"  --infrastructure-only %s[,<workload cluster>...]\n\n"+
			"which destroys provider resources by their CAPH ownership labels and reports "+
			"the Kubernetes cleanup as not performed", o.ClusterName, o.ClusterName)
	case !reachable:
		fmt.Printf("[teardown] Infrastructure-only teardown of '%s'.\n", o.ClusterName)
		fmt.Println("[teardown] The Kubernetes API is not being used, so none of the cluster's")
		fmt.Println("[teardown] own cleanup runs. It is reported as outstanding at the end.")
		o.Outstanding = append(o.Outstanding,
			"load balancers the in-cluster controller owns were not deprovisioned by it",
			"Kubernetes finalizers were not stripped",
			"DNS records were not released: the owner id lives in the cluster")
	}

	spokes := o.spokeClusterNames(ctx)
	if len(spokes) == 0 && len(o.Spokes) > 0 {
		// The hub could not be asked. Fall back to what the box declares. This is
		// a last resort and not the mechanism: the hub is the source of truth and
		// is queried first, above, while it is still alive.
		spokes = o.Spokes
		fmt.Printf("[teardown] The hub could not be asked for its spokes; using the "+
			"declared pool(s): %s\n", strings.Join(spokes, ", "))
	}
	if len(spokes) == 0 && len(o.Spokes) == 0 && !reachable {
		fmt.Println("[teardown] ⚠️  No spoke is known, from the cluster or the command line.")
		fmt.Println("[teardown]     Any spoke servers this box provisioned will be LEFT RUNNING.")
		fmt.Println("[teardown]     Name the pool with --spoke to remove them.")
	}
	if len(spokes) > 0 {
		fmt.Printf("[teardown] This hub provisioned %d spoke cluster(s): %s\n",
			len(spokes), strings.Join(spokes, ", "))
	}

	// Read the box's DNS identity and credential now, for the same reason: both
	// live only in the cluster, and the credential arrives by ExternalSecret and
	// dies with it. The records themselves are released at the end -- external-dns
	// recreates anything it is still watching.
	dnsOwn, hasDNS := o.resolveDNSOwnership(ctx)

	ccmLoadBalancerIPs := o.drainLoadBalancerServices(ctx)

	// Step 2: Strip Kubernetes finalizers & delete CAPI CRs fast (non-blocking)
	o.stripKubernetesFinalizers(ctx)

	// Step 3: Delete Hetzner cloud infrastructure directly via Hetzner API
	if err := o.deleteHetznerResources(ctx, ccmLoadBalancerIPs, spokes); err != nil {
		fmt.Printf("[teardown] ⚠️  Hetzner cloud resource cleanup encountered warnings: %v\n", err)
	}

	// Step 4: Release the DNS records this box published, now that nothing is
	// left to recreate them. A record outliving its cluster is not inert: it
	// keeps resolving to a decommissioned address, and no later box can correct
	// it, because external-dns silently ignores records it does not own.
	if hasDNS {
		o.releaseDNSRecords(ctx, dnsOwn)
	} else {
		fmt.Println("[teardown] ⚠️  DNS ownership could not be read from the cluster, so any")
		fmt.Println("[teardown]     records it published are left in place. If this box served")
		fmt.Println("[teardown]     public hostnames, check the zone: an orphaned record cannot")
		fmt.Println("[teardown]     be reclaimed by a future box and must be deleted by hand.")
	}

	// Step 5: Local Kind, Docker, and state cleanup
	if err := o.localCleanup(ctx); err != nil {
		fmt.Printf("[teardown] ⚠️  Local cleanup encountered warnings: %v\n", err)
	}

	if len(o.Outstanding) > 0 {
		fmt.Printf("\n⚠️  Teardown of '%s' is INCOMPLETE. Outstanding:\n", o.ClusterName)
		for _, item := range o.Outstanding {
			fmt.Printf("   - %s\n", item)
		}
		// Returned as an error so the exit status says so too. A teardown that left
		// work behind and exited 0 is indistinguishable from one that finished, to
		// a person skimming and to every script.
		return fmt.Errorf("%d cleanup operation(s) could not be performed", len(o.Outstanding))
	}

	fmt.Printf("\n✓ Teardown of cluster '%s' completed\n", o.ClusterName)
	return nil
}

// gitopsDirForMessage names where teardown looked, so the warning is actionable
// rather than just negative.
func (o *Orchestrator) gitopsDirForMessage() string {
	if o.GitopsDir != "" {
		return o.GitopsDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "the working directory"
	}
	return wd
}

// resolveKubectlBaseArgs locates the kubeconfig (or kind context) for the cluster
// being torn down and returns the kubectl flags that select it, or ok=false when
// no usable target exists.
//
// Shared by every step that talks to the doomed cluster so they cannot drift onto
// different clusters — a teardown that drains load balancers from one cluster and
// strips finalizers on another would be worse than doing neither.
func (o *Orchestrator) resolveKubectlBaseArgs() (baseArgs []string, ok bool) {
	// One source, and it is the one the caller resolved. No search: see the
	// Kubeconfig field for what the search cost.
	if strings.TrimSpace(o.Kubeconfig) == "" {
		return nil, false
	}
	if strings.HasPrefix(o.Kubeconfig, "--context=") {
		return []string{"--context", strings.TrimPrefix(o.Kubeconfig, "--context=")}, true
	}
	return []string{"--kubeconfig", o.Kubeconfig}, true
}

// spokeClusterNames returns the CAPI clusters this hub provisioned, other than
// the hub itself.
//
// Asked BEFORE anything is destroyed, because the hub is the only thing that
// knows. A spoke's servers carry the spoke's name and the spoke's
// caph-cluster-<spoke> label and mention the hub nowhere, so a teardown scoped
// to the hub's name walks straight past them: one cx33 control plane for
// spoke-pool-hybrid-dev-01 survived every `local-e2e clean` in this repository
// and was still billing when someone thought to look.
//
// Empty when the hub cannot be reached. That is not the same as "no spokes", and
// the caller must not treat it as such -- see reportOrphanedSpokes.
func (o *Orchestrator) spokeClusterNames(ctx context.Context) []string {
	baseArgs, ok := o.resolveKubectlBaseArgs()
	if !ok {
		return nil
	}
	args := append([]string{}, baseArgs...)
	args = append(args, "get", "clusters.cluster.x-k8s.io", "-A",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		return nil
	}

	var spokes []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == o.ClusterName {
			continue
		}
		spokes = append(spokes, name)
	}
	return spokes
}

// reportOrphanedSpokes names the CAPH-managed servers this teardown did not
// touch, so a hub that could not be asked does not silently leave a bill.
//
// It reports and does not delete. The label says a server belongs to SOME
// cluster, not that it belongs to THIS box, and a Hetzner project can hold more
// than one -- deleting on that evidence would make a cleanup command capable of
// destroying a cluster it was never pointed at.
func reportOrphanedSpokes(servers []string) {
	if len(servers) == 0 {
		return
	}
	fmt.Println("[teardown] ⚠️  CAPH-managed servers remain, and this teardown could not")
	fmt.Println("[teardown]     confirm they belong to this box (the hub was unreachable, so")
	fmt.Println("[teardown]     its spoke list could not be read):")
	for _, name := range servers {
		fmt.Printf("[teardown]       %s\n", name)
	}
	fmt.Println("[teardown]     They are NOT deleted. If they are yours:")
	for _, name := range servers {
		fmt.Printf("[teardown]       hcloud server delete %s\n", name)
	}
}

// drainLoadBalancerServices deletes every type=LoadBalancer Service on the cluster
// and gives the Hetzner CCM the chance to deprovision the load balancers it owns,
// while it is still running. It returns the public IPs those services held, so a
// later sweep can reap by IP whatever the CCM did not remove.
//
// This must run before anything else is destroyed. A CCM-created load balancer is
// invisible to the name/label matcher in deleteHetznerResources: the CCM names it
// a<service-uid> and labels it only hcloud-ccm/service-uid, so it carries neither
// the cluster name nor a caph-cluster-* label. The CCM is the only component that
// can deprovision it, and it dies with the cluster — so the load balancer survives
// as a billed orphan that nothing will ever collect.
//
// That is not hypothetical. Four rebuilds on 2026-08-22 leaked four load balancers
// into a five-load-balancer project quota. The fifth request — the spoke's own
// control-plane LB — was refused with resource_limit_exceeded, leaving
// spoke-pool-hybrid-dev-01 at InfrastructureReady=False for five hours while the
// bootstrap polled a terminal error to timeout. See ADR-046 §23.
//
// Order matters in the other direction too: the Service must go first. Deleting the
// cloud load balancer while its Service still exists just makes the CCM recreate it.
func (o *Orchestrator) drainLoadBalancerServices(ctx context.Context) []string {
	baseArgs, ok := o.resolveKubectlBaseArgs()
	if !ok {
		fmt.Println("\n[teardown] ⚠️  No usable kubeconfig; skipping load balancer drain")
		fmt.Println("[teardown]    CCM-owned load balancers will be reaped by name only — see ADR-046 §23")
		return nil
	}

	fmt.Println("\n[teardown] Draining LoadBalancer Services so the CCM deprovisions its load balancers...")

	listCtx, cancelList := context.WithTimeout(ctx, 20*time.Second)
	defer cancelList()

	listArgs := append(append([]string{}, baseArgs...),
		"get", "svc", "--all-namespaces",
		"-o", `jsonpath={range .items[?(@.spec.type=="LoadBalancer")]}{.metadata.namespace}{" "}{.metadata.name}{" "}{.status.loadBalancer.ingress[*].ip}{"\n"}{end}`)
	out, err := exec.CommandContext(listCtx, "kubectl", listArgs...).Output()
	if err != nil {
		fmt.Printf("[teardown] ⚠️  Could not list LoadBalancer Services (%v); continuing\n", err)
		return nil
	}

	type lbService struct{ namespace, name string }
	var services []lbService
	var ips []string

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		services = append(services, lbService{namespace: fields[0], name: fields[1]})
		// Remaining fields are the assigned VIPs. A Hetzner LB publishes IPv4, IPv6
		// and (when attached) a private address; only the IPv4 identifies the load
		// balancer in the API, but collecting all of them costs nothing and the
		// sweep matches exactly.
		ips = append(ips, fields[2:]...)
	}

	if len(services) == 0 {
		fmt.Println("[teardown] ✓ No LoadBalancer Services present")
		return nil
	}

	for _, svc := range services {
		fmt.Printf("[teardown] Deleting LoadBalancer Service: %s/%s\n", svc.namespace, svc.name)
		delCtx, cancelDel := context.WithTimeout(ctx, 15*time.Second)
		delArgs := append(append([]string{}, baseArgs...),
			"delete", "svc", svc.name, "-n", svc.namespace, "--wait=false", "--ignore-not-found")
		if err := exec.CommandContext(delCtx, "kubectl", delArgs...).Run(); err != nil {
			fmt.Printf("[teardown] ⚠️  Failed to delete Service %s/%s: %v\n", svc.namespace, svc.name, err)
		}
		cancelDel()
	}

	// The service controller holds a load-balancer-cleanup finalizer until the CCM
	// confirms the cloud resource is gone, so the Service disappearing IS the
	// deprovision completing. Bounded, because a broken or already-dead CCM never
	// clears it and teardown must not hang on that — the IP sweep is the fallback.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		remaining := 0
		for _, svc := range services {
			checkCtx, cancelCheck := context.WithTimeout(ctx, 10*time.Second)
			getArgs := append(append([]string{}, baseArgs...),
				"get", "svc", svc.name, "-n", svc.namespace, "--ignore-not-found",
				"-o", "jsonpath={.metadata.name}")
			gotOut, gotErr := exec.CommandContext(checkCtx, "kubectl", getArgs...).Output()
			cancelCheck()
			if gotErr == nil && len(strings.TrimSpace(string(gotOut))) > 0 {
				remaining++
			}
		}
		if remaining == 0 {
			fmt.Println("[teardown] ✓ CCM deprovisioned all load balancers")
			return ips
		}
		time.Sleep(5 * time.Second)
	}

	fmt.Printf("[teardown] ⚠️  CCM did not finish within 90s; %d load balancer IP(s) will be reaped directly\n", len(ips))
	return ips
}

// stripKubernetesFinalizers attempts fast non-blocking removal of finalizers on CAPI CRs
func (o *Orchestrator) stripKubernetesFinalizers(ctx context.Context) {
	baseArgs, ok := o.resolveKubectlBaseArgs()
	if !ok {
		return
	}

	fmt.Println("\n[teardown] Attempting fast Kubernetes finalizer stripping (timeout: 5s)...")
	kctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	namespaces := []string{constants.NamespaceCAPI, "hub-platform-capi", "platform-capi", "default"}
	resourceTypes := []string{"cluster", "hetznercluster", "kubeadmcontrolplane", "machinedeployment", "machine", "hcloudmachine"}

	for _, ns := range namespaces {
		for _, rType := range resourceTypes {
			getArgs := append(baseArgs, "get", rType, "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
			cmd := exec.CommandContext(kctx, "kubectl", getArgs...)
			out, err := cmd.Output()
			if err != nil || len(out) == 0 {
				continue
			}

			names := strings.Fields(string(out))
			for _, name := range names {
				if strings.Contains(name, o.ClusterName) || (rType == "cluster" && name == o.ClusterName) {
					// Patch finalizers to empty array
					patchArgs := append(baseArgs, "patch", rType, name, "-n", ns, "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
					exec.CommandContext(kctx, "kubectl", patchArgs...).Run()
					// Delete immediately without waiting
					delArgs := append(baseArgs, "delete", rType, name, "-n", ns, "--wait=false", "--grace-period=0", "--force", "--ignore-not-found")
					exec.CommandContext(kctx, "kubectl", delArgs...).Run()
				}
			}
		}
	}
}

// deleteHetznerResources deletes all matching Hetzner cloud resources directly via
// the Hetzner API.
//
// ccmLoadBalancerIPs comes from drainLoadBalancerServices and lists the public
// addresses of load balancers the CCM owned. Matching on those addresses is what
// catches a CCM load balancer at all: its name and labels carry nothing that ties
// it to this cluster, so the name/label matcher below cannot see it. The IP match
// needs no cooperation from naming conventions, which also means it covers
// LoadBalancer Services authored by a fleet, not just platform ones.
func (o *Orchestrator) deleteHetznerResources(ctx context.Context, ccmLoadBalancerIPs, spokes []string) error {
	fmt.Println("\n[teardown] Forcefully deleting Hetzner Cloud infrastructure resources...")

	// Supplied by the caller, which read it from the box's escrow or took it from
	// the environment. Not read from k8-secrets/hetzner/token: a credential on the
	// bootstrap machine is why a box could only be destroyed from there.
	hcloudToken := strings.TrimSpace(o.ProviderToken)
	if hcloudToken == "" {
		// An ERROR, not a warning. This returned nil after printing "skipping
		// Hetzner cloud resource cleanup", so a teardown with no credential
		// deleted nothing, exited 0, and reported "Teardown completed" -- while
		// every server kept running and kept billing.
		return fmt.Errorf("no provider credential, so no server can be deleted: the box's " +
			"servers are still running.\n\n" +
			"kakt reads it from the box's escrow with your own identity. If this box has " +
			"not escrowed one yet, set HCLOUD_TOKEN for this run")
	}

	client := hcloud.NewClient(hcloud.WithToken(hcloudToken))

	// THE PROVIDER IS ASKED FIRST, and what it says is shown before anything is
	// destroyed.
	//
	// Three things come out of this that nothing else can establish: whether the
	// infrastructure is there at all, which resources are positively owned by the
	// clusters in scope, and which are excluded and why. A missing kubeconfig
	// proves none of them -- reading it as "already gone" is what let a teardown
	// destroy nothing and report success.
	inv, invErr := o.discoverInventory(ctx, client, append([]string{o.ClusterName}, spokes...))
	if invErr != nil {
		// Uncertainty refuses. An inventory that could not be read is not an empty
		// one, and the difference is a box whose servers keep running.
		return fmt.Errorf("could not read the provider inventory, so nothing can be "+
			"established about this box: %w\n\n"+
			"Nothing was deleted. Check the credential and the provider's availability, "+
			"then re-run", invErr)
	}
	inv.Management = o.ClusterName
	inv.Workload = spokes
	state := Classify(inv, !o.InfrastructureOnly)
	inv.Render(os.Stdout, state, o.Outstanding)

	if state == StateAbsent {
		// Idempotent, and established rather than assumed: the inventory was read
		// and this box owns nothing in it. A repeated teardown lands here.
		fmt.Printf("\n[teardown] %s owns no provider resources; nothing to delete.\n", o.ClusterName)
		return nil
	}

	// Every cluster this box owns, matched the same way.
	//
	// The hub and its spokes are named differently -- acme-hub against
	// spoke-pool-hybrid-dev-01 -- but CAPH marks them identically, with the
	// server name carrying the cluster name and a caph-cluster-<cluster> label
	// declaring ownership. One convention, applied to the whole set, so a spoke
	// is not a special case that someone has to remember.
	owned := append([]string{o.ClusterName}, spokes...)

	// OWNERSHIP IS A LABEL, AND ONLY A LABEL.
	//
	// CAPH marks every resource it provisions with caph-cluster-<cluster>, so that
	// key naming a cluster in the target set is a statement of ownership by the
	// thing that created it. An exact key, not a prefix of one: caph-cluster-x
	// must not match a resource belonging to cluster "x-2".
	//
	// WHAT THIS REPLACES, because it could have destroyed the wrong cluster. The
	// previous matcher returned true on any of:
	//
	//	strings.HasPrefix(name, tenantPrefix+"-")   // every cluster in the box
	//	strings.Contains(name, cluster)             // a name containing another name
	//	strings.Contains(k, cluster)                // a label KEY containing it
	//	strings.Contains(v, cluster)                // a label VALUE containing it
	//
	// On this platform a box's clusters share a prefix by construction (ADR-082:
	// nutgraf.in gives nutgraf-hub and nutgraf-01), so `--tenant nutgraf` made a
	// HUB teardown delete the workload cluster's servers as well -- and nothing in
	// the output distinguished the two. Substring matching on a label value is
	// worse: any resource whose metadata happens to mention the name was fair game.
	//
	// A resource CAPH did not label cannot be attributed this way, and is reported
	// rather than deleted. The two exceptions are evidence of the same strength
	// from elsewhere: a load balancer the CCM told us about by IP, and a volume
	// still attached to a server this run has just deleted.
	// Attribution lives in inventory.go, so the rule the operator was shown is the
	// rule that deletes: `caph-cluster-<cluster>: owned` for a cluster in the
	// target set. `shared` never authorizes deletion -- CAPH uses it for resources
	// other clusters also use (api/v1beta1/tags.go at v1.0.7) -- and an unexpected
	// value is unknown rather than permitted.
	ownedByTarget := func(labels map[string]string) bool {
		got := Attribute([]Resource{{Labels: labels}}, owned)
		return len(got) == 1 && got[0].Eligible
	}
	matchesCluster := func(_ string, labels map[string]string) bool {
		return ownedByTarget(labels)
	}

	// Whatever CAPH owns and this teardown did not claim. Reported, never
	// deleted -- see reportOrphanedSpokes.
	var unclaimed []string
	noteIfUnclaimed := func(name string, labels map[string]string) {
		if ownedByTarget(labels) {
			return
		}
		for k := range labels {
			if strings.HasPrefix(k, "caph-cluster-") {
				unclaimed = append(unclaimed, name)
				return
			}
		}
	}

	deletedServerIDs := make(map[int]bool)

	// 1. Delete servers
	allServers, err := client.Server.All(ctx)
	if err == nil {
		for _, server := range allServers {
			noteIfUnclaimed(server.Name, server.Labels)
			if matchesCluster(server.Name, server.Labels) {
				deletedServerIDs[server.ID] = true
				fmt.Printf("[teardown] Deleting server: %s (ID: %d)\n", server.Name, server.ID)
				if _, _, err := client.Server.DeleteWithResult(ctx, server); err != nil {
					fmt.Printf("[teardown] ⚠️  Failed to delete server %s: %v\n", server.Name, err)
				} else {
					fmt.Printf("[teardown] ✓ Deleted server: %s\n", server.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list servers: %v\n", err)
	}

	// 2. Delete load balancers
	//
	// Two matchers, because they cover disjoint sets. CAPH's control-plane LB is
	// named after the cluster and matches by name. A CCM-created LB is named
	// a<service-uid> with a single hcloud-ccm/service-uid label and matches
	// neither name nor label — it is only reachable by the IP recorded during the
	// drain. Missing that second matcher is what leaked four load balancers and
	// exhausted the project quota (ADR-046 §23).
	orphanIPs := make(map[string]bool, len(ccmLoadBalancerIPs))
	for _, ip := range ccmLoadBalancerIPs {
		if ip = strings.TrimSpace(ip); ip != "" {
			orphanIPs[ip] = true
		}
	}

	matchesDrainedIP := func(lb *hcloud.LoadBalancer) bool {
		if len(orphanIPs) == 0 || lb.PublicNet.IPv4.IP == nil {
			return false
		}
		return orphanIPs[lb.PublicNet.IPv4.IP.String()]
	}

	allLBs, err := client.LoadBalancer.All(ctx)
	if err == nil {
		for _, lb := range allLBs {
			if matchesCluster(lb.Name, lb.Labels) || matchesDrainedIP(lb) {
				fmt.Printf("[teardown] Deleting load balancer: %s (ID: %d)\n", lb.Name, lb.ID)
				if _, err := client.LoadBalancer.Delete(ctx, lb); err != nil {
					fmt.Printf("[teardown] ⚠️  Failed to delete load balancer %s: %v\n", lb.Name, err)
				} else {
					fmt.Printf("[teardown] ✓ Deleted load balancer: %s\n", lb.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list load balancers: %v\n", err)
	}

	// 3. Delete volumes
	allVolumes, err := client.Volume.All(ctx)
	if err == nil {
		for _, volume := range allVolumes {
			isAttachedToDeletedServer := volume.Server != nil && deletedServerIDs[volume.Server.ID]
			if matchesCluster(volume.Name, volume.Labels) || isAttachedToDeletedServer {
				fmt.Printf("[teardown] Deleting volume: %s (ID: %d, Size: %d GB)\n", volume.Name, volume.ID, volume.Size)
				if volume.Server != nil {
					client.Volume.Detach(ctx, volume)
				}
				if _, err := client.Volume.Delete(ctx, volume); err != nil {
					fmt.Printf("[teardown] ⚠️  Failed to delete volume %s: %v\n", volume.Name, err)
				} else {
					fmt.Printf("[teardown] ✓ Deleted volume: %s\n", volume.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list volumes: %v\n", err)
	}

	// 4. Delete Placement Groups
	allPGs, err := client.PlacementGroup.All(ctx)
	if err == nil {
		for _, pg := range allPGs {
			if matchesCluster(pg.Name, pg.Labels) {
				fmt.Printf("[teardown] Deleting placement group: %s (ID: %d)\n", pg.Name, pg.ID)
				if _, err := client.PlacementGroup.Delete(ctx, pg); err != nil {
					fmt.Printf("[teardown] ⚠️  Failed to delete placement group %s: %v\n", pg.Name, err)
				} else {
					fmt.Printf("[teardown] ✓ Deleted placement group: %s\n", pg.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list placement groups: %v\n", err)
	}

	// 5. Delete Firewalls
	allFirewalls, err := client.Firewall.All(ctx)
	if err == nil {
		for _, fw := range allFirewalls {
			if matchesCluster(fw.Name, fw.Labels) {
				fmt.Printf("[teardown] Deleting firewall: %s (ID: %d)\n", fw.Name, fw.ID)
				if _, err := client.Firewall.Delete(ctx, fw); err != nil {
					fmt.Printf("[teardown] ⚠️  Failed to delete firewall %s: %v\n", fw.Name, err)
				} else {
					fmt.Printf("[teardown] ✓ Deleted firewall: %s\n", fw.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list firewalls: %v\n", err)
	}

	// 6. Delete Floating IPs
	allFloatingIPs, err := client.FloatingIP.All(ctx)
	if err == nil {
		for _, fip := range allFloatingIPs {
			if matchesCluster(fip.Name, fip.Labels) {
				fmt.Printf("[teardown] Deleting floating IP: %s (ID: %d)\n", fip.Name, fip.ID)
				if _, err := client.FloatingIP.Delete(ctx, fip); err != nil {
					fmt.Printf("[teardown] ⚠️  Failed to delete floating IP %s: %v\n", fip.Name, err)
				} else {
					fmt.Printf("[teardown] ✓ Deleted floating IP: %s\n", fip.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list floating IPs: %v\n", err)
	}

	// 7. Delete Primary IPs
	allPrimaryIPs, err := client.PrimaryIP.All(ctx)
	if err == nil {
		for _, pip := range allPrimaryIPs {
			if matchesCluster(pip.Name, pip.Labels) {
				fmt.Printf("[teardown] Deleting primary IP: %s (ID: %d)\n", pip.Name, pip.ID)
				if _, err := client.PrimaryIP.Delete(ctx, pip); err != nil {
					fmt.Printf("[teardown] ⚠️  Failed to delete primary IP %s: %v\n", pip.Name, err)
				} else {
					fmt.Printf("[teardown] ✓ Deleted primary IP: %s\n", pip.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list primary IPs: %v\n", err)
	}

	// 8. Delete Networks (with quick retry)
	allNetworks, err := client.Network.All(ctx)
	if err == nil {
		for _, network := range allNetworks {
			if matchesCluster(network.Name, network.Labels) {
				fmt.Printf("[teardown] Deleting network: %s (ID: %d)\n", network.Name, network.ID)
				if _, err := client.Network.Delete(ctx, network); err != nil {
					time.Sleep(2 * time.Second)
					if _, err := client.Network.Delete(ctx, network); err != nil {
						fmt.Printf("[teardown] ⚠️  Failed to delete network %s: %v\n", network.Name, err)
					} else {
						fmt.Printf("[teardown] ✓ Deleted network: %s (on retry)\n", network.Name)
					}
				} else {
					fmt.Printf("[teardown] ✓ Deleted network: %s\n", network.Name)
				}
			}
		}
	} else if o.Debug {
		fmt.Printf("[DEBUG] Failed to list networks: %v\n", err)
	}

	fmt.Println("[teardown] ✓ Hetzner Cloud resource cleanup completed")

	// Said last, so it is the final thing on screen rather than scrolled away by
	// the deletions above.
	reportOrphanedSpokes(unclaimed)
	return nil
}

// roots are the directories this box's local artifacts can be under: the working
// directory Day-0 wrote them relative to, and the repository it was pointed at.
//
// Both, because the two halves of the platform disagree about which one is
// authoritative and each is right about its own file: the bootstrap orchestrator
// writes the kubeconfig relative to the working directory, while the state package
// writes a cluster's state file under the repository root it is handed. A teardown
// that assumed either answer left the other file behind.
func (o *Orchestrator) roots() []string {
	roots := []string{"."}
	if o.GitopsDir != "" && o.GitopsDir != "." {
		roots = append(roots, o.GitopsDir)
	}
	return roots
}

// localArtifacts are this cluster's kubeconfigs, at every root.
func (o *Orchestrator) localArtifacts() []string {
	var paths []string
	for _, root := range o.roots() {
		paths = append(paths,
			filepath.Join(root, "k8-secrets", "kubeconfig", o.ClusterName+".kubeconfig"),
			filepath.Join(root, o.ClusterName+".kubeconfig"),
		)
	}
	return paths
}

// stateArtifacts are the state files that must not outlive the cluster, at every
// root.
//
// A state file left behind is not inert: it says a cluster exists, and the next
// bootstrap resumes against it rather than starting -- so phases are skipped for
// infrastructure that is no longer there, and the failure surfaces much later as
// a missing dependency rather than as a stale file.
//
// The tenant file's path comes from state.TenantStatePath, the function that
// wrote it, so the two cannot be spelled differently. The rest are listed in both
// layouts: state moved from .zero-ops to .state, and teardown must clean whichever
// a box actually has.
func (o *Orchestrator) stateArtifacts() []string {
	var paths []string
	for _, root := range o.roots() {
		paths = append(paths,
			state.TenantStatePath(root, o.ClusterName),
			filepath.Join(root, ".state", "bootstrap-mgmt.json"),
			filepath.Join(root, ".state", "bootstrap-workload.json"),
			// The name those two replaced. Still removed, because teardown deleting
			// a stale file is cleanup rather than acting on it.
			filepath.Join(root, ".state", "bootstrap-state.json"),
			filepath.Join(root, ".state", "infisical-bootstrap.json"),
			filepath.Join(root, ".state", "kind", "kind-config-generated.yaml"),

			// The layout these replaced.
			filepath.Join(root, ".zero-ops", "state", o.ClusterName+".json"),
			filepath.Join(root, ".zero-ops", "bootstrap-state.json"),
			filepath.Join(root, ".zero-ops", "infisical-bootstrap.json"),
			filepath.Join(root, ".zero-ops", "kind", "kind-config-generated.yaml"),
		)
	}
	return paths
}

// localCleanup deletes Kind cluster, prunes Docker artifacts, and removes local state/kubeconfig files.
func (o *Orchestrator) localCleanup(ctx context.Context) error {
	fmt.Println("\n[teardown] Cleaning up local files and Docker/Kind resources...")

	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Delete kind cluster
	fmt.Printf("[teardown] Deleting kind cluster: %s...\n", o.ClusterName)
	cmd := exec.CommandContext(cmdCtx, "kind", "delete", "cluster", "--name", o.ClusterName)
	if o.Debug {
		fmt.Printf("[DEBUG] Running: kind delete cluster --name %s\n", o.ClusterName)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		if o.Debug {
			fmt.Printf("[DEBUG] kind delete output: %s\n", string(output))
			fmt.Printf("[DEBUG] kind delete error: %v\n", err)
		}
		if !strings.Contains(string(output), "not found") {
			fmt.Printf("[teardown] ⚠️  Kind cluster deletion note: %v\n", err)
		}
	} else {
		fmt.Printf("[teardown] ✓ Deleted kind cluster: %s\n", o.ClusterName)
	}

	// Prune Docker artifacts left by kind + local-path-storage PVCs
	fmt.Println("[teardown] Pruning Docker volumes, containers, and networks...")
	exec.CommandContext(cmdCtx, "docker", "container", "prune", "-f").Run()
	if out, err := exec.CommandContext(cmdCtx, "docker", "volume", "prune", "-f").CombinedOutput(); err != nil {
		if o.Debug {
			fmt.Printf("[DEBUG] Docker volume prune: %v\n%s\n", err, out)
		}
	} else {
		fmt.Println("[teardown] ✓ Docker volumes pruned")
	}
	exec.CommandContext(cmdCtx, "docker", "builder", "prune", "-f").Run()
	exec.CommandContext(cmdCtx, "docker", "network", "rm", "kind").Run()

	// Remove kubeconfigs, AT BOTH ROOTS.
	//
	// Day-0 writes k8-secrets/kubeconfig/<cluster>.kubeconfig relative to the
	// working directory it runs in, and resolveKubectlBaseArgs above is willing to
	// read it under --gitops-dir as well. Teardown removed only the working
	// directory's, so a teardown run from anywhere but the repository left the
	// admin kubeconfig of a destroyed cluster on disk -- still named for a cluster
	// whose API server is gone, and still the first thing a later command picks up.
	//
	// os.Remove on an absent path reports nothing here, so naming both roots costs
	// nothing on a box where they are the same directory, which is the normal case.
	for _, p := range o.localArtifacts() {
		if err := os.Remove(p); err == nil {
			fmt.Printf("[teardown] ✓ Removed %s\n", p)
		}
	}

	// Remove state files, in both layouts.
	//
	// State moved from .zero-ops to .state, and teardown must clean whichever a
	// box actually has. Missing one leaves a state file behind that says a
	// cluster exists after it has been destroyed, and the next bootstrap resumes
	// against it instead of starting -- the failure is a phase being skipped for
	// infrastructure that is no longer there.
	//
	// os.Remove on an absent path is reported as nothing by the loop below, so
	// listing both costs nothing on a box that has only one.
	for _, p := range o.stateArtifacts() {
		if err := os.Remove(p); err == nil {
			fmt.Printf("[teardown] ✓ Removed %s\n", p)
		}
	}

	// Clean up kubectl config contexts & clusters
	contextsToDelete := []string{
		fmt.Sprintf("kind-%s", o.ClusterName),
		o.ClusterName,
		fmt.Sprintf("admin@%s", o.ClusterName),
	}
	for _, ctxName := range contextsToDelete {
		exec.CommandContext(cmdCtx, "kubectl", "config", "delete-context", ctxName).Run()
		exec.CommandContext(cmdCtx, "kubectl", "config", "delete-cluster", ctxName).Run()
		exec.CommandContext(cmdCtx, "kubectl", "config", "unset", fmt.Sprintf("users.%s", ctxName)).Run()
	}

	fmt.Println("[teardown] ✓ Local cleanup complete")
	return nil
}
