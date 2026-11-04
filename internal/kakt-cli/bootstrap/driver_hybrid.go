package bootstrap

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/capi"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/preflight"
)

// HybridDriver implements CloudDriver for the hybrid provider cell
// (Hetzner control plane + home-lab Flatcar workers over Tailscale).
// The Hetzner driver handles the cloud control-plane and burst pool;
// hybrid-specific behavior (Tailscale-only networking, home worker
// integration) is layered on top here.
//
// ADR-046: The hybrid cell is a platform/provider cell, not a new CAPI
// infrastructure provider. Hetzner CAPI remains the infrastructure provider.
// Key differences from the hetzner driver:
//   - LoadBalancer is disabled (controlPlaneLoadBalancer.enabled = false);
//     the spoke API endpoint is a Tailscale MagicDNS name.
//   - ClusterClassPaths returns nothing — hybrid hub bootstrap doesn't
//     install a separate hub ClusterClass; the spoke ClusterClass lives in
//     the Crossplane composition (manifests/providers/hybrid/).
//   - CCM addon is read from hetzner/base/spoke-addons/ (same shared base).
type HybridDriver struct {
	// Driver provides Hetzner CAPI infra (CP + burst pool).
	Driver *HetznerDriver

	// TailnetName is the Tailscale tailnet (e.g. "example.ts.net").
	// Used to construct the spoke controlPlaneEndpoint MagicDNS name.
	TailnetName string

	// OnPremEnabled controls whether the home-worker join flow is
	// activated in the hub-operator (reconcileHomeWorkerJoin).
	OnPremEnabled bool

	// HomeWorkerTTL is the bootstrap-token TTL for home worker join
	// credentials (e.g. "24h"). Used by hub-operator token minting.
	HomeWorkerTTL string

	// ClusterName names the control plane on the tailnet (<name>-cp).
	ClusterName string
}

// ── CloudDriver interface ──────────────────────────────────────────────────

func (d *HybridDriver) Name() string { return "hybrid" }

// CAPIPlacement keeps this cell's CAPI controllers off the tenant's premises.
//
// Returns the shared rule in placement.go, which mirrors
// environment-manager.cloudControlPlacement in _placement.tpl. One rule, two
// expressions, because CAPI is installed before the chart that would render it.
//
// This method used to return {workload-location: on-prem} and pin the
// controllers TO the on-prem node. That was measured and correct at the time:
// the cpx22 control plane sat at 89% memory with etcd, the apiserver and six
// CAPI controllers on it, and every controller was restarting on "leader
// election lost" -- 85, 85, 73, 67 restarts -- because it could not renew a
// lease on a saturated node.
//
// It is reversed because the reason expired, not because it was wrong. The
// control plane's leader election was fixed at its source by raising the lease
// timeouts in the ClusterClass (lease-duration 60s, renew-deadline 40s,
// retry-period 10s; verified zero restarts over 79 minutes), so saturation no
// longer decides the placement. What decides it now is the network: an on-prem
// node reaches the API server over VXLAN-over-Tailscale, and CAPH was losing its
// lease across that link instead.
//
// Nothing for pure Hetzner, still. That hub's nodes are all cloud, so the rule
// is a no-op there and the driver need not implement it.
func (d *HybridDriver) CAPIPlacement() string {
	return cloudControlPlacementPatch
}

// PlannedWorkerReplicas is zero for every hybrid box regardless of environment:
// the cell exists so worker capacity comes from the tenant's own hardware.
func (d *HybridDriver) PlannedWorkerReplicas() int { return 0 }

// CiliumAddonPath is this cell's own artifact. It carries the standalone
// cilium-envoy DaemonSet (ADR-046 addendum 10) and the hostNetwork mangle guard
// (addenda 8 and 27), which its config base requires and the shared template does
// not ship.
func (d *HybridDriver) CiliumAddonPath() string {
	return "manifests/providers/hybrid/k8s/cilium-addon-hybrid.yaml"
}

// OnPremRequested reports whether this cell expects home-lab worker nodes.
// The orchestrator's on-prem-join phase keys off this: only a hybrid cell that
// asked for home workers has one to wait for. Hetzner cells do not implement it.
func (d *HybridDriver) OnPremRequested() bool { return d.OnPremEnabled }
func (d *HybridDriver) OSType() string        { return d.Driver.OSType() }

