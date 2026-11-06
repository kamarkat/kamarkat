package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/capi"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/preflight"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
)

// HetznerDriver implements CloudDriver for Hetzner Cloud.
type HetznerDriver struct {
	Token             string
	Region            string
	OS                string // node OS; "ubuntu" is the only supported value (ADR-001)
	NetworkCIDR       string
	SSHKey            string
	Debug             bool
	BuildFlatcarImage bool

	// OnPremEnabled reports whether this box accepts nodes on the tenant's own
	// premises. Read here only to decide worker capacity: a development box runs
	// no cloud workers, so its on-prem nodes are the only capacity it has.
	OnPremEnabled bool

	// ClusterName names the control plane on the tailnet (<name>-cp). Only read
	// when on-prem nodes are enabled.
	ClusterName string

	// WorkerReplicas overrides the count the environment would choose.
	//
	// A pointer because zero is a real answer -- a development box whose capacity
	// comes from the tenant's own hardware -- so it cannot double as "not given".
	// An int sentinel was tried and was wrong in the quietest possible way: Go's
	// zero value made every driver built without the field claim an explicit zero,
	// and every environment provisioned no workers.
	WorkerReplicas *int
}

// PlannedWorkerReplicas is how many cloud workers this box starts with.
//
// Two, and the count is declared rather than derived. An `Environment` field stood
// here claiming to decide it, and had already stopped being read: development
// briefly defaulted to zero on the reasoning that its capacity should come from the
// tenant's own hardware (ADR-075) -- which is the right end state and the wrong
// default to reach it by: it made on-prem hardware a precondition for the simplest
// box the platform can build, and `kakt tenant scaffold` with no arguments produced
// a repository whose bootstrap was refused for having nowhere to schedule.
//
// The field outlived that default as write-only state with a comment describing
// behaviour the code no longer had. Management capacity is management policy
// (ADR-111 Revision 7 §3) and arrives through WorkerReplicas below.
//
// A tenant who wants that arrangement asks for it with --workers 0, which is why
// the override below distinguishes an explicit zero from an absent flag.
//
// The count is a starting value, not a fixed one. Day-0 records the topology at
// clusters/<name>/generated/hub-cluster.yaml and the tenant's own control plane
// reconciles it, so changing cloud capacity later -- adding workers when an on-prem
// node leaves, or removing them once one arrives -- is an edit to that file.
func (d *HetznerDriver) PlannedWorkerReplicas() int {
	if d.WorkerReplicas != nil {
		return *d.WorkerReplicas
	}
	return 2
}

// ── CloudDriver interface ──────────────────────────────────────────────────

func (d *HetznerDriver) Name() string { return "hetzner" }

// OnPremRequested reports whether this box accepts nodes on the tenant's own
// premises (ADR-075).
//
// Declared here rather than only on the hybrid driver, which is what made
// `--provider hetzner --on-prem` silently do nothing: the pre-flight capacity
// check and the on-prem join phase both ask the provider this question, and a
// driver that could not answer it was read as "no". A hetzner box therefore
// staged empty tailnet credentials, joined no nodes, and -- once development
// boxes stopped provisioning cloud workers -- was refused for having no capacity
// while the flag that would have given it some was set.
func (d *HetznerDriver) OnPremRequested() bool { return d.OnPremEnabled }

// hubTailnetHostname is the name the control plane registers on the tailnet.
func (d *HetznerDriver) hubTailnetHostname() string {
	if d.ClusterName != "" {
		return d.ClusterName + "-cp"
	}
	return "hub-cp"
}

// CiliumAddonPath is the shared spoke-bootstrap template: hetzner's hub installs
// exactly what its spokes install. Its config base declares external-envoy-proxy
// false and gateway-api-hostnetwork-enabled false, and this artifact ships only
// the cilium DaemonSet -- no standalone cilium-envoy, no mangle guard. The two
// agree, which is what the hybrid cell needed its own artifact to achieve.
func (d *HetznerDriver) CiliumAddonPath() string {
	return "manifests/spoke/spoke-bootstrap/cilium-addon-template.yaml"
}
func (d *HetznerDriver) OSType() string { return d.OS }

// ── Phase 1: Preflight ──────────────────────────────────────────────────────

func (d *HetznerDriver) PreflightValidators() []preflight.Validator {
	v := []preflight.Validator{
		&preflight.DockerValidator{},
		&preflight.KindValidator{},
		&preflight.HetznerTokenValidator{Token: d.Token},
	}

	if d.SSHKey != "" {
		v = append(v, &preflight.SSHKeyValidator{Token: d.Token, KeyName: d.SSHKey})
	}

	return v
}

