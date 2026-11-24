package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/bootstrap"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/preflight"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
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
		if opstore.ValueOrEnv("TS_AUTHKEY", "tailscale/authkey") == "" {
			missing = append(missing, "  no Tailscale authkey. Set TS_AUTHKEY (or TAILSCALE_AUTHKEY), "+
				"or write one to ~/.kamarkat/tailscale/authkey")
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

// resolveHCloudToken puts the BOX'S OWN cloud credential into the environment.
//
// The readers downstream -- the provider driver, the pivot, the preflight
// validators -- take it from the environment, so it is resolved once here and set
// for them rather than threaded through each.
//
// It comes from the box's escrow and from nowhere else. HCLOUD_TOKEN exported in a
// shell and k8-secrets/hetzner/token beside a checkout were both accepted before,
// and both are ways for a credential belonging to a DIFFERENT provider project to
// authenticate perfectly and answer about the wrong infrastructure: an inventory
// that comes back empty, a duplicate management cluster admitted because the first
// was invisible, a teardown that deletes nothing and succeeds. Anything already in
// the environment is overwritten for the same reason.
//
// Enrolment is the one place an operator hands over a credential, and it is
// explicit and piped (`kakt escrow enroll-provider-credential`). A box whose
// escrow holds none fails closed here, naming that command.
func resolveHCloudToken(ctx context.Context, gitopsDir, clusterName string) (string, error) {
	token, err := hubaccess.ProviderToken(ctx, gitopsDir, clusterName, "hetzner")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("this box's escrow holds no Hetzner credential, and clusters are "+
			"created with one.\n\nEnrol it, as the platform administrator:\n\n"+
			"  kakt escrow enroll-provider-credential --gitops-dir %s --cluster %s < token",
			gitopsDir, clusterName)
	}
	if err := os.Setenv("HCLOUD_TOKEN", token); err != nil {
		return "", fmt.Errorf("could not make the box's Hetzner credential available: %w", err)
	}
	return token, nil
}

func newBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap-mgmt",
		Short: "Bootstrap the management cluster (one step of a box)",
		// A RUNTIME FAILURE IS NOT A USAGE ERROR. Day-0 failures printed this
		// command's twenty-five flags under the error, and a tenant's CI log
		// then ended with a flag list -- which read as "unknown flag" and sent
		// the next reader looking for a typo instead of at the phase that
		// failed. `kakt bootstrap` already silences it; this is the command that
		// bootstrap runs.
		SilenceUsage: true,
		Long: `Bootstrap a self-hosted Hub (Management) Cluster using Cluster API (CAPI).
Supports multiple infrastructure providers: hetzner (cloud) and hybrid (home-lab workers).

Naming follows kubefirst: the MANAGEMENT cluster runs the platform, WORKER
clusters run tenant workloads. This command builds the former.

This is ONE STEP of a box. It stands up the management control plane and stops there:
the gates that decide whether the box is usable -- secrets present, OpenBao
reachable, the database provisioned, the declared worker clusters actually up --
are separate steps.

Running this directly gives you a reconciled management cluster and no assurance about anything
else. That is a legitimate thing to want while developing; it is not a bootstrapped
box, and on 2026-09-15 the difference was a worker cluster that had been dead for
sixteen hours behind a run that reported success.`,
		PreRunE: validateFlags,
		RunE:    runBootstrap,
	}

	// Required flags
	cmd.Flags().StringVar(&clusterName, "name", "", "management cluster name (alphanumeric + hyphens)")
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
		"cloud worker nodes this management cluster runs on (default: 2). It needs at "+
			"least one: a cluster with none is refused before anything is provisioned, "+
			"because the control plane keeps its taint and no platform workload can schedule")
	cmd.Flags().StringVar(&topology, "topology", "single", "Topology mode: single (default) or multi (bridged)")
	cmd.Flags().StringVar(&bootstrapEncryption, "encryption-mode", string(cluster.EncryptionSecretbox),
		"how this box encrypts Secrets at rest: secretbox (ADR-003 §6, default)")
	cmd.Flags().StringVar(&gating, "gating", "sequenced", "Cluster creation mode (ADR-055): sequenced (default, boundaries activated in phase order) or converged (all boundaries reconcile concurrently)")

	// Hybrid-provider flags (ADR-046 §WS4)
	cmd.Flags().BoolVar(&onPremEnabled, "on-prem", false, "accept nodes on the tenant's own premises, joining this cluster over their tailnet (ADR-075)")
	cmd.Flags().StringVar(&onPremJoinTTL, "on-prem-join-ttl", "24h", "kubeadm bootstrap-token TTL for on-prem nodes")
	cmd.Flags().StringVar(&tailnetName, "tailnet-name", "", "the tenant's Tailscale tailnet, e.g. acme.ts.net. Required with --on-prem")

	// NOT OFFERED. A management cluster admits no nodes on the tenant's own
	// premises, so there is nothing for an operator to turn on here: the
	// capability belongs to a managed cluster serving dev, where listing the
	// machines is the whole of the decision.
	//
	// Hidden rather than deleted because the hybrid cell still joins home-lab
	// nodes to its own hub -- ADR-046 left it no other worker capacity -- and
	// `kakt bootstrap` passes these for a hybrid box's declaration. They go when
	// the cell goes.
	for _, f := range []string{"on-prem", "tailnet-name", "on-prem-join-ttl"} {
		if err := cmd.Flags().MarkHidden(f); err != nil {
			panic(fmt.Sprintf("flag %s is declared above and must be hideable: %v", f, err))
		}
	}

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
	// ON-PREM IS A MANAGED CLUSTER'S CAPABILITY, IN DEV, AND NOT THE HUB'S.
	//
	// That is what the architecture this platform publishes says -- "On-premises
	// worker nodes · dev only", inside the managed-clusters block, "your own
	// machines, joining over your tailnet" -- and the documented shape is the one
	// that holds. ADR-075 had made on-prem a property of a BOX instead, so
	// `--on-prem` was accepted here and the hub pipeline carried an on-prem-join
	// phase for every build; on a hetzner box that phase has never admitted a
	// node, which is ADR-075's own unmet exit condition.
	//
	// A hub's capacity is its cloud workers. A tenant who wants their own machines
	// declares them on a managed cluster that serves dev, where the thing those
	// machines run is the thing they are for.
	//
	// The hybrid cell is the exception and is on its way out: ADR-046 made its
	// hub's ONLY worker capacity home-lab nodes, so refusing it here would leave
	// those boxes with one tainted control plane and nowhere to schedule. It keeps
	// the hub-side join until the cell is retired.
	if onPremEnabled && provider != "hybrid" {
		return fmt.Errorf("--on-prem is not a management cluster's setting.\n\n"+
			"On-premises nodes are capacity for a MANAGED cluster serving dev -- your own\n"+
			"machines, joining that cluster over your tailnet. A management cluster runs on\n"+
			"its cloud workers; --workers sets how many (0 is not supported on %s).\n\n"+
			"Declare the on-prem pool on the managed cluster instead, and drop onPrem from\n"+
			"this box's declaration.", provider)
	}
	if onPremEnabled {
		if err := requireTailnetCredentials(tailnetName); err != nil {
			return err
		}
	}

	// ONE MANAGEMENT CLUSTER PER BOX, settled here for the same reason: a second
	// one is only detectable before it is built, and a resumed bootstrap skips
	// completed phases so a check inside a phase never runs on the attempt that
	// matters.
	//
	// The provider's inventory is the authority and the only thing asked. A
	// declaration in the repository and an escrow artefact both outlive the
	// cluster by design, and an unreachable Kubernetes API is the state of a
	// wedged cluster whose servers are still running -- the worst possible moment
	// to conclude that nothing is there and build a second control plane.
	// The box's own credential, read once, used by the gate below and by every
	// provisioning step after it. One value from one source: a gate that asked a
	// different credential than the provisioning uses would be answering about
	// different infrastructure.
	providerToken, err := resolveHCloudToken(ctx, gitopsDir, clusterName)
	if err != nil {
		return err
	}

	admission, err := bootstrap.ManagementAdmission{
		GitopsDir: gitopsDir,
		Creating:  clusterName,
		Owned:     providerOwnedCounts(providerToken),
		// Box-wide: an incarnation whose directory was removed still owns its
		// servers, and the repository's history is the box's own record of
		// having declared it.
		Incarnations: tenant.IncarnationsEverDeclared,
	}.Admit(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("   Box: %s\n", admission.BoxID)
	if admission.Resuming > 0 {
		// Named rather than silent: this run is continuing an attempt that left
		// infrastructure behind, and a tenant who expected a clean build should
		// see that it is not one.
		fmt.Printf("   Resuming: %s already owns %d provider resource(s) from an earlier attempt\n",
			clusterName, admission.Resuming)
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

	// The box's own declarations (registry/clusters/<mgmt>/values.yaml) must be
	// readable and complete before any cluster work: Day-0 reads them later, and
	// failing there would leave a half-built cluster.
	if _, err := bootstrap.ReadBoxValues(gitopsDir, clusterName); err != nil {
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
		// What admission counted, not counted again. See
		// Orchestrator.ProviderResourcesOwned.
		ProviderResourcesOwned: admission.Resuming,
		KeepBootstrap:          keepBootstrap,
		MergeKubeconfig:        mergeKubeconfig,
		Debug:                  debug,
		Topology:               topology,
		Gating:                 gatingMode,
		GitopsDir:              gitopsDir,
		OIDCIssuerURL:          oidcIssuerURL,
		OIDCClientID:           oidcClientID,
		Encryption:             encryptionMode,
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
