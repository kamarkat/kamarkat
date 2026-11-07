package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/platform"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/binaries"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/capi"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/config"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/pivot"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/preflight"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// ──────────────────────────────────────────────────────────────────────────────
// CloudProvider — single implementation for ALL cloud providers (ADR-036 §1)
// ──────────────────────────────────────────────────────────────────────────────
//
// CloudProvider implements the full 12-phase Provider pipeline for any cloud
// (Hetzner, AWS, GCP, etc.). Cloud-specific details are delegated to a
// CloudDriver implementation. Adding a new cloud means implementing CloudDriver
// — the CloudProvider itself never changes.
//
// Phase mapping:
//   5 (provision):  creates VMs via CAPI, waits for cluster ready
//   6 (pivot):      retrieves mgmt kubeconfig, clusterctl move
//   7 (pivot-ready): waits for CAPI reconciliation post-pivot
//   12 (finalize):  extracts kubeconfig from CAPI secret

type CloudProvider struct {
	driver      CloudDriver
	clusterName string
	debug       bool
	encryption  cluster.EncryptionMode
}

func NewCloudProvider(driver CloudDriver, clusterName string, debug bool) *CloudProvider {
	return &CloudProvider{
		driver:      driver,
		clusterName: clusterName,
		debug:       debug,
	}
}

func (p *CloudProvider) SetEncryption(mode cluster.EncryptionMode) {
	p.encryption = mode
}

func (p *CloudProvider) Encryption() cluster.EncryptionMode {
	return p.encryption
}

// ── Identity ────────────────────────────────────────────────────────────────

func (p *CloudProvider) Name() string { return p.driver.Name() }

// PlannedWorkerReplicas is how many cloud workers this box will start with, read
// by the pre-flight capacity check before anything is provisioned.
func (p *CloudProvider) PlannedWorkerReplicas() int { return p.driver.PlannedWorkerReplicas() }
func (p *CloudProvider) KindConfigPath() string {
	// Cloud providers use default kind config (no custom config needed).
	return ""
}

// ── Phase 1: Preflight ──────────────────────────────────────────────────────

func (p *CloudProvider) PreflightValidators() []preflight.Validator {
	return p.driver.PreflightValidators()
}

// ── Phase 3: Day-0 Infrastructure ───────────────────────────────────────────

func (p *CloudProvider) ProvisionDayZero(ctx context.Context, kubeconfig string) error {
	return p.driver.ProvisionDayZero(ctx, kubeconfig)
}

// ── Phase 4: CAPI Init ──────────────────────────────────────────────────────

func (p *CloudProvider) CAPIProviders() []capi.CAPIProvider {
	return p.driver.CAPIProviders()
}

func (p *CloudProvider) OnCAPIInit(ctx context.Context, kubeconfig, kubeContext, namespace string) error {
	return p.driver.OnCAPIInit(ctx, kubeconfig, kubeContext, namespace)
}

// ── Phase 5: Management Cluster Provisioning ────────────────────────────────

