package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/bootstrap"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/preflight"
	"github.com/spf13/cobra"
)

var (
	bootstrapEncryption string
)

var (
	// Required flags
	clusterName   string
	gitopsDir     string
	oidcIssuerURL string
	oidcClientID  string
	region        string

	// Optional flags
	provider          string
	osType            string
	bootstrapContext  string
	keepBootstrap     bool
	dryRun            bool
	mergeKubeconfig   bool
	networkCIDR       string
	upgrade           bool
	sshKey            string
	buildFlatcarImage bool
	debug             bool
	topology          string
	gating            string

	// Hybrid-provider flags (ADR-046 §WS4)
	onPremEnabled bool
	// -1 rather than 0: zero workers is a real answer, and the default depends on
	// the environment, which is not known when flags are declared.
	workerReplicas int
	onPremJoinTTL  string
	tailnetName    string
)

// workerReplicasOverride is the --workers value, or nil when it was not passed.
// Zero is a real count, so the flag uses a negative sentinel and this converts it.
// requireTailnetCredentials refuses an on-prem bootstrap that cannot put the
// control plane on the tenant's tailnet.
//
// Both halves are needed and they come from different places: the authkey joins
// the tailnet, the name is what the box is called on it. Checked together so a
// half-configured run is refused once, with both gaps named, rather than failing
// twice.
func requireTailnetCredentials(tailnet string) error {
	var missing []string

	if strings.TrimSpace(tailnet) == "" {
		missing = append(missing, "  --tailnet-name is empty. Pass the tenant's tailnet, e.g. --tailnet-name acme.ts.net")
	}

	keyed := os.Getenv("TS_AUTHKEY") != "" || os.Getenv("TAILSCALE_AUTHKEY") != ""
	if !keyed {
		if _, err := os.Stat(filepath.Join("k8-secrets", "tailscale", "authkey")); err != nil {
			missing = append(missing, "  no Tailscale authkey. Set TS_AUTHKEY (or TAILSCALE_AUTHKEY), "+
				"or write one to k8-secrets/tailscale/authkey")
		}
	}

	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("--on-prem needs the control plane on the tenant's tailnet:\n%s\n\n"+
		"Without it Cilium cannot route pod traffic between the control plane and on-prem\n"+
		"nodes, and the cluster reports every node Ready while one direction silently fails\n"+
		"(ADR-046 invariant 6). Refused here rather than discovered later.",
		strings.Join(missing, "\n"))
}

func workerReplicasOverride() *int {
	if workerReplicas < 0 {
		return nil
	}
	n := workerReplicas
	return &n
}

// resolveHCloudToken makes the Hetzner token available to everything that reads it.
//
// Several places in this CLI read HCLOUD_TOKEN from the environment -- bootstrap,
// pivot twice, and teardown -- so the token is resolved once, here, and put
// back into the environment rather than threaded through each of them.
//
// The file fallback exists because teardown already had it and bootstrap did not:
// the same credential, in the same file, worked for tearing a cluster down and not
// for building one. k8-secrets/ is the operator's own gitignored directory, so this
// helps someone working in a checkout and does nothing in CI, where the variable is
// set from a secret.
func resolveHCloudToken() error {
	if os.Getenv("HCLOUD_TOKEN") != "" {
		return nil
	}
	const rel = "k8-secrets/hetzner/token"
	for _, path := range hcloudTokenPaths(rel) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			continue
		}
		// Set rather than returned: the readers downstream take it from the
		// environment, and passing it explicitly would mean changing all of them.
		if err := os.Setenv("HCLOUD_TOKEN", token); err != nil {
			return fmt.Errorf("could not make the Hetzner token available: %w", err)
		}
		fmt.Printf("[bootstrap] using the Hetzner token from %s\n", path)
		return nil
	}
	return fmt.Errorf("no Hetzner API token.\n\n"+
		"Clusters are created with it, so bootstrap needs one of:\n\n"+
		"  HCLOUD_TOKEN   exported in this shell\n"+
		"  %s   the same token, relative to this checkout\n\n"+
		"Create one at https://console.hetzner.cloud/ under Security > API tokens,\n"+
		"with Read & Write permission.", rel)
}