// ── Phase 3: Day-0 Infrastructure ───────────────────────────────────────────

func (d *HetznerDriver) ProvisionDayZero(ctx context.Context, kubeconfig string) error {
	// Create hetzner secret for CSI driver
	fmt.Println("[day0] Creating hetzner secret for CSI...")
	secretCmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"create", "secret", "generic", "hetzner",
		"-n", constants.NamespaceCloud,
		"--from-literal=hcloud="+d.Token,
		"--dry-run=client", "-o", "yaml",
	)
	secretYAML, err := secretCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to generate hetzner secret: %w", err)
	}

	applyCmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"apply", "-f", "-",
	)
	applyCmd.Stdin = bytes.NewReader(secretYAML)
	if out, err := applyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create hetzner secret: %w\n%s", err, out)
	}
	fmt.Println("[day0] ✓ Hetzner secret created")

	// The hcloud CSI controller can only run on a Hetzner Cloud server: it resolves
	// its own location from the metadata service at 169.254.169.254, and falls back
	// to looking itself up in the Hetzner API by KUBE_NODE_NAME. On an ephemeral
	// kind bootstrap cluster neither exists — the node is kind://docker/... — so the
	// driver container CrashLoops and this phase times out after 3 minutes.
	//
	// Nothing on the bootstrap cluster needs it. Its job is to run the CAPI/CAPH
	// controllers that provision the hub and then pivot; it creates no PVCs. The
	// management cluster gets its own CSI install in the platform-pre phase below,
	// where the nodes really are Hetzner servers.
	//
	// This surfaced with the hybrid provider (ADR-046), where the bootstrap cluster
	// is kind on a workstation. Gate on what the CSI actually requires rather than
	// on the provider name, so a bootstrap cluster that *is* hosted on Hetzner
	// (--bootstrap-context) still gets the driver.
	onHetzner, err := nodesAreHetznerServers(ctx, kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to inspect bootstrap cluster nodes: %w", err)
	}
	if !onHetzner {
		fmt.Println("[day0] Skipping hetzner-csi: bootstrap cluster nodes are not Hetzner servers")
		fmt.Println("[day0]   (the CSI controller requires the Hetzner metadata service;")
		fmt.Println("[day0]    it is installed on the management cluster in platform-pre)")
		return nil
	}

	// Install CSI driver
	fmt.Println("[day0] Installing hetzner-csi...")
	csiManifest, err := assets.ReadCatalog("cloud-providers/hetzner/csi/install.yaml")
	if err != nil {
		return fmt.Errorf("failed to read CSI manifest: %w", err)
	}

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "--kubeconfig", kubeconfig, "-f", "-")
	cmd.Stdin = bytes.NewReader(csiManifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to install CSI: %w\n%s", err, out)
	}

	if err := waitForDeployment(ctx, kubeconfig, constants.NamespaceCloud, "hcloud-csi-controller", 3*time.Minute); err != nil {
		return fmt.Errorf("failed to verify CSI: %w", err)
	}
	fmt.Println("[day0] ✓ hetzner-csi ready")
	return nil
}

// writeTailscaleSecret creates or updates tailscale-hybrid-psk in the target
// namespace. Empty values are meaningful, not a bug: they render the ClusterClass's
// tailscale steps inert on clusters that have no tailnet, while still satisfying
// the contentFrom.secret reference that would otherwise block bootstrap rendering.
func writeTailscaleSecret(ctx context.Context, kubeconfig, namespace, authkey, hostname string) error {
	gen := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"create", "secret", "generic", "tailscale-hybrid-psk",
		"-n", namespace,
		"--from-literal=authkey="+authkey,
		"--from-literal=hostname="+hostname,
		"--dry-run=client", "-o", "yaml",
	)
	manifest, err := gen.Output()
	if err != nil {
		return fmt.Errorf("render tailscale secret: %w", err)
	}

	apply := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	apply.Stdin = bytes.NewReader(manifest)
	if out, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("apply tailscale secret: %w\n%s", err, out)
	}
	return nil
}

// nodesAreHetznerServers reports whether every node in the target cluster is a
// Hetzner Cloud server, judged by providerID (hcloud://...). A kind node reports
// kind://docker/..., and a node that has not yet been assigned a providerID
// reports an empty string — neither can run the hcloud CSI controller.
func nodesAreHetznerServers(ctx context.Context, kubeconfig string) (bool, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"get", "nodes",
		"-o", "jsonpath={range .items[*]}{.spec.providerID}{\"\\n\"}{end}",
	)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("kubectl get nodes: %w", err)
	}

	found := false
	for _, line := range strings.Split(string(out), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		found = true
		if !strings.HasPrefix(id, "hcloud://") {
			return false, nil
		}
	}
	// No providerIDs at all: the cloud-controller-manager has not run yet, so this
	// is certainly not a ready Hetzner cluster.
	return found, nil
}