func (p *CloudProvider) ProvisionManagementCluster(ctx context.Context, cfg *ProvisionConfig) error {
	fmt.Println("\n[cluster-provision] Provisioning Management Cluster...")

	// Build cluster.Config from driver (cloud-specific fields) + shared fields
	clusterCfg := &cluster.Config{}
	p.driver.PopulateClusterConfig(clusterCfg)

	// Fill shared fields known to CloudProvider
	clusterCfg.ClusterName = p.clusterName
	clusterCfg.Namespace = constants.NamespaceCAPI
	clusterCfg.GitopsDir = cfg.GitopsDir
	// Shared, not driver-specific: which issuer a box trusts is a property of the
	// box, not of the cloud underneath it.
	clusterCfg.OIDCIssuerURL = cfg.OIDCIssuerURL
	clusterCfg.OIDCClientID = cfg.OIDCClientID
	// At-rest encryption is a property of the box, not of the cloud underneath it:
	// a Hetzner box and a hybrid box encrypt etcd the same way.
	clusterCfg.Encryption = cfg.Encryption
	p.encryption = cfg.Encryption

	// THE BOX ID, which nothing set.
	//
	// cluster.Config declared it, documented it as "what the escrow keys
	// box-scoped artefacts by", and the provisioner reads it on both sides of the
	// at-rest encryption key -- but no code ever assigned it. The escrow validates
	// the identity and refuses an empty box id, and Provision calls
	// applyEncryptionConfig unconditionally before the ClusterClass, so every
	// bootstrap failed there: "the escrow needs the box id". Nothing keyed by the
	// box has ever been escrowed by a bootstrap, which is why a box can reach this
	// point with no escrowed kubeconfig and no escrowed credential.
	//
	// Read from the box's own declaration (ADR-111 §1), never derived from the
	// cluster name: a successor hub carries a new cluster name under the same box,
	// and deriving it would send that successor's artefacts to a path nothing reads.
	boxID, err := hubaccess.DeclaredBoxID(cfg.GitopsDir, p.clusterName)
	if err != nil {
		return fmt.Errorf("the box's identity is required before anything is escrowed: %w", err)
	}
	clusterCfg.BoxID = boxID

	// Read the shared cilium addon manifest and recompose it with the config half.
	//
	// ADR-046 §24 split the cilium-config ConfigMap out of the addon Secret so that
	// exactly one component renders the copy a SPOKE receives (hub-operator, which
	// injects the spoke's control-plane endpoint). The HUB has no SpokePool and no
	// renderer, so this is where the hub's copy is composed: the provider's static
	// base, retargeted below and joined back onto the addon here.
	//
	// The base carries no endpoint, for the hub either. §24.1 exempted the hub on the
	// grounds that it "keeps kube-proxy", which it does not: the base it installs sets
	// kube-proxy-replacement true and no kube-proxy DaemonSet runs on the hub, so its
	// agents resolved the API server through 10.96.0.1 and deadlocked permanently when
	// that endpoint was withdrawn (2026-10-28). §24.6 withdrew the exemption. The two
	// endpoint keys are written by the provisioner, once the infrastructure provider
	// has set Cluster.spec.controlPlaneEndpoint — see injectAPIServerEndpoint.
	//
	// Reading the same base file both halves use is deliberate: a second copy of 160
	// cilium settings would drift, and drift in this file is a cluster that boots
	// with a datapath nobody intended.
	//
	// That reasoning applies to the WORKLOAD half too, and it did not used to. The
	// hub took its agent and operator from spoke-bootstrap/cilium-addon-template.yaml
	// while its settings came from the provider base, and the two disagreed: the base
	// declares external-envoy-proxy true (ADR-046 addendum 10 decoupled Envoy), but
	// that template ships no cilium-envoy at all. The hub therefore ran with an agent
	// that delegates L7 to an Envoy nobody deployed. Nothing surfaced until the hub
	// served a Gateway, at which point the agent could not push the listener
	// configuration anywhere ("failed to upsert envoy resources: context deadline
	// exceeded") and the Gateway reported Programmed with no dataplane behind it.
	//
	// The provider's rendered addon is the one artifact that carries the workloads and
	// the settings in agreement, including the pieces hostNetwork Gateway mode needs:
	// the standalone cilium-envoy DaemonSet (addendum 10) and the mangle guard that
	// keeps the host-bound listeners reachable (addendum 8, addendum 27). A hub that
	// binds :80/:443 through Envoy needs both for the same reasons a spoke does.
	addonPath := p.driver.CiliumAddonPath()
	ciliumRaw, err := readTemplateManifest(
		filepath.Dir(addonPath)+string(filepath.Separator), filepath.Base(addonPath), "cilium.yaml")
	if err != nil {
		return fmt.Errorf("failed to read cilium addon for provider %s at %s: %w",
			p.driver.Name(), addonPath, err)
	}
	//
	// Two things the base is NOT, and both were wrong here:
	//
	//   1. It is not provider-neutral. The path was hardcoded to hetzner, so a hybrid
	//      hub was built from the hetzner datapath: mtu 1230 instead of 1200, no
	//      `tailscale0` in `devices`, and gateway-api-hostnetwork-enabled false. A
	//      hybrid hub reaches its home workers over the tailnet and binds its Gateway
	//      on the control-plane host, so none of those are cosmetic.
	//
	//   2. It is not the delivered artifact. It is the RENDERER's input —
	//      ConfigMap/cilium-config-base-<provider> in platform-capi, read by
	//      hub-operator to render a spoke's copy. Cilium consumes
	//      ConfigMap/cilium-config in kube-system. Appending the file verbatim shipped
	//      a document targeting platform-capi, a namespace that does not exist on a
	//      workload cluster, so it never applied: cilium-operator hung in
	//      ContainerCreating on `configmap "cilium-config" not found`, the agent
	//      CrashLoopBackOff'd, and every node stayed NotReady with no CNI.
	//
	// Read the provider's own base and retarget it to what Cilium actually consumes.
	ciliumConfigPath := filepath.Join(
		"manifests", "providers", p.driver.Name(), "k8s", "cilium-config-base.yaml")
	ciliumConfig, err := platform.ReadFile(ciliumConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read cilium-config base (ADR-046 §24) at %s: %w", ciliumConfigPath, err)
	}
	var ciliumCM map[string]interface{}
	if err := yaml.Unmarshal(ciliumConfig, &ciliumCM); err != nil {
		return fmt.Errorf("failed to parse cilium-config base at %s: %w", ciliumConfigPath, err)
	}
	ciliumMeta, ok := ciliumCM["metadata"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("cilium-config base at %s has no metadata block", ciliumConfigPath)
	}
	ciliumMeta["name"] = "cilium-config"
	ciliumMeta["namespace"] = constants.NamespaceKubeSystem
	// Renderer-selection label; it selects a base on the hub and means nothing here.
	delete(ciliumMeta, "labels")
	ciliumConfigOut, err := yaml.Marshal(ciliumCM)
	if err != nil {
		return fmt.Errorf("failed to render cilium-config for delivery: %w", err)
	}
	// Gateway API CRDs travel WITH Cilium, ahead of it in the document stream.
	//
	// Cilium here runs with enable-gateway-api=true, and its operator blocks until
	// these CRDs exist. Nothing on the cluster gets past that: the operator never
	// settles, the home-lab worker never goes Ready, and the worker is the hub's
	// only worker capacity (ADR-046 §19/§21). Delivering the CRDs through ArgoCD
	// is circular — ArgoCD needs the worker that needs the CRDs that ArgoCD would
	// install — which is why they belong here, in the same ClusterResourceSet
	// addon that already carries the CNI (ADR-041).
	//
	// Ordered first deliberately. Same addon, one document stream, so the CRDs
	// cannot be separated from the CNI that requires them by reordering a list.
	gatewayCRDs, err := assets.ReadManifest("addons/gateway-api-crds.yaml")
	if err != nil {
		return fmt.Errorf("failed to read the Gateway API CRDs delivered with Cilium: %w", err)
	}
	clusterCfg.CiliumManifest = string(gatewayCRDs) + "\n---\n" +
		string(ciliumRaw) + "\n---\n" + string(ciliumConfigOut)

	// The escrow, for the at-rest encryption key (ADR-003 section 6, ADR-076).
	//
	// Constructed here and not inside the provisioner so the failure lands before the
	// cluster is touched: an escrow that is absent or half-configured is a reason not
	// to start, not something to discover after a control plane exists. ADR-076
	// already requires one for a box to be built at all, so this adds no new input.
	escrowClient, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("the management cluster's at-rest encryption key needs an escrow "+
			"to be kept in, and a box is not built without one (ADR-076): %w", err)
	}

	// The provider credential, escrowed before any infrastructure exists.
	//
	// Required, not best-effort: see enrollProviderCredential. A failure here
	// fails the bootstrap, because a box whose recovery credential is not escrowed
	// can only ever be destroyed from this machine -- and the servers do not exist
	// yet, so refusing now costs nothing.
	if err := enrollProviderCredential(ctx, escrowClient, boxID, p.clusterName,
		p.driver.Name(), os.Getenv("HCLOUD_TOKEN")); err != nil {
		return fmt.Errorf("the box's recovery credentials must be escrowed before its "+
			"infrastructure is created (ADR-076): %w", err)
	}

	provisioner := &cluster.Provisioner{
		Kubeconfig:   cfg.BootstrapKubeconfig,
		Context:      cfg.BootstrapContext,
		Config:       clusterCfg,
		Debug:        p.debug || cfg.Debug,
		EscrowClient: escrowClient,
	}

	// Check if cluster already exists
	checkCmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", cfg.BootstrapKubeconfig,
		"--context", cfg.BootstrapContext,
		// Fully qualified: CNPG registers a Cluster kind too, and the short name
		// resolves to it once cloudnative-pg is installed (health.CAPIClusterKind).
		"get", "clusters.cluster.x-k8s.io", p.clusterName,
		"-n", constants.NamespaceCAPI,
		"-o", "jsonpath={.status.phase}",
	)
	if output, err := checkCmd.Output(); err == nil && string(output) == "Provisioned" {
		fmt.Println("[cluster-provision] ✓ Cluster already exists and provisioned")
	} else {
		if err := provisioner.Provision(ctx); err != nil {
			return fmt.Errorf("failed to provision cluster: %w", err)
		}
		fmt.Println("[cluster-provision] ✓ Cluster resources and CRS applied")
	}

	fmt.Println("[cluster-provision] Waiting for cluster Ready (CRS installing CNI/CCM)...")
	if err := provisioner.WaitForReady(ctx); err != nil {
		return fmt.Errorf("cluster not ready: %w", err)
	}
	fmt.Println("[cluster-provision] ✓ Management Cluster ready")
	return nil
}