// hcloudTokenPaths lists where the token file may be, nearest first.
//
// The working directory is no longer where the operator's checkout is. A released
// binary carries its platform content (ADR-063), so bootstrap is run FROM the
// tenant's repository -- and the tenant's repository has no k8-secrets/. The
// fallback is therefore resolved against the checkout rather than the cwd, which
// is what it always meant: k8-secrets/ is the operator's own gitignored directory,
// and reading it from wherever bootstrap happens to be run was an accident of the
// two being the same place.
//
// ZERO_OPS_DIR first because a developer can say where the checkout is; the
// executable's directory second because `bin/kakt` and `./kakt` both sit in one.
func hcloudTokenPaths(rel string) []string {
	paths := []string{rel}
	if root := strings.TrimSpace(os.Getenv("ZERO_OPS_DIR")); root != "" {
		paths = append(paths, filepath.Join(root, rel))
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			dir := filepath.Dir(exe)
			// Both layouts the repository produces: `./kakt` at the root, and
			// `bin/kakt` from the Makefile.
			paths = append(paths,
				filepath.Join(dir, rel),
				filepath.Join(filepath.Dir(dir), rel))
		}
	}
	return paths
}

func newBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap-mgmt",
		Short: "Bootstrap the management cluster (one step of a box)",
		Long: `Bootstrap a self-hosted Hub (Management) Cluster using Cluster API (CAPI).
Supports multiple infrastructure providers: hetzner (cloud) and hybrid (home-lab workers).

Naming follows kubefirst: the MANAGEMENT cluster runs the platform, WORKER
clusters run tenant workloads. This command builds the former.

This is ONE STEP of a box. It stands up the management control plane and stops there:
the gates that decide whether the box is usable -- secrets present, Infisical
reachable, the database provisioned, the declared worker clusters actually up --
are separate steps, and ` + "`kakt bootstrap`" + ` runs all of them in order.

Running this directly gives you a reconciled management cluster and no assurance about anything
else. That is a legitimate thing to want while developing; it is not a bootstrapped
box, and on 2026-09-15 the difference was a worker cluster that had been dead for
sixteen hours behind a run that reported success.`,
		PreRunE: validateFlags,
		RunE:    runBootstrap,
	}

	// Required flags
	cmd.Flags().StringVar(&clusterName, "name", "", "Hub Cluster name (alphanumeric + hyphens)")
	// ADR-076. Empty on a first build: the issuer runs on the cluster being
	// created, so at this moment it does not exist and cannot be pointed at.
	// Supplied on a REBUILD of an environment whose issuer already runs, which is
	// the case where declaring them here avoids provisioning without them and then
	// replacing the control plane to add them.
	cmd.Flags().StringVar(&oidcIssuerURL, "oidc-issuer-url", "",
		"identity provider the API server accepts tokens from (ADR-076); requires --oidc-client-id")
	cmd.Flags().StringVar(&oidcClientID, "oidc-client-id", "",
		"OIDC client the API server names; requires --oidc-issuer-url")
	cmd.Flags().StringVar(&gitopsDir, "gitops-dir", "",
		"a checkout of the tenant's own repository; the cluster is seeded with the declaration it holds (ADR-072)")
	cmd.Flags().StringVar(&provider, "provider", "hetzner", "Infrastructure provider: hetzner (default) or hybrid (home-lab)")

	// Hetzner-specific flags (required when provider=hetzner)
	cmd.Flags().StringVar(&region, "region", "", "Hetzner region (fsn1, nbg1, hel1)")

	// Optional flags
	cmd.Flags().StringVar(&osType, "os", "ubuntu", "OS type: ubuntu (default; the only supported node OS, ADR-001)")
	cmd.Flags().StringVar(&bootstrapContext, "bootstrap-context", "", "Use existing K8s cluster instead of Kind")
	cmd.Flags().BoolVar(&keepBootstrap, "keep-bootstrap", false, "Preserve Kind cluster after successful pivot")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate prerequisites and generate manifests without provisioning")
	cmd.Flags().BoolVar(&mergeKubeconfig, "merge-kubeconfig", false, "Merge generated kubeconfig into ~/.kube/config")
	cmd.Flags().StringVar(&networkCIDR, "network-cidr", "10.0.0.0/16", "Custom CIDR for Hetzner private network")
	cmd.Flags().BoolVar(&upgrade, "upgrade", false, "Reconcile/update existing cluster components to match CLI version")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH key name for Hetzner Rescue Mode emergencies only")
	cmd.Flags().BoolVar(&buildFlatcarImage, "build-flatcar-image", false, "Trigger Packer build for Flatcar image")
	cmd.Flags().BoolVar(&debug, "debug", false, "Enable verbose logging")
	cmd.Flags().IntVar(&workerReplicas, "workers", -1,
		"cloud worker nodes to provision (default: 2). Pass 0 to run on on-prem "+
			"capacity alone, which then has to be there (ADR-075)")
	cmd.Flags().StringVar(&topology, "topology", "single", "Topology mode: single (default) or multi (bridged)")
	cmd.Flags().StringVar(&bootstrapEncryption, "encryption-mode", string(cluster.EncryptionSecretbox),
		"how this box encrypts Secrets at rest: secretbox (ADR-003 §6, default)")
	cmd.Flags().StringVar(&gating, "gating", "sequenced", "Cluster creation mode (ADR-055): sequenced (default, boundaries activated in phase order) or converged (all boundaries reconcile concurrently)")

	// Hybrid-provider flags (ADR-046 §WS4)
	cmd.Flags().BoolVar(&onPremEnabled, "on-prem", false, "accept nodes on the tenant's own premises, joining this cluster over their tailnet (ADR-075)")
	cmd.Flags().StringVar(&onPremJoinTTL, "on-prem-join-ttl", "24h", "kubeadm bootstrap-token TTL for on-prem nodes")
	cmd.Flags().StringVar(&tailnetName, "tailnet-name", "", "the tenant's Tailscale tailnet, e.g. acme.ts.net. Required with --on-prem")

	// Mark required flags
	cmd.MarkFlagRequired("name")

	return cmd
}