// ── Phase 4: CAPI Initialization ────────────────────────────────────────────

func (d *HetznerDriver) CAPIProviders() []capi.CAPIProvider {
	providers := []capi.CAPIProvider{
		{Kind: "CoreProvider", Name: "cluster-api", Version: versions.CAPIVersion, Manifest: "core/capi-operator/providers/core-provider.yaml"},
	}

	providers = append(providers,
		capi.CAPIProvider{Kind: "BootstrapProvider", Name: "kubeadm", Version: versions.KubeadmBootstrapProviderVersion, Manifest: "core/capi-operator/providers/bootstrap-provider-kubeadm.yaml"},
		capi.CAPIProvider{Kind: "ControlPlaneProvider", Name: "kubeadm", Version: versions.KubeadmControlPlaneProviderVersion, Manifest: "core/capi-operator/providers/controlplane-provider-kubeadm.yaml"},
	)

	providers = append(providers, capi.CAPIProvider{
		Kind:     "InfrastructureProvider",
		Name:     "hetzner",
		Version:  versions.HetznerInfraProviderVersion,
		Manifest: "core/capi-operator/providers/infrastructure-provider-hetzner.yaml",
	})

	return providers
}

func (d *HetznerDriver) OnCAPIInit(ctx context.Context, kubeconfig, context, namespace string) error {
	secretMgr := &capi.SecretManager{
		Kubeconfig: kubeconfig,
		Context:    context,
		Namespace:  namespace,
	}

	if err := secretMgr.CreateHetznerSecret(ctx, d.Token); err != nil {
		return fmt.Errorf("failed to create Hetzner secret: %w", err)
	}
	fmt.Println("[capi-init] ✓ Hetzner credentials secret created")

	// The shared hub ClusterClass reads /etc/tailscale-{authkey,hostname} via
	// contentFrom.secret, so the Secret must exist before the Cluster is created or
	// the KubeadmConfig never renders. It is written here, at capi-init, because
	// the ClusterClass reads it while the hub Cluster is being created.
	//
	// A box with no on-prem nodes gets EMPTY values: every tailscale command in the
	// ClusterClass is guarded on `[ -s /etc/tailscale-hostname ]` and becomes a
	// no-op.
	return d.StageTailscaleCredentials(ctx, kubeconfig, namespace)
}

// StageTailscaleCredentials puts this box's control plane on the tenant's tailnet,
// or leaves the credentials empty when it has no on-prem nodes.
//
// ADR-046 invariant 6: Cilium derives its VXLAN tunnel endpoint from a node's
// InternalIP, and an on-prem node's only InternalIP is its tailnet address. Such a
// node cannot route the hub control plane's Hetzner private IP, so unless the
// control plane also advertises a tailnet address, cross-node pod traffic dies in
// one direction while both nodes report Ready.
//
// Shared by both providers rather than overridden by the hybrid one. It used to
// live only there, which is why a hetzner box with --on-prem staged nothing: the
// flag was read, the capability was reported, and the credential the ClusterClass
// needed was written empty anyway.
func (d *HetznerDriver) StageTailscaleCredentials(ctx context.Context, kubeconfig, namespace string) error {
	if !d.OnPremEnabled {
		if err := writeTailscaleSecret(ctx, kubeconfig, namespace, "", ""); err != nil {
			return fmt.Errorf("failed to create placeholder tailscale secret: %w", err)
		}
		return nil
	}

	authkey, err := readTailscaleAuthkey()
	if err != nil {
		// Not fatal: the cluster still builds, but on-prem nodes will not be able
		// to exchange pod traffic with it. Said loudly here rather than failing
		// later with an unexplained timeout.
		fmt.Printf("[capi-init] ⚠️  %v\n", err)
		fmt.Println("[capi-init] ⚠️  Control plane will NOT join the tailnet; cross-node pod traffic to")
		fmt.Println("[capi-init]     on-prem nodes will fail (ADR-046 invariant 6).")
		if err := writeTailscaleSecret(ctx, kubeconfig, namespace, "", ""); err != nil {
			return fmt.Errorf("failed to create placeholder tailscale secret: %w", err)
		}
		return nil
	}

	hostname := d.hubTailnetHostname()
	if err := writeTailscaleSecret(ctx, kubeconfig, namespace, authkey, hostname); err != nil {
		return fmt.Errorf("failed to write tailscale credentials: %w", err)
	}
	fmt.Printf("[capi-init] ✓ Tailscale credentials staged for control plane (%s)\n", hostname)
	return nil
}