// ── Phase 6: Pivot Move ─────────────────────────────────────────────────────

func (p *CloudProvider) PivotMove(ctx context.Context, cfg *PivotConfig) (string, error) {
	fmt.Println("\n[pivot] Moving CAPI resources to Management Cluster...")

	// Refresh kind kubeconfig if using kind bootstrap
	if cfg.BootstrapContext != "" && strings.HasPrefix(cfg.BootstrapContext, "kind-") {
		kindClusterName := strings.TrimPrefix(cfg.BootstrapContext, "kind-")
		cmd := exec.CommandContext(ctx, "kind", "export", "kubeconfig", "--name", kindClusterName)
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to export kind kubeconfig: %w\n%s", err, output)
		}
		if p.debug {
			fmt.Println("[DEBUG] Refreshed kubeconfig from kind")
		}
	}

	// 30 minutes, not 10.
	//
	// A worker's join time is not the control plane's. Measured on one fresh
	// Hetzner box, all three machines created within 25 seconds of each other:
	//
	//	control plane      4m
	//	worker (first)    12m
	//	worker (second)   44m
	//
	// The old budget was 10 minutes, which is shorter than even the FASTER
	// worker took -- so the phase was not marginal, it was under-provisioned,
	// and had been passing on luck. It failed here after 10m05s on a machine
	// that joined perfectly well at 44m, having already built three servers.
	//
	// A bound is still wanted: a machine that never joins should be reported,
	// not waited on for ever. 30m covers the observed spread with room, and the
	// failure names the machine and what it lacks, so a genuine non-join is
	// still diagnosable rather than a hang.
	fmt.Println("[pivot] Waiting for all nodes to join cluster...")
	if err := waitForAllMachinesRunning(ctx, cfg.BootstrapKubeconfig, cfg.BootstrapContext, 30*time.Minute); err != nil {
		return "", fmt.Errorf("machines not ready for pivot: %w", err)
	}
	if cfg.Encryption != "" {
		p.encryption = cfg.Encryption
	}

	pivotOrch := &pivot.Orchestrator{
		BootstrapKubeconfig: cfg.BootstrapKubeconfig,
		BootstrapContext:    cfg.BootstrapContext,
		ClusterName:         p.clusterName,
		Namespace:           constants.NamespaceCAPI,
		OSType:              p.driver.OSType(),
		Debug:               p.debug || cfg.Debug,
	}

	mgmtKubeconfig, err := pivotOrch.ExecuteMove(ctx)
	if err != nil {
		return "", fmt.Errorf("pivot move failed: %w", err)
	}
	fmt.Println("[pivot] ✓ Resources moved to Management Cluster")
	return mgmtKubeconfig, nil
}