func validateFlags(cmd *cobra.Command, args []string) error {
	// Both or neither. The ClusterClass patch is conditioned on both being set,
	// so one alone is not a partial configuration -- it is silently no
	// configuration, and the API server comes up trusting nothing while the flag
	// that was passed suggests otherwise (ADR-076).
	if (oidcIssuerURL == "") != (oidcClientID == "") {
		return fmt.Errorf("--oidc-issuer-url and --oidc-client-id are supplied together or not at all; " +
			"one alone configures no OIDC at all and looks as though it did")
	}

	// Validate provider
	validProviders := map[string]bool{
		"hetzner": true,
		"hybrid":  true,
	}
	if !validProviders[provider] {
		return fmt.Errorf("invalid provider: must be 'hetzner' or 'hybrid'")
	}

	// Validate cluster name (alphanumeric + hyphens)
	nameRegex := regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
	if !nameRegex.MatchString(clusterName) {
		return fmt.Errorf("invalid cluster name: must contain only alphanumeric characters and hyphens")
	}

	validRegions := map[string]bool{
		"fsn1": true,
		"nbg1": true,
		"hel1": true,
	}

	// Provider-specific validation (hetzner and hybrid share region/token requirements)
	switch provider {
	case "hetzner", "hybrid":
		if region == "" {
			return fmt.Errorf("--region is required for provider '%s'", provider)
		}
		if !validRegions[region] {
			return fmt.Errorf("invalid region: must be one of fsn1, nbg1, hel1")
		}
		if osType != "ubuntu" {
			return fmt.Errorf("unsupported OS type %q: ubuntu is the only supported node operating system (ADR-001)", osType)
		}
		if err := resolveHCloudToken(); err != nil {
			return err
		}
	}

	return nil
}