// ── Phase 5: Cluster Provisioning Config ────────────────────────────────────

func (d *HetznerDriver) PopulateClusterConfig(cfg *cluster.Config) {
	subnetCIDR := d.NetworkCIDR[:len(d.NetworkCIDR)-2] + "24"

	cfg.Region = d.Region
	cfg.OSType = d.OS
	// No node image here. The management ClusterClass declares `imageId` with a
	// default, and the spoke pool composition already relies on it rather than
	// restating it ("We omit imageId ... relying on the ClusterClass defaults").
	// Setting it here made the hub the only caller carrying a second copy of the
	// same string, so the OS a box runs was written in two files that nothing
	// compared.
	cfg.KubernetesVersion = versions.KubernetesVersion
	cfg.NetworkCIDR = d.NetworkCIDR
	cfg.SubnetCIDR = subnetCIDR
	cfg.ControlPlaneMachineType = "cx33"
	cfg.WorkerMachineType = "cx33"
	cfg.ControlPlaneReplicas = 1
	cfg.WorkerReplicas = d.PlannedWorkerReplicas()
	cfg.HCloudToken = d.Token

	// SSH key name for rescue/emergency access. Defaults to the key present in
	// the Hetzner project when --ssh-key is not supplied.
	if d.SSHKey != "" {
		cfg.SSHKeyName = d.SSHKey
	} else {
		cfg.SSHKeyName = "mac-mini-ssh"
	}

	// Read CCM addon manifest from shared base (ADR-046 §WS1: spoke-addons moved to hetzner/base/).
	ccmRaw, err := readTemplateManifest(
		"manifests/providers/hetzner/base/spoke-addons/", "ccm-addon-template.yaml", "ccm.yaml")
	if err != nil {
		// Log but don't fail — CCM can be installed later
		fmt.Printf("[cluster-provision] Warning: failed to read CCM manifest: %v\n", err)
	} else {
		cfg.CCMManifest = string(ccmRaw)
	}
}

// ── Phase 10: Platform Pre-Requisites ────────────────────────────────────────
// The day-0 CSI install runs against the ephemeral bootstrap cluster, which is
// deleted after pivot. Re-apply the CSI driver + StorageClass to the management
// (Hub) cluster so PVs provision (hcloud-volumes) for hub databases/Redis.
func (d *HetznerDriver) OnPlatformPreReqs(ctx context.Context, kubeconfig string) error {
	fmt.Println("[platform-pre] Installing hetzner-csi on management cluster...")
	csiManifest, err := assets.ReadCatalog("cloud-providers/hetzner/csi/install.yaml")
	if err != nil {
		return fmt.Errorf("failed to read CSI manifest: %w", err)
	}

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "--kubeconfig", kubeconfig, "-f", "-")
	cmd.Stdin = bytes.NewReader(csiManifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to install CSI on management cluster: %w\n%s", err, out)
	}

	if err := waitForDeployment(ctx, kubeconfig, constants.NamespaceCloud, "hcloud-csi-controller", 3*time.Minute); err != nil {
		return fmt.Errorf("failed to verify CSI on management cluster: %w", err)
	}
	fmt.Println("[platform-pre] ✓ hetzner-csi ready on management cluster")
	return nil
}

// ── Phase 9: ClusterClass ───────────────────────────────────────────────────

func (d *HetznerDriver) ClusterClassPaths() []string {
	return []string{"classes/hetzner-mgmt-ubuntu-v1.yaml"}
}

// ── Capabilities ────────────────────────────────────────────────────────────

func (d *HetznerDriver) OperatorWebhookPatterns() []string {
	return []string{"caph"}
}
func (d *HetznerDriver) Capabilities() CapabilityContract {
	return CapabilityContract{
		Version:      "1.0.0",
		StorageClass: "hcloud-volumes",
		BlockStorage: true,
		LoadBalancer: true,
		GPU:          true,
		MaxNodes:     0,
	}
}

// ── Shared helper ───────────────────────────────────────────────────────────

// waitForDeployment polls kubectl rollout status for a deployment.
func waitForDeployment(ctx context.Context, kubeconfig, namespace, deployment string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for deployment %s/%s", namespace, deployment)
			}
			cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
				"rollout", "status", "deployment/"+deployment,
				"-n", namespace, "--timeout=10s")
			if err := cmd.Run(); err == nil {
				return nil
			}
		}
	}
}