// ── Phase 7: Pivot Ready ────────────────────────────────────────────────────

func (p *CloudProvider) PivotReady(ctx context.Context, mgmtKubeconfig string) error {
	fmt.Println("\n[pivot-ready] Waiting for cluster reconciliation after move...")

	pivotOrch := &pivot.Orchestrator{
		BootstrapKubeconfig: mgmtKubeconfig,
		ClusterName:         p.clusterName,
		Namespace:           constants.NamespaceCAPI,
		OSType:              p.driver.OSType(),
		Debug:               p.debug,
	}

	// Place the CAPI controllers BEFORE waiting for them to be ready.
	//
	// Applied here and not at capi-init: that phase installs the providers into
	// the KIND bootstrap cluster, where no node carries a placement label, so a
	// selector set then would leave every provider Pending and fail the install.
	// By this point the on-prem workers have joined (on-prem-join precedes
	// pivot-move) and the providers have been moved onto the hub, so the label
	// they are being sent to exists.
	//
	// Before WaitForReady rather than after, because the wait is what those
	// restarts were failing: rescheduling first means it waits on controllers
	// that have somewhere to run.
	if err := p.placeCAPIControllers(ctx, mgmtKubeconfig); err != nil {
		return err
	}

	if err := pivotOrch.WaitForReady(ctx, mgmtKubeconfig); err != nil {
		return fmt.Errorf("pivot ready failed: %w", err)
	}
	fmt.Println("[pivot-ready] ✓ Cluster ready on Management Cluster")

	// Re-stage the tailnet credential on the pivoted hub.
	//
	// It was written at capi-init, into the BOOTSTRAP cluster, because the hub's
	// own ClusterClass reads it through contentFrom.secret while the hub Cluster
	// is being created. `clusterctl move` carries CAPI-owned objects; a plain
	// Secret is not one, so it does not cross the pivot.
	//
	// Nothing noticed, because the hub was already built by then. The first
	// object to need it on the hub is the first SPOKE, whose KubeadmConfig cannot
	// render without it:
	//
	//   BootstrapReady False  failed to resolve file source: secret not found:
	//                         platform-capi/tailscale-hybrid-psk
	//
	// Every machine then sits Pending for ever -- control plane included -- so the
	// spoke has no API server, and anything watching it (the burst autoscaler)
	// fails against an endpoint that will never answer.
	//
	// Empty values are correct on a box with no tailnet: every tailscale step in
	// the ClusterClass is guarded on a non-empty hostname and becomes a no-op.
	// What matters is that the reference RESOLVES.
	if err := p.stageTailnetCredentialOnHub(ctx, mgmtKubeconfig); err != nil {
		return err
	}
	return nil
}