// StageTailscaleCredentials is forwarded, and must be.
//
// HybridDriver holds its Hetzner driver as a NAMED field, not an embedded one,
// so nothing is promoted: a method that is not written here does not exist on
// this type. PivotReady reaches the stager by type assertion, and an assertion
// against a type missing the method does not fail loudly -- it simply does not
// match, and the caller returns nil having done nothing.
//
// That is what happened. pivot-ready re-ran, reported success in 15 seconds, and
// staged no Secret at all; the hub-operator then logged "Source secret not found,
// skipping" and the tailscale-hybrid-psk ExternalSecret stayed in
// SecretSyncedError. The one driver that needs this method is the one that did
// not have it -- a hetzner box has no tailnet to stage.
//
// d.OnPremEnabled is the hybrid driver's own field, and the Hetzner driver reads
// its own copy, so it is passed through the same way OnCAPIInit does.
func (d *HybridDriver) StageTailscaleCredentials(ctx context.Context, kubeconfig, namespace string) error {
	d.Driver.OnPremEnabled = d.OnPremEnabled
	return d.Driver.StageTailscaleCredentials(ctx, kubeconfig, namespace)
}

// ── Phase 1: Preflight ──────────────────────────────────────────────────────
// Reuse Hetzner preflight (token, region, SSH key). Tailscale connectivity
// is checked by the Flatcar worker provisioning scripts, not here.

func (d *HybridDriver) PreflightValidators() []preflight.Validator {
	return d.Driver.PreflightValidators()
}

// ── Phase 3: Day-0 Infrastructure ───────────────────────────────────────────
// Hybrid Day-0 = Hetzner Day-0 (Hetzner secret + CSI for the Hub).
// Home-worker join-config Secret is written by hub-operator after CAPI
// provisioning (WS4: reconcileHomeWorkerJoin), not here.

func (d *HybridDriver) ProvisionDayZero(ctx context.Context, kubeconfig string) error {
	return d.Driver.ProvisionDayZero(ctx, kubeconfig)
}

// ── Phase 4: CAPI Init ──────────────────────────────────────────────────────

func (d *HybridDriver) CAPIProviders() []capi.CAPIProvider {
	return d.Driver.CAPIProviders()
}

func (d *HybridDriver) OnCAPIInit(ctx context.Context, kubeconfig, kubeContext, namespace string) error {
	// The embedded driver stages the tailnet credentials, guarded on its own
	// OnPremEnabled (ADR-046 invariant 6, ADR-075). This used to be duplicated
	// here, which is how the hetzner path came to lack it entirely.
	return d.Driver.OnCAPIInit(ctx, kubeconfig, kubeContext, namespace)
}

// readTailscaleAuthkey loads the tailnet auth key.
//
// The environment comes first, the on-disk path second. That order is the whole
// point: the file is `k8-secrets/`, which is the platform operator's own
// gitignored directory on their own laptop. Day-0 now runs in the tenant's CI
// under the tenant's secrets (ADR-072), where that directory does not exist and
// never will -- so a tenant bootstrapping a hybrid box got the warning below,
// built a control plane that never joined the tailnet, and discovered it when
// pod traffic to their home workers died in one direction (ADR-046 invariant 6).
//
// The file is retained because the home-worker provisioning script reads the same
// path, so an operator running both halves from one machine still enrols both
// sides of the tailnet from one credential.
func readTailscaleAuthkey() (string, error) {
	if key := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); key != "" {
		return key, nil
	}
	if key := strings.TrimSpace(os.Getenv("TAILSCALE_AUTHKEY")); key != "" {
		return key, nil
	}
	const path = "k8-secrets/tailscale/authkey"
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no tailnet auth key: TS_AUTHKEY is unset and %s is not readable: %w", path, err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("tailscale authkey at %s is empty", path)
	}
	return key, nil
}

func (d *HybridDriver) OnPlatformPreReqs(ctx context.Context, kubeconfig string) error {
	return d.Driver.OnPlatformPreReqs(ctx, kubeconfig)
}

// ── Phase 5: Cluster Provisioning Config ────────────────────────────────────
// Hybrid differs from Hetzner in two ways:
//  1. CCM addon is read from hetzner/base/ (same path as hetzner after WS1).
//  2. Hub mgmt cluster mirrors hetzner (2 workers) — the burst-pool replicas:0
//     for SPOKES lives in the hybrid Crossplane composition, not here.