func runBootstrap(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if debug {
		fmt.Println("[DEBUG] Bootstrap command started")
		fmt.Printf("[DEBUG] Provider: %s\n", provider)
		fmt.Printf("[DEBUG] Cluster Name: %s\n", clusterName)
		fmt.Printf("[DEBUG] OS Type: %s\n", osType)
		fmt.Printf("[DEBUG] Dry Run: %v\n", dryRun)
	}

	fmt.Println("🚀 Starting Hub Cluster bootstrap...")
	fmt.Printf("   Provider: %s\n", provider)
	fmt.Printf("   Cluster Name: %s\n", clusterName)
	if provider == "hetzner" || provider == "hybrid" {
		fmt.Printf("   OS Type: %s\n", osType)
		fmt.Printf("   Region: %s\n", region)
		fmt.Printf("   Network CIDR: %s\n", networkCIDR)
	}
	if provider == "hybrid" {
		fmt.Printf("   On-prem nodes: %v\n", onPremEnabled)
		fmt.Printf("   Tailnet: %s\n", tailnetName)
	}

	// On-prem requires a tailnet, and the requirement is checked HERE -- before
	// anything is provisioned -- rather than warned about inside a phase.
	//
	// ADR-046 invariant 6: Cilium takes its VXLAN tunnel endpoint from a node's
	// InternalIP. An on-prem node's only InternalIP is its tailnet address, so
	// unless the control plane is on the tailnet too, cross-node pod traffic dies
	// in one direction WHILE BOTH NODES REPORT READY. That last part is why this
	// is fatal rather than advisory: the cluster looks healthy and cannot route.
	//
	// It used to be a warning printed by capi-init. A resumed bootstrap skips
	// completed phases, so on the run that mattered the warning was never
	// printed, the control plane had no tailnet address, and the failure surfaced
	// much later as cert-manager's webhook timing out from the API server --
	// three layers from its cause.
	if onPremEnabled {
		if err := requireTailnetCredentials(tailnetName); err != nil {
			return err
		}
	}

	// At-rest encryption is settled HERE, before anything is provisioned, for the
	// same reason the tailnet check above is: the control-plane provider
	// configuration is FILE content CAPI reads while the management cluster is being
	// created, so a box that gets to phase 5 without a usable key authority has
	// already provisioned infrastructure it will have to tear down. A resumed
	// bootstrap skips completed phases, so a check inside the phase is a check that
	// does not run on the attempt that matters.
	encryptionMode, err := resolveEncryptionMode()
	if err != nil {
		return err
	}

	// Build provider based on --provider flag
	var bp bootstrap.Provider

	switch provider {
	case "hetzner":
		hcloudToken := os.Getenv("HCLOUD_TOKEN")
		driver := &bootstrap.HetznerDriver{
			Token:             hcloudToken,
			Region:            region,
			OS:                osType,
			NetworkCIDR:       networkCIDR,
			SSHKey:            sshKey,
			Debug:             debug,
			BuildFlatcarImage: buildFlatcarImage,
			OnPremEnabled:     onPremEnabled,
			WorkerReplicas:    workerReplicasOverride(),
			ClusterName:       clusterName,
		}
		bp = bootstrap.NewCloudProvider(driver, clusterName, debug)
	case "hybrid":
		hcloudToken := os.Getenv("HCLOUD_TOKEN")
		driver := &bootstrap.HetznerDriver{
			Token:             hcloudToken,
			Region:            region,
			OS:                osType,
			NetworkCIDR:       networkCIDR,
			SSHKey:            sshKey,
			Debug:             debug,
			BuildFlatcarImage: buildFlatcarImage,
			OnPremEnabled:     onPremEnabled,
			WorkerReplicas:    workerReplicasOverride(),
			ClusterName:       clusterName,
		}
		hybridDriver := &bootstrap.HybridDriver{
			Driver:        driver,
			TailnetName:   tailnetName,
			OnPremEnabled: onPremEnabled,
			HomeWorkerTTL: onPremJoinTTL,
			ClusterName:   clusterName,
		}
		bp = bootstrap.NewCloudProvider(hybridDriver, clusterName, debug)
	default:
		return fmt.Errorf("unsupported provider: %s", provider)
	}

	if dryRun {
		// Dry-run validates without checkpointing — no state is written.
		fmt.Println("\n[preflight] Running validation checks...")
		runner := preflight.NewRunner()
		for _, v := range bp.PreflightValidators() {
			runner.Add(v)
		}
		if err := runner.Run(ctx); err != nil {
			return fmt.Errorf("preflight validation failed: %w", err)
		}
		fmt.Println("[preflight] ✓ All checks passed")
		fmt.Println("\n✓ Dry-run mode: Validation successful")
		fmt.Println("  All prerequisites validated. Ready to bootstrap.")
		return nil
	}

	// The management secret scope, READ from the box's own declaration (ADR-111
	// Revision 7 §2). There is no --environment flag and no default.
	//
	// It was `envSlug := environment; if envSlug == "" { envSlug = "prod" }`: a
	// hub with no environment given silently became a production one, which is
	// why hub-bootstrap.sh had to make its own --environment mandatory to defend
	// against this coercion. Reading the declaration removes the flag, the
	// default and the guard together, and makes activating a new scope an edit to
	// clusters/<mgmt>/values.yaml rather than a code change.
	box, err := bootstrap.ReadBoxValues(gitopsDir, clusterName)
	if err != nil {
		return err
	}

	// ADR-055: mode and environment are independent inputs. Every environment
	// class may be created in either mode; no combination is prohibited.
	gatingMode, err := bootstrap.ValidGatingMode(gating)
	if err != nil {
		return err
	}
	fmt.Printf("[bootstrap] Cluster creation mode: %s\n", gatingMode)

	// Phase 2-12: Bootstrap pipeline
	orchestrator := &bootstrap.Orchestrator{
		Provider:         bp,
		ClusterName:      clusterName,
		BootstrapContext: bootstrapContext,
		KeepBootstrap:    keepBootstrap,
		MergeKubeconfig:  mergeKubeconfig,
		Debug:            debug,
		ManagementScope:  box.ManagementScope(),
		Topology:         topology,
		Gating:           gatingMode,
		GitopsDir:        gitopsDir,
		OIDCIssuerURL:    oidcIssuerURL,
		OIDCClientID:     oidcClientID,
		Encryption:       encryptionMode,
	}

	if err := orchestrator.Run(ctx); err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}

	return nil
}

// resolveEncryptionMode validates --encryption-mode.
//
// `secretbox` (ADR-003 §6) is the platform's at-rest encryption mechanism.
func resolveEncryptionMode() (cluster.EncryptionMode, error) {
	mode := cluster.EncryptionMode(strings.TrimSpace(bootstrapEncryption))
	switch mode {
	case cluster.EncryptionSecretbox, "":
		return cluster.EncryptionSecretbox, nil
	default:
		return "", fmt.Errorf(
			"--encryption-mode %q is not supported; expected %q (ADR-003 §6)",
			bootstrapEncryption, cluster.EncryptionSecretbox)
	}
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

func readGitHubToken() string {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token
	}
	data, err := os.ReadFile("k8-secrets/github/github-pat-token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