// placeCAPIControllers pins the CAPI provider Deployments to the nodes this
// provider says they belong on.
//
// A no-op unless the driver asks for it, which only the hybrid one does: a pure
// Hetzner hub has worker nodes already and needs no selector, and applying one
// it does not carry would strand every controller Pending.
//
// The selector goes on the provider CRs rather than on the Deployments. The
// capi-operator owns those Deployments and reconciles them, so a patch applied
// directly would be reverted the next time it looked.
func (p *CloudProvider) placeCAPIControllers(ctx context.Context, kubeconfig string) error {
	placer, ok := p.driver.(interface{ CAPIPlacement() string })
	if !ok {
		return nil
	}
	patch := placer.CAPIPlacement()
	if patch == "" {
		return nil
	}

	// Every provider kind the operator manages. Named rather than discovered so a
	// provider added later is a compile-time edit here, not a silent omission.
	//
	// Each object is patched BY NAME. `kubectl patch` has no --all: it takes a
	// resource name or -f, and passing --all fails with "unknown flag" rather
	// than patching everything, which is how this first shipped.
	for _, kind := range []string{
		"coreprovider", "bootstrapprovider", "controlplaneprovider", "infrastructureprovider",
	} {
		listed, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"get", kind, "-n", constants.NamespaceCAPI,
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}").Output()
		if err != nil {
			return fmt.Errorf("could not list %s to place its controllers: %w", kind, err)
		}
		for _, name := range strings.Fields(string(listed)) {
			out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
				"patch", kind, name, "-n", constants.NamespaceCAPI,
				"--type", "merge", "-p", patch).CombinedOutput()
			if err != nil {
				return fmt.Errorf("could not place %s/%s: %s: %w",
					kind, name, strings.TrimSpace(string(out)), err)
			}
		}
	}
	fmt.Println("[pivot-ready] ✓ CAPI controllers placed off the tenant's premises (see placement.go)")
	return nil
}