func (d *HybridDriver) PopulateClusterConfig(cfg *cluster.Config) {
	// Start from Hetzner defaults (region, OS, image, machine types, K8s version).
	d.Driver.PopulateClusterConfig(cfg)

	// ADR-046: the hybrid cell exists so that worker capacity comes from home-lab
	// hardware — "Costs nothing to keep running (no idle Hetzner worker nodes)".
	// The Hetzner driver provisions 2 cx33 workers for the hub, which is right for
	// a pure-Hetzner hub but is exactly the idle capacity this provider avoids.
	//
	// The hub's workers are the Flatcar home-lab nodes: provision-flatcar-worker.sh
	// joins them with hub-role=worker + node-role.kubernetes.io/worker +
	// workload-location=on-prem when its target cluster is "hub" (see home-lab.env,
	// where flatcar-hub-node-1 is registered against the hub).
	//
	// Overriding here rather than in HetznerDriver keeps the tested pure-Hetzner
	// path (WorkerReplicas = 2) untouched — this applies only when --provider=hybrid.
	//
	// Keyed on the PROVIDER, not the environment: choosing --provider=hybrid is
	// itself the statement that this cell's worker capacity comes from home-lab
	// hardware. It therefore applies to every hybrid environment, and topology is
	// not consulted — hybrid's own shape is the default when none is passed.
	// --provider=hetzner is untouched and keeps 2 Hetzner workers.
	//
	// Zero Hetzner workers means SOMETHING else has to be able to run the platform,
	// or every workload sits Pending and the bootstrap hangs.
	//
	// With --on-prem the home-lab nodes are that something: the
	// on-prem-join phase brings one up before boundary-01, and the control
	// plane keeps its taint exactly as ADR-046 §11 / ADR-014 require ("every
	// stateful workload runs on worker nodes only, control-plane nodes keep
	// control-plane:NoSchedule"). Untainting it here would re-create the failure
	// §11 was written to ban — CNPG landing on the control plane because storage
	// happened to bind there.
	//
	// Without home workers there is no other node at all, so the control plane has
	// to carry the platform and is registered without the taint.
	cfg.WorkerReplicas = 0
	cfg.ControlPlaneSchedulable = !d.OnPremEnabled

	// The cilium operator declares hostPorts, so its two replicas cannot share one
	// node; on a single-node hub the second would sit Pending forever and register
	// as a permanently unhealthy pod. Scaled here rather than in the shared addon
	// manifest, which the multi-node Hetzner hub also consumes.
	cfg.CiliumOperatorReplicas = 1

	// Re-read CCM from hetzner/base/ — driver_hetzner already reads from hetzner/base/
	// after WS1 fix, so this is a no-op path correction guard. Explicit for clarity.
	ccmRaw, err := readTemplateManifest(
		"manifests/providers/hetzner/base/spoke-addons/", "ccm-addon-template.yaml", "ccm.yaml")
	if err != nil {
		fmt.Printf("[cluster-provision] Warning: failed to read CCM manifest (hybrid): %v\n", err)
	} else {
		cfg.CCMManifest = string(ccmRaw)
	}

	// Wire home-worker config into cluster.Config (read by hub-operator: WS4).
	cfg.HomeWorker = cluster.HomeWorkerConfig{
		Enabled:     d.OnPremEnabled,
		TTL:         d.HomeWorkerTTL,
		TailnetName: d.TailnetName,
	}
}

// ── Phase 9: ClusterClass paths ────────────────────────────────────────────
// For the hybrid hub bootstrap, no extra hub ClusterClass is needed.
// The spoke ClusterClass (spokepool-v1) lives in hetzner/base/ and is deployed
// by the Crossplane composition (manifests/providers/hybrid/). Return empty
// so the orchestrator skips the ClusterClass apply step for hybrid.

func (d *HybridDriver) ClusterClassPaths() []string {
	return []string{}
}

// ── Capabilities ────────────────────────────────────────────────────────────
// Hybrid: the spoke API IS fronted by a cloud LoadBalancer — the CAPH-managed
// Hetzner LB (controlPlaneLoadBalancer.enabled=true, ADR-046 "Spoke API
// Endpoint"). The composition sets controlPlaneLoadBalancerEnabled: true and
// the same LB now also carries tenant ingress on 80/443 via extraServices.
// The earlier "Tailscale-only" comment described a pre-ADR-046 design that the
// spoke-api-front removal (addendum 5) already retired.
// StorageClass comes from Hetzner CSI (same as hetzner provider).

func (d *HybridDriver) OperatorWebhookPatterns() []string {
	return d.Driver.OperatorWebhookPatterns()
}

func (d *HybridDriver) Capabilities() CapabilityContract {
	base := d.Driver.Capabilities()
	// CAPH provisions the spoke control-plane LB (and, via extraServices, the
	// tenant ingress ports). Keep the inherited LoadBalancer capability.
	return base
}