// stageTailnetCredentialOnHub asks the driver to write its tailnet credential
// into the pivoted hub, if it has one to write.
//
// Type assertion rather than a new interface method, matching OnPremRequested
// above: only the drivers that provision through a ClusterClass reading this
// secret implement it, and a driver that does not is not in error.
func (p *CloudProvider) stageTailnetCredentialOnHub(ctx context.Context, kubeconfig string) error {
	stager, ok := p.driver.(interface {
		StageTailscaleCredentials(context.Context, string, string) error
	})
	if !ok {
		return nil
	}
	if err := stager.StageTailscaleCredentials(ctx, kubeconfig, constants.NamespaceCAPI); err != nil {
		return fmt.Errorf("stage tailnet credential on the pivoted hub: %w", err)
	}
	fmt.Println("[pivot-ready] ✓ tailnet credential staged on the hub (spokes render without it otherwise)")
	return nil
}

// ── Phase 9: ClusterClass ───────────────────────────────────────────────────

// OnPremRequested forwards the driver's answer when it has one. Only the
// hybrid driver does; a Hetzner cell has no home workers and reports false.
func (p *CloudProvider) OnPremRequested() bool {
	if hw, ok := p.driver.(interface{ OnPremRequested() bool }); ok {
		return hw.OnPremRequested()
	}
	return false
}

func (p *CloudProvider) ClusterClassPaths() []string {
	return p.driver.ClusterClassPaths()
}

// ── Phase 10: Platform Pre-Requisites ───────────────────────────────────────

func (p *CloudProvider) OnPlatformPreReqs(ctx context.Context, kubeconfig string) error {
	return p.driver.OnPlatformPreReqs(ctx, kubeconfig)
}

// ── Phase 12: Finalize ──────────────────────────────────────────────────────

func (p *CloudProvider) Finalize(ctx context.Context, cfg *FinalizeConfig) (string, error) {
	configMgr := &config.Manager{
		BootstrapKubeconfig: cfg.MgmtKubeconfig,
		ClusterName:         cfg.ClusterName,
		Namespace:           cfg.Namespace,
	}

	kubeconfigPath, err := configMgr.SaveKubeconfig(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to save kubeconfig: %w", err)
	}
	fmt.Printf("[finalize] ✓ Kubeconfig saved to %s\n", kubeconfigPath)

	if cfg.MergeKubeconfig {
		if err := configMgr.MergeKubeconfig(ctx, kubeconfigPath); err != nil {
			fmt.Printf("[finalize] Warning: failed to merge kubeconfig: %v\n", err)
		} else {
			fmt.Println("[finalize] ✓ Kubeconfig merged into ~/.kube/config")
		}
	}

	return kubeconfigPath, nil
}

// ── Capability Contract ─────────────────────────────────────────────────────

func (p *CloudProvider) OperatorWebhookPatterns() []string {
	return p.driver.OperatorWebhookPatterns()
}
func (p *CloudProvider) Capabilities() CapabilityContract {
	return p.driver.Capabilities()
}

// ──────────────────────────────────────────────────────────────────────────────
// CloudDriver — cloud-specific details (ADR-036 §1)
// ──────────────────────────────────────────────────────────────────────────────
//
// CloudDriver is the pluggable extension point for multi-cloud support.
// Implementing this interface is all that's required to add a new cloud
// (AWS, GCP, Azure, etc.). The CloudProvider delegates every cloud-specific
// decision to the driver, including CAPI provider lists, ClusterClass paths,
// CSI installation, credentials secrets, and provisioning config.
//
// Each driver is self-contained: it knows its own CCM manifest paths, machine
// types, region codes, and token sources. The CloudProvider never branches on
// cloud name.

type CloudDriver interface {
	// Name returns the cloud identifier ("hetzner", "aws", "gcp").
	Name() string

	// OSType returns the node operating system for CAPI provisioning
	// ("ubuntu" is the only supported value, ADR-001).
	OSType() string

	// PlannedWorkerReplicas is how many cloud workers this driver will provision.
	// Zero is legitimate -- a development box, or a hybrid cell -- and is what the
	// pre-flight capacity check exists to pair against on-prem capacity.
	PlannedWorkerReplicas() int

	// CiliumAddonPath is the repository-relative path to the rendered Cilium
	// workloads this driver's hub installs.
	//
	// Declared per driver rather than derived from Name(), because the drivers do
	// not have a file each. hetzner's hub and its spokes install the same
	// artifact -- one copy, two consumers, deliberately: the comments on both warn
	// that drift between them is a cluster booting with a datapath nobody
	// intended. Deriving `cilium-addon-<name>.yaml` assumed a per-provider file
	// that was only ever authored for hybrid, so a released hetzner build could
	// not bootstrap at all.
	CiliumAddonPath() string

	// ── Phase 1: Preflight validators (docker, kind, cloud credentials) ───
	PreflightValidators() []preflight.Validator

	// ── Phase 3: ProvisionDayZero (create cloud secret, install CSI) ──────
	ProvisionDayZero(ctx context.Context, kubeconfig string) error

	// ── Phase 4: CAPI providers + init hook ──────────────────────────────
	CAPIProviders() []capi.CAPIProvider
	OnCAPIInit(ctx context.Context, kubeconfig, kubeContext, namespace string) error

	// ── Phase 5: Populate provisioning config ────────────────────────────
	// PopulateClusterConfig fills cloud-specific fields on a cluster.Config.
	// The CloudProvider fills shared fields (ClusterName, Namespace,
	// CiliumManifest) after this call returns.
	PopulateClusterConfig(cfg *cluster.Config)

	// ── Phase 9: ClusterClass paths ──────────────────────────────────────
	ClusterClassPaths() []string

	// ── Operator webhook patterns ───────────────────────────────────────
	OperatorWebhookPatterns() []string

	// ── Phase 10: Platform pre-requisites (post-pivot, on the mgmt cluster) ──
	// Applied against the management (Hub) kubeconfig AFTER pivot. Day-0
	// resources (cloud secret, CSI) installed on the ephemeral bootstrap are
	// lost when it is deleted; anything the Hub itself needs (e.g. CSI +
	// StorageClass) must be re-applied here.
	OnPlatformPreReqs(ctx context.Context, kubeconfig string) error

	// ── Capabilities (ADR-036 §5) ────────────────────────────────────────
	Capabilities() CapabilityContract
}

// ──────────────────────────────────────────────────────────────────────────────
// Shared helpers for CloudProvider
// ──────────────────────────────────────────────────────────────────────────────

// readTemplateManifest reads a manifest template and extracts the content under
// the given data key (used for cilium/CCM addon templates).
func readTemplateManifest(basePath, templateFile, dataKey string) ([]byte, error) {
	biosPath := basePath + templateFile
	data, err := platform.ReadFile(biosPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", biosPath, err)
	}

	var template map[string]interface{}
	if err := yaml.Unmarshal(data, &template); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", templateFile, err)
	}

	var manifestContent string
	if stringData, ok := template["stringData"].(map[string]interface{}); ok {
		if content, ok := stringData[dataKey].(string); ok {
			manifestContent = content
		}
	} else if dataMap, ok := template["data"].(map[string]interface{}); ok {
		if content, ok := dataMap[dataKey].(string); ok {
			manifestContent = content
		}
	}

	if manifestContent == "" {
		return nil, fmt.Errorf("manifest content not found in %s under key %s", templateFile, dataKey)
	}

	return []byte(manifestContent), nil
}

// waitForAllMachinesRunning polls CAPI machines until every machine has a
// nodeRef assigned (meaning the node has joined the cluster).
//
// The AllMachinesHaveNodesHealth check encapsulates the same JSONPath
// and per-machine validation logic that was previously inlined here.
//
// The context is named explicitly rather than inherited from the kubeconfig's
// current-context. Current-context is ambient state: earlier phases write the
// hub kubeconfig, and hub-bootstrap.sh exports KUBECONFIG to it whenever that
// file already exists — true on every resumed run. Relying on it made this
// wait query the hub, which has no CAPI CRDs until pivot installs them.
func waitForAllMachinesRunning(ctx context.Context, kubeconfig, kubeContext string, timeout time.Duration) error {
	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewAllMachinesHaveNodesHealth(constants.NamespaceCAPI, kubeContext),
		},
		Interval: 10 * time.Second,
		Timeout:  timeout,
	}
	return waiter.Wait(ctx, kubeconfig)
}

// binaries import is used by pivot but referenced transitively through the
// pivot package. This ensures the import stays.
var _ = binaries.ClusterctlManager{}

// bytes import for manifest reading.
var _ = bytes.NewReader

// filepath import for kubeconfig paths.
var _ = filepath.Join
