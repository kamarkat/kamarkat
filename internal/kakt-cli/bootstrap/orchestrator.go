package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/platform"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/cluster"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/capi"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/clusterclass"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/components"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/preflight"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"gopkg.in/yaml.v3"
)

// Orchestrator runs the 17-phase hub cluster bootstrap pipeline.
//
// The pipeline is identical for every provider — local or cloud. The
// orchestrator never branches on provider name or type. Cloud-specific
// phases (5-7) are identity/no-ops for local providers by design.
//
//	Phase  1: preflight               Provider.PreflightValidators()
//	Phase  2: bootstrap-create        kind cluster (orchestrator-owned)
//	Phase  3: day0-infra              Provider.ProvisionDayZero(kubeconfig)
//	Phase  4: capi-init               CAPI operator + Provider.OnCAPIInit()
//	Phase  5: cluster-provision       Provider.ProvisionManagementCluster(cfg)
//	Phase  6: pivot-move              Provider.PivotMove(cfg) → mgmtKubeconfig
//	Phase  7: pivot-ready             Provider.PivotReady(mgmtKubeconfig)
//	Phase  8: cleanup                 delete kind unless --keep-bootstrap
//	Phase  9: clusterclass            ClusterClass deploy (orchestrator-owned)
//	Phase 10: platform-pre-reqs       Provider.OnPlatformPreReqs(kubeconfig)
//	Phase  5a: argocd-install        ArgoCD + seed Application (pre-worker, hybrid)
//	Phase 11a: boundary-01            infra operators ready (orchestrator-owned)
//	Phase 11b: generate-local-secrets Static Secrets (crypto, postgres connection, infisical-db-credentials)
//	Phase 11c: boundary-02            Data workloads (CNPG, Redis)
//	Phase 11d: inject-ca-cert         Wait for CNPG Ready → inject DB_ROOT_CERT into infisical-secrets
//	Phase 11e: boundary-03            Services (Infisical, hub Gateway, apps)
//	Phase 11f: bootstrap-infisical-api Wait for Infisical health → bootstrap Org/Project/MI → store credentials
//	           adr045-commit          Commit + push the generated artifacts
//	Phase 11g: boundary-04            Tenant services (READS the artifacts above, through git)
//	Phase 11h: boundary-05            Fleet provisioning (ADR-047)
//	Phase 11i: boundary-06            Public tenant TLS (ADR-051)
//	Phase 11j: adr045-verify          Wait for the consuming Applications to reconcile
//	Phase 12: finalize                Provider.Finalize(cfg) → kubeconfigPath
//
// The commit sits between the phase that WRITES the generated artifacts and the
// first that reads them. It is listed because it was not: this map ran 11f
// straight into 12, the commit and the three boundaries between them were absent
// from it, and the commit was consequently ordered after its own consumers --
// two phases both labelled 11g, which is what that looks like in code.
type Orchestrator struct {
	Provider    Provider
	ClusterName string

	// OIDCIssuerURL and OIDCClientID name the identity provider this box's API
	// server accepts tokens from (ADR-076). Empty on a first build; supplied on a
	// rebuild of an environment whose issuer already runs, to avoid provisioning
	// without them and replacing the control plane afterwards.
	OIDCIssuerURL string
	OIDCClientID  string

	// GitopsDir is a checkout of the tenant's own repository.
	//
	// Set, the cluster is seeded with the Application that repository declares
	// (ADR-072), and the tenant's values are what the cluster runs on. Empty, the
	// CLI renders a seed itself, which is the platform's own development path and
	// the reason ADR-062's claim that the tenant owns its values was untrue in
	// practice: two seeds install the same bundle and disagree about who supplies
	// its configuration.
	GitopsDir        string
	BootstrapContext string
	KeepBootstrap    bool
	MergeKubeconfig  bool
	Debug            bool
	EnvironmentSlug  string
	Topology         string
	// Gating selects how the cluster is created (ADR-055). Empty means
	// sequenced: converged creation is requested by name or it does not occur.
	Gating GatingMode

	// Encryption selects at-rest encryption for this box (ADR-003 section 6).
	// Empty Encryption means secretbox.
	Encryption cluster.EncryptionMode
	// ProviderName names the provider when no Provider is constructed. A
	// bootstrap always has one; operations that only restate Day-0 artifacts on
	// an existing cluster need the name and nothing else, and building a whole
	// provider to read one string would mean standing up cloud credentials to
	// apply a manifest.
	ProviderName string

	// BundleVersionOverride replaces the version a cluster already records,
	// instead of preserving it.
	//
	// Empty means the normal rule: the cluster owns its version and a reseed
	// leaves it alone (ADR-064), so an operator's binary cannot silently return
	// a promoted cluster to an older bundle.
	//
	// Set, it is an explicit act and is reported as one. This is not the
	// promotion mechanism ADR-064 describes -- that is a pull request against
	// the tenant's repository, and the platform never causes it to take effect.
	// This moves the platform's own cluster during development, where the
	// alternative is rebuilding a cluster to change one string.
	BundleVersionOverride string

	// InstanceRepoURL is this box's instance repository (ADR-062): where its
	// cluster declarations and per-tenant values live. Empty means take it from
	// the gitops repository Day-0 is running inside; there is no other source.
	InstanceRepoURL string
}

// providerName returns the provider's name from whichever of the two fields is
// populated. Provider wins: when a bootstrap has constructed one, it is the
// authority on its own name.
func (o *Orchestrator) providerName() string {
	if o.Provider != nil {
		return o.Provider.Name()
	}
	return o.ProviderName
}

// resolveScriptPath resolves a script path relative to the zero-ops checkout.
// Bootstrap runs from the tenant's workspace, not the checkout, so relative
// paths like "scripts/hybrid/..." would fail. This mirrors hcloudTokenPaths:
// ZERO_OPS_DIR first, then the executable's directory.
func resolveScriptPath(rel string) string {
	if root := strings.TrimSpace(os.Getenv("ZERO_OPS_DIR")); root != "" {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			return filepath.Join(root, rel)
		}
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			dir := filepath.Dir(exe)
			for _, base := range []string{dir, filepath.Dir(dir)} {
				candidate := filepath.Join(base, rel)
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}
	return rel
}

// Run executes the full 17-phase bootstrap pipeline with checkpoint/restart.
func (o *Orchestrator) Run(ctx context.Context) error {
	if o.Debug {
		fmt.Println("[DEBUG] Orchestrator.Run() started")
		fmt.Printf("[DEBUG] Provider: %s, ClusterName: %s\n",
			o.Provider.Name(), o.ClusterName)
	}

	if o.Provider != nil {
		if cp, ok := o.Provider.(*CloudProvider); ok {
			cp.encryption = o.Encryption
		}
	}

	// A tenant's bootstrap keeps its state in the tenant's repository, so a run
	// resumed on a fresh runner picks up where the last one stopped (ADR-072).
	stateMgr := state.NewStateManager(o.ClusterName)
	if o.GitopsDir != "" {
		stateMgr = state.NewTenantStateManager(o.GitopsDir, o.ClusterName)
	}
	bs, err := stateMgr.Load()
	if err == nil && bs != nil {
		return o.handleExistingState(ctx, stateMgr, bs)
	}

	// Guard: kind cluster must not already exist on fresh bootstrap
	if err := o.checkKindClusterExists(); err != nil {
		return err
	}

	return o.runFresh(ctx, stateMgr, nil)
}

// ──────────────────────────────────────────────────────────────────────────
// Fresh bootstrap (or resume with existing state)
// ──────────────────────────────────────────────────────────────────────────

func (o *Orchestrator) runFresh(ctx context.Context, stateMgr *state.StateManager, existing *state.BootstrapState) error {
	bs := existing
	if bs == nil {
		bs = &state.BootstrapState{
			Version:      "1.0",
			ClusterName:  o.ClusterName,
			Provider:     o.Provider.Name(),
			CurrentPhase: state.PhasePreFlight,
		}
	}

	// ── Phase 1: Preflight (checkpointed per-bootstrap) ───────────────
	// Infra preflight (docker/kind/hetzner token) — checkpointed via state/<cluster>.json.
	// Shell static preflight (59 checks, manifests) lives in hub-bootstrap.sh and is
	// hash-gated there; this Go phase is infra-only. Once passed, skipped on resume
	// via runPhase's phaseDone check. Dry-run bypasses the orchestrator entirely.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhasePreFlight, "preflight",
		"Running infra preflight validation (docker/kind/token)...",
		func() error {
			if err := o.checkPlacementCapacityRequested(); err != nil {
				return err
			}
			r := preflight.NewRunner()
			for _, v := range o.Provider.PreflightValidators() {
				r.Add(v)
			}
			return r.Run(ctx)
		},
		func() { fmt.Println("[preflight] ✓ Infra checks passed") },
	); err != nil {
		return err
	}

	// ── Phase 2: Bootstrap cluster (kind) ─────────────────────────────
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBootstrapCreate, "bootstrap-create",
		"Creating ephemeral bootstrap cluster...",
		func() error { return o.createKindCluster(ctx, bs) },
		nil); err != nil {
		return fmt.Errorf("bootstrap create failed: %w", err)
	}

	kubeconfig, bootstrapCtx, err := o.kubeconfigPaths(bs)
	if err != nil {
		return err
	}

	// ── Phase 3: Day-0 infrastructure ─────────────────────────────────
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseDayZero, "day0-infra",
		"Applying provider-specific Day-0 infrastructure...",
		func() error { return o.Provider.ProvisionDayZero(ctx, kubeconfig) },
		func() { fmt.Println("[day0-infra] ✓ Day-0 infrastructure applied") },
	); err != nil {
		return err
	}

	// ── Phase 4: CAPI initialization ──────────────────────────────────
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseCAPIInit, "capi-init",
		"Installing cluster-api-operator...",
		func() error { return o.installCAPI(ctx, kubeconfig, bootstrapCtx) },
		func() { fmt.Println("[capi-init] ✓ CAPI operator installed") },
	); err != nil {
		return err
	}

	// ── Phase 5: Management cluster provisioning ──────────────────────
	provCfg := &ProvisionConfig{
		ClusterName:         o.ClusterName,
		BootstrapKubeconfig: kubeconfig,
		BootstrapContext:    bootstrapCtx,
		Debug:               o.Debug,
		GitopsDir:           o.GitopsDir,
		OIDCIssuerURL:       o.OIDCIssuerURL,
		OIDCClientID:        o.OIDCClientID,
		Encryption:          o.Encryption,
	}
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseClusterProvision, "cluster-provision",
		"",
		func() error { return o.Provider.ProvisionManagementCluster(ctx, provCfg) },
		nil,
	); err != nil {
		return err
	}

	// ── Phase 5b: Home-lab worker join (hybrid) ───────────────────────
	// The hybrid hub runs no Hetzner workers (ADR-046 §19/§21), so its only worker
	// capacity is a home-lab Flatcar node — and it must exist BEFORE anything is
	// installed on the hub.
	//
	// This sits between cluster-provision and pivot-move on purpose. The hub API is
	// already up (the control plane is Ready), but nothing has been deployed to it
	// yet. pivot-move immediately installs cert-manager and the CAPI operators, and
	// the control plane keeps its ADR-014 taint, so with no worker present those
	// pods sit Pending and pivot-move fails on
	// "timed out waiting for the condition on deployments/cert-manager".
	//
	// CAPI cannot provision these nodes (they are Hyper-V VMs on a workstation),
	// which is why this shells out to the provisioning script rather than creating a
	// Machine. For --cluster hub the script mints its own bootstrap token from the
	// hub kubeconfig, so it depends on nothing that pivot installs.
	//
	// The Gateway API CRDs the Cilium operator waits for are delivered with Cilium
	// itself, in the same ClusterResourceSet addon (ADR-041), so the operator
	// settles and this node reaches Ready without anything having to be installed
	// on the cluster first.
	// The postcondition distinguishes "nothing to join" from "nothing joined".
	//
	// Under a provider with no on-prem capacity this phase returns in 46
	// milliseconds having done nothing, which is correct -- and records itself
	// complete, which is correct for THAT provider and wrong for the cluster.
	// Observed: a hybrid box was resumed once as hetzner, this phase logged
	// "Not a home-worker cell — skipping" and was marked done; every later hybrid
	// run then skipped it, and the box sat with one tainted control plane and no
	// worker at all, because the record described a decision made under different
	// flags rather than a state of the cluster.
	//
	// After pivot-move the bootstrap kind cluster is destroyed, so
	// hubKubeconfigFromBootstrap returns "". Fall back to the management
	// kubeconfig that pivot-move recorded — it points at the hub directly.
	resolveHubKubeconfig := func() string {
		if kc := o.hubKubeconfigFromBootstrap(ctx, kubeconfig); kc != "" {
			return kc
		}
		return bs.MgmtKubeconfig
	}
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseOnPremJoin, "on-prem-join",
		"Joining home-lab worker(s) to the hub...",
		func() error { return o.joinHomeWorkers(ctx, resolveHubKubeconfig()) },
		nil,
		withPostcondition(func() error {
			return o.phasePostconditions(resolveHubKubeconfig())[state.PhaseOnPremJoin](ctx)
		}),
	); err != nil {
		return err
	}

	// ── Phase 5c: ArgoCD install + seed ────────────────────────────────
	// Installs ArgoCD and establishes the seed Application (ADR-055).
	//
	// This runs AFTER the worker join, not before it. ArgoCD's server, repo
	// server and application controller are ordinary workloads and belong on a
	// worker; the hub control plane keeps its taint so it can stay dedicated to
	// cluster state. Installing pre-worker would have meant tolerating that
	// taint, which is the wrong answer to a question that no longer needs
	// asking: the Gateway API CRDs now ship with Cilium, so the worker reaches
	// Ready on its own and there is a node to schedule on by the time we get
	// here.
	//
	// Still ahead of pivot-move, so the seed exists before CAPI moves in.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseArgoCDInstall, "argocd-install",
		"Installing ArgoCD + seed on hub...",
		// resolveHubKubeconfig, not a bare hubKubeconfigFromBootstrap: this phase
		// normally runs pre-pivot, where the bootstrap cluster holds the CAPI
		// secret -- but a resumed run that has already pivoted has no bootstrap
		// cluster left, and the lookup correctly returns "". bs.MgmtKubeconfig
		// names the hub directly in that case. Same authority, same reasoning as
		// on-prem-join above; passing the raw lookup here handed installArgoCDAndSeed
		// an empty string, which kubectl tolerates by silently reading $KUBECONFIG
		// while client-go does not -- so ArgoCD installed against one cluster and
		// the GitHub-secret step below it failed with "no configuration has been
		// provided".
		func() error { return o.installArgoCDAndSeed(ctx, resolveHubKubeconfig()) },
		nil,
	); err != nil {
		return err
	}

	// ── Phase 6: Pivot move ───────────────────────────────────────────
	pivotCfg := &PivotConfig{
		ClusterName:         o.ClusterName,
		BootstrapKubeconfig: kubeconfig,
		BootstrapContext:    bootstrapCtx,
		Debug:               o.Debug,
		Encryption:          o.Encryption,
	}

	// Self-heal: pivot-move writes the mgmt kubeconfig on success. A phase
	// marked complete but with no kubeconfig recorded is a torn write from a
	// pre-1.x run (the kubeconfig was only persisted after runPhase saved).
	// Treat it as incomplete so the move re-runs and pivot-ready receives a
	// real path instead of "".
	if bs.MgmtKubeconfig == "" && o.phaseDone(bs, state.PhasePivotMove) {
		fmt.Println("[recovery] pivot-move completed without a recorded mgmt kubeconfig; re-running it")
		bs.CompletedPhases = removePhase(bs.CompletedPhases, state.PhasePivotMove)
	}

	mgmtKubeconfig := bs.MgmtKubeconfig
	if err := o.runPhase(ctx, stateMgr, bs, state.PhasePivotMove, "pivot-move",
		"",
		func() error {
			var err error
			mgmtKubeconfig, err = o.Provider.PivotMove(ctx, pivotCfg)
			if err == nil {
				// Persist the mgmt kubeconfig with the completed phase so a
				// resumed run passes a real path to pivot-ready instead of "".
				bs.MgmtKubeconfig = mgmtKubeconfig
			}
			return err
		},
		nil,
	); err != nil {
		return err
	}
	if mgmtKubeconfig != "" {
		bs.MgmtKubeconfig = mgmtKubeconfig
	}

	// ── Phase 7: Pivot ready ──────────────────────────────────────────
	// The postcondition is the tailnet credential existing ON THE HUB.
	//
	// PivotReady stages it there because `clusterctl move` does not carry a plain
	// Secret across the pivot -- it is written into the bootstrap cluster at
	// capi-init, and the hub gets none. Without it the hub-operator logs
	// "Source secret not found, skipping", never uploads tailscale-authkey to
	// Infisical, and the tailscale-hybrid-psk ExternalSecret sits in
	// SecretSyncedError for ever.
	//
	// Asserted rather than assumed because this phase completed on boxes built
	// before it staged anything, and a completion record written then describes a
	// hub that never received the credential.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhasePivotReady, "pivot-ready",
		"",
		func() error { return o.Provider.PivotReady(ctx, mgmtKubeconfig) },
		nil,
		withPostcondition(func() error {
			return o.phasePostconditions(mgmtKubeconfig)[state.PhasePivotReady](ctx)
		}),
	); err != nil {
		return err
	}

	// ── Phase 8: Cleanup bootstrap cluster ────────────────────────────
	if !o.KeepBootstrap {
		fmt.Println("\n[cleanup] Deleting bootstrap cluster...")
		kindMgr := &KindManager{ClusterName: o.ClusterName}
		if err := kindMgr.Delete(ctx); err != nil {
			fmt.Printf("[cleanup] Warning: failed to delete bootstrap cluster: %v\n", err)
		} else {
			fmt.Println("[cleanup] ✓ Bootstrap cluster deleted")
		}
	}
	// Guarded, unlike every other phase, because this one is not run through
	// runPhase and so nothing else checks whether it was already recorded. It
	// appended an entry on every resume: the acme-hub state reached six "cleanup"
	// records interleaved with the phases they were recorded between, which is
	// unbounded growth in a file committed to the tenant's repository and makes
	// the phase history unreadable exactly when it is being read to explain a
	// failure.
	if !o.phaseDone(bs, state.PhaseCleanup) {
		o.markPhaseComplete(bs, state.PhaseCleanup)
		stateMgr.Save(bs)
	}

	// ── Phase 9: ClusterClass deployment ──────────────────────────────
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseClusterClassDeploy, "clusterclass-deploy",
		"Deploying ClusterClass library...",
		func() error {
			ccd := &clusterclass.Deployer{
				Kubeconfig: mgmtKubeconfig,
				Namespace:  constants.NamespaceCAPI,
				ClassPaths: o.Provider.ClusterClassPaths(),
			}
			return ccd.Deploy(ctx)
		},
		func() { fmt.Println("[clusterclass-deploy] ✓ ClusterClass library deployed") },
	); err != nil {
		return err
	}

	// ── Phase 10: Platform pre-requisites ─────────────────────────────
	if err := o.runPhase(ctx, stateMgr, bs, state.PhasePlatformPreReqs, "platform-pre-reqs",
		"Applying platform pre-requisites...",
		func() error { return o.Provider.OnPlatformPreReqs(ctx, mgmtKubeconfig) },
		func() { fmt.Println("[platform-pre-reqs] ✓ Platform pre-requisites applied") },
	); err != nil {
		return err
	}

	// ── Phase 11a: Boundary 01 — platform infrastructure ──────────────
	// Installs ArgoCD, CNPG operator, Crossplane, ESO, cert-manager, and
	// other core operators. Waits for webhooks, CRDs, and operator pods
	// before proceeding.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBoundary01, "boundary01",
		"Deploying platform infrastructure (boundary 01)...",
		func() error { return o.deployBoundaryN(ctx, mgmtKubeconfig, 1) },
		func() { fmt.Println("[boundary01] ✓ Platform infrastructure deployed") },
	); err != nil {
		return err
	}

	// ── Phase 11b: Generate local secrets ─────────────────────────────
	// Generates cryptographic keys (ENCRYPTION_KEY, AUTH_SECRET, REDIS_URL),
	// creates infisical-secrets, infisical-redis-credentials, and
	// infisical-db-credentials. The latter MUST exist before CNPG's initdb runs
	// in B02, so this phase runs between B01 and B02.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseGenerateLocalSecrets, "generate-local-secrets",
		"Generating local bootstrap secrets...",
		func() error {
			ci := &components.Installer{Kubeconfig: mgmtKubeconfig, EnvironmentSlug: o.EnvironmentSlug, GitopsDir: o.GitopsDir, ClusterName: o.ClusterName}
			if err := ci.GenerateLocalSecrets(ctx); err != nil {
				return err
			}
			// The optional credentials, here rather than later: the hub-operator
			// uploads them to Infisical on its first reconcile, and a Secret that
			// appears afterwards waits for the next one. Nothing created these at
			// all until now, so the mappings that consume them retried forever --
			// five ExternalSecrets on every box, and grafana-alloy never starting.
			return ci.InstallPlatformCredentials(ctx)
		},
		func() { fmt.Println("[generate-local-secrets] ✓ Local secrets generated") },
	); err != nil {
		return err
	}

	// ── Phase 11c: Boundary 02 — platform data workloads ─────────────
	// Deploys the CNPG Cluster and Redis. The CNPG Cluster
	// CR triggers the operator (installed in B01). infisical-db-credentials was
	// created in the previous phase (generate-local-secrets), so CNPG's
	// initdb has the credentials it needs immediately.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBoundary02, "boundary02",
		"Deploying platform data (boundary 02)...",
		func() error { return o.deployBoundaryN(ctx, mgmtKubeconfig, 2) },
		func() { fmt.Println("[boundary02] ✓ Platform data deployed") },
	); err != nil {
		return err
	}

	// ── Phase 11d: Inject CNPG CA certificate ────────────────────────
	// Waits for CNPG Cluster to be Ready, reads infisical-db-ca, and
	// injects DB_ROOT_CERT into infisical-secrets. This MUST run after
	// B02 (CNPG Cluster applied) and before B03 (Infisical deployed)
	// so that Infisical Helm chart renders with DB_ROOT_CERT present,
	// enabling TLS connectivity on first boot.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseInjectCACert, "inject-ca-cert",
		"Injecting CNPG CA certificate into infisical-secrets...",
		func() error {
			ci := &components.Installer{Kubeconfig: mgmtKubeconfig, EnvironmentSlug: o.EnvironmentSlug, GitopsDir: o.GitopsDir, ClusterName: o.ClusterName}
			return ci.UpdateInfisicalSecretsWithCNPGCert(ctx)
		},
		func() { fmt.Println("[inject-ca-cert] ✓ CNPG CA certificate injected") },
	); err != nil {
		return err
	}

	// ── Phase 11e: Boundary 03 — platform services ───────────────────
	// Deploys the hub Gateway, API gateway, Infisical, and spoke
	// cluster configs. Infisical Helm chart starts with ALL secrets
	// already present (infisical-secrets includes DB_ROOT_CERT from the
	// previous phase), preventing CreateContainerConfigError deadlocks.
	// Routes are applied after the Gateway exists, so they have a parent to attach to
	// webhook is guaranteed up.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBoundary03, "boundary03",
		"Deploying platform services (boundary 03)...",
		func() error { return o.deployBoundaryN(ctx, mgmtKubeconfig, 3) },
		func() { fmt.Println("[boundary03] ✓ Platform services deployed") },
	); err != nil {
		return err
	}

	// ── Phase 11f: Bootstrap Infisical API ───────────────────────────
	// Waits for Infisical to be healthy, then bootstraps the Infisical
	// REST API (Org, Project, Machine Identity), creates the infisical-auth
	// Secret, and stores Layer 1+2 credentials in Infisical vault.
	// This MUST run after B03 when Infisical pods are Running.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBootstrapInfisicalAPI, "bootstrap-infisical-api",
		"Bootstrapping Infisical API...",
		func() error {
			ci := &components.Installer{Kubeconfig: mgmtKubeconfig, EnvironmentSlug: o.EnvironmentSlug, GitopsDir: o.GitopsDir, ClusterName: o.ClusterName}
			return ci.BootstrapInfisicalAPI(ctx)
		},
		func() { fmt.Println("[bootstrap-infisical-api] ✓ Infisical API bootstrapped") },
	); err != nil {
		return err
	}

	// The seed carries the Infisical PKI coordinates as chart values, and they
	// are only knowable once the phase above has created the project and the
	// machine identity. The seed was applied before boundary 01, five phases
	// earlier, so at that point they were empty and the security component
	// rendered without its ClusterIssuers.
	//
	// Re-applying the seed here supplies them. The renderer reads them back from
	// the infisical-auth Secret, so this needs no new state threaded through the
	// pipeline, and applying the same manifest twice is what ReapplySeed exists
	// for.
	//
	// This replaces a Kustomize patch the previous phase wrote into the
	// repository and committed. That arrangement appeared to work only because
	// the committed file survived between runs: a fresh cluster reconciled the
	// PREVIOUS cluster's project id until the phase above overwrote it, so the
	// issuers existed early and were wrong rather than absent.
	// The PKI coordinates reach the cluster through the tenant's declared state,
	// not through the seed. Before the re-apply, so the same run that learns them
	// records them.
	if err := o.writeFleetPKIValues(ctx, mgmtKubeconfig); err != nil {
		return err
	}

	if err := o.ReapplySeed(ctx, mgmtKubeconfig); err != nil {
		return fmt.Errorf("re-apply seed with Infisical coordinates: %w", err)
	}
	fmt.Println("[bootstrap-infisical-api] ✓ seed re-applied with PKI coordinates")

	// ── Commit the generated artifacts ────────────────────────────────
	//
	// HERE, not after the boundaries.
	//
	// Everything above this line wrote artifacts into the tenant's repository.
	// Boundary 04 is the first phase that READS them, and it reads them the only
	// way anything in a GitOps box can: through git, rendered by ArgoCD. The
	// commit used to run after boundaries 04, 05 and 06, which meant boundary 04
	// waited on values that were sitting uncommitted on disk a few lines above
	// it. hub-bootstrap-config rendered INFISICAL_PROJECT_ID="", the hub-operator
	// shut down waiting for it and crash-looped, DatabaseRolesProvisioned was
	// never published, and the gate timed out after 45 minutes on a condition
	// that could not arrive. A fresh box could not pass it, ever -- and no run
	// had ever reached this phase to find out.
	//
	// The verification that the consuming Applications reconciled stays where it
	// was, after the boundaries. An artifact is complete when it is written, but
	// the Applications that consume it are not deployed until later, so that is
	// the earliest point the question can be asked.
	if err := o.commitGeneratedArtifacts(ctx); err != nil {
		return err
	}

	// ── Phase 11g: Boundary 04 — tenant services ──────────────────────
	//
	// The postcondition is the identity provider serving, not the boundary having
	// been applied. Everything downstream -- iam-admin-pat, the identity token,
	// identity-service-credentials, kube-sbt -- is waiting on Zitadel, so a phase
	// that reports success while Zitadel is down moves the failure two namespaces
	// away from its cause and takes eleven minutes to surface there.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBoundary04, "boundary04",
		"Deploying tenant services (boundary 04)...",
		func() error { return o.deployBoundaryN(ctx, mgmtKubeconfig, 4) },
		func() { fmt.Println("[boundary04] ✓ Tenant services deployed") },
		withPostcondition(func() error {
			return o.phasePostconditions(mgmtKubeconfig)[state.PhaseBoundary04](ctx)
		}),
	); err != nil {
		return err
	}

	// ── Phase 11h: Boundary 05 — fleet provisioning (ADR-047) ────────────
	// Deploys the tenant-fleet provisioning ApplicationSets (xr / spoke /
	// workloads). Requires boundary04 (tenant services + ArgoCD fleet-registry
	// auth) to be in place first.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBoundary05, "boundary05",
		"Deploying fleet provisioning (boundary 05)...",
		func() error { return o.deployBoundaryN(ctx, mgmtKubeconfig, 5) },
		func() { fmt.Println("[boundary05] ✓ Fleet provisioning deployed") },
	); err != nil {
		return err
	}

	// ── Phase 11i: Boundary 06 — public tenant TLS (ADR-051) ─────────────
	// Deploys the tenant-public-tls ApplicationSet: per-tenant public ACME
	// Certificates + the :443 tenant-tls-gateway in platform-ops on each spoke,
	// rendered from fleet-registry public.hosts declarations. Requires
	// boundary05 (fleet provisioning) so fleet values are already reconciling.
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseBoundary06, "boundary06",
		"Deploying public tenant TLS (boundary 06)...",
		func() error { return o.deployBoundaryN(ctx, mgmtKubeconfig, 6) },
		func() { fmt.Println("[boundary06] ✓ Public tenant TLS deployed") },
	); err != nil {
		return err
	}

	// ── Phase 11j: Verify the ADR-045 consumers reconciled ───────────
	// The artifacts were committed and pushed before boundary 04, which is the
	// first phase that reads them. What is left to establish is that the
	// Applications consuming them actually converged -- which could not be asked
	// earlier, because those Applications are deployed by the boundaries above.
	// Poll ArgoCD apps for health. The apps will not reconcile until
	// the generated files are in Git (committed + pushed). If the
	// auto-commit succeeded, only a git push is needed.
	// Taken from the registry's consumedBy, not listed here. A second literal
	// beside the declarations is what produced this phase's other failure: the
	// registry stopped naming security/generated and a hard-coded copy did not,
	// so the step tried to stage a directory that no longer exists.
	//
	// platform-security-infra was in that literal and is not in the registry: its
	// Infisical coordinates arrive as chart values on the re-applied seed rather
	// than as a committed patch, so waiting for it to reflect a commit waited for
	// something that could never happen.
	appsToWait, err := o.adr045Consumers()
	if err != nil {
		return fmt.Errorf("[adr045-commit] %w", err)
	}
	fmt.Printf("[adr045-commit] Waiting for ArgoCD apps to reconcile: %v\n", appsToWait)
	fmt.Println("[adr045-commit] This requires the generated files to be committed AND pushed.")
	fmt.Println("[adr045-commit] If auto-push failed, push manually.")

	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	if err := o.waitForArgoCDAppsHealthy(waitCtx, mgmtKubeconfig, appsToWait); err != nil {
		// Print diagnostic info even on timeout
		fmt.Println("[adr045-commit] ❌ Some ArgoCD apps did not become healthy.")
		fmt.Println("[adr045-commit] Diagnostic commands:")
		for _, app := range appsToWait {
			fmt.Printf("  kubectl describe application %s -n platform-ops\n", app)
		}
		return fmt.Errorf("[adr045-commit] %w", err)
	}
	fmt.Println("[adr045-commit] ✓ All ArgoCD apps reconciled successfully")

	// ── Phase 12: Finalize ────────────────────────────────────────────
	var kubeconfigPath string
	if err := o.runPhase(ctx, stateMgr, bs, state.PhaseFinalize, "finalize",
		"Finalizing bootstrap...",
		func() error {
			var err error
			kubeconfigPath, err = o.Provider.Finalize(ctx, &FinalizeConfig{
				MgmtKubeconfig:  mgmtKubeconfig,
				ClusterName:     o.ClusterName,
				Namespace:       constants.NamespaceCAPI,
				MergeKubeconfig: o.MergeKubeconfig,
				Debug:           o.Debug,
			})
			return err
		},
		nil,
	); err != nil {
		return err
	}

	// ── Completion ────────────────────────────────────────────────────
	bs.CompletedPhases = append(bs.CompletedPhases, state.PhaseComplete)
	bs.CurrentPhase = state.PhaseComplete
	completed := time.Now().UTC()
	bs.CompletedAt = &completed
	stateMgr.Save(bs)

	o.printTimingSummary(bs)

	// finalize ASSIGNS kubeconfigPath, and a resumed run skips finalize.
	//
	// The variable is written inside that phase's closure, so on any run where
	// the phase is already complete the closure never executes and the path stays
	// empty -- printing "Kubeconfig:" with nothing after it and handing the
	// operator `kubectl --kubeconfig= get nodes`, which is not a command.
	//
	// The state file has carried the answer since the pivot wrote it, so the
	// banner reads from there rather than depending on which phases happened to
	// run this time.
	if kubeconfigPath == "" {
		kubeconfigPath = bs.MgmtKubeconfig
	}

	argoCDPwd := o.getArgoCDPassword(ctx, mgmtKubeconfig)
	fmt.Println("\n✓ Hub Cluster bootstrap complete!")
	fmt.Printf("  Provider: %s\n", o.Provider.Name())
	fmt.Printf("  Cluster Name: %s\n", o.ClusterName)
	fmt.Printf("  Kubeconfig: %s\n", kubeconfigPath)
	fmt.Printf("  ArgoCD Password: %s\n", argoCDPwd)
	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Verify cluster: kubectl --kubeconfig=%s get nodes\n", kubeconfigPath)
	fmt.Println("  2. Access ArgoCD UI (username: admin)")
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Phase runner
// ──────────────────────────────────────────────────────────────────────────

// runPhase executes a single bootstrap phase with checkpoint persistence.
// If the phase is already completed, it skips. On success, it saves state
// and advances to the next phase.
// phaseSpec carries the optional properties of a phase. Options rather than
// parameters because every phase has an action and almost none has anything
// else, and a signature that grew a field per phase would make twenty-one call
// sites carry nils for a property one of them uses.
type phaseSpec struct {
	postcondition func() error
}

type phaseOption func(*phaseSpec)

// withPostcondition declares what must be TRUE for this phase to count as done,
// as distinct from what the phase DOES.
//
// A completion record says a phase ran to the end of its action once, under
// whatever assertions that binary carried. It does not say the phase's effect is
// still in place, and it cannot say the phase was checked against assertions
// added since. Boundary 04 is the case that proved the difference: it was
// recorded complete by a binary that asserted only that ArgoCD had generated the
// Applications, so every later run skipped it -- including the runs whose whole
// purpose was the Zitadel repair -- while the identity provider had never once
// served. The bootstrap printed "complete" twenty-four phases deep over a box
// with no working identity provider.
//
// So a phase that declares a postcondition is re-checked on resume, and a
// completion record that no longer holds is withdrawn rather than trusted. The
// check is a point-in-time probe, never a wait: resume must stay cheap, and the
// waiting belongs to the action that runs if the probe says the phase is unfinished.
//
// Adoption is per phase. A phase that declares none behaves exactly as before,
// so this can be taken up where it earns its keep instead of all at once.
func withPostcondition(check func() error) phaseOption {
	return func(s *phaseSpec) { s.postcondition = check }
}

func (o *Orchestrator) runPhase(
	ctx context.Context,
	stateMgr *state.StateManager,
	bs *state.BootstrapState,
	phase state.BootstrapPhase,
	label, header string,
	action func() error,
	onSuccess func(),
	opts ...phaseOption,
) error {
	var spec phaseSpec
	for _, opt := range opts {
		opt(&spec)
	}

	if o.phaseDone(bs, phase) {
		if spec.postcondition == nil {
			fmt.Printf("[%s] ✓ Skipped (already completed)\n", label)
			return nil
		}
		if err := spec.postcondition(); err == nil {
			fmt.Printf("[%s] ✓ Skipped (already completed, postcondition holds)\n", label)
			return nil
		} else {
			fmt.Printf("[%s] ⚠ recorded complete, but its postcondition does not hold: %v\n", label, err)
			fmt.Printf("[%s]   withdrawing the completion record and running the phase again\n", label)
			bs.CompletedPhases = removePhase(bs.CompletedPhases, phase)
			if saveErr := stateMgr.Save(bs); saveErr != nil {
				return saveErr
			}
		}
	}
	if header != "" {
		fmt.Println("\n[" + label + "] " + header)
	}
	if o.Debug {
		fmt.Printf("[DEBUG] Phase: %s\n", phase)
	}

	// Timed here because runPhase is the single choke point every phase passes
	// through, so no phase can be added later and silently escape accounting.
	started := time.Now().UTC()
	if err := action(); err != nil {
		fmt.Printf("[%s] ✗ failed after %s\n", label, formatDuration(time.Since(started)))
		return fmt.Errorf("[%s] %w", label, err)
	}
	elapsed := time.Since(started)

	if onSuccess != nil {
		onSuccess()
	}
	fmt.Printf("[%s] ⏱  %s\n", label, formatDuration(elapsed))

	bs.PhaseTimings = append(bs.PhaseTimings, state.PhaseTiming{
		Phase:       phase,
		StartedAt:   started,
		CompletedAt: started.Add(elapsed),
		Seconds:     elapsed.Round(time.Millisecond).Seconds(),
	})
	o.markPhaseComplete(bs, phase)
	if err := stateMgr.Save(bs); err != nil {
		return err
	}
	return o.persistTenantState(ctx, label)
}

// persistTenantState pushes the bootstrap state to the tenant's repository after
// a phase completes.
//
// Every phase, not once at the end. The state file already updates on disk at
// this cadence; on a CI runner that disk is discarded when the job ends, so a
// state committed only after the last phase would leave a run that failed at
// cluster-provision restarting from nothing -- and provisioning a cluster twice
// is fifteen minutes and a second set of nodes.
//
// Failure is reported and does not stop the bootstrap. A phase that succeeded
// has succeeded, and abandoning the run because its bookkeeping could not be
// pushed would turn a recoverable state into a lost one. What it costs is that
// the next run repeats from the last phase that did persist, which is the
// behaviour without this at all.
func (o *Orchestrator) persistTenantState(ctx context.Context, label string) error {
	if o.GitopsDir == "" {
		return nil
	}

	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = o.GitopsDir
		return cmd.CombinedOutput()
	}

	if out, err := git("add", state.TenantStateDir); err != nil {
		fmt.Printf("[%s] ⚠️  could not stage bootstrap state: %v\n%s\n", label, err, out)
		return nil
	}

	// Nothing to commit is the normal case for a phase that changed no state,
	// and `git commit` treats it as an error.
	if out, err := git("diff", "--cached", "--quiet"); err == nil {
		_ = out
		return nil
	}

	if out, err := git("commit", "-m",
		fmt.Sprintf("chore(bootstrap): %s [skip ci]", label)); err != nil {
		fmt.Printf("[%s] ⚠️  could not commit bootstrap state: %v\n%s\n", label, err, out)
		return nil
	}

	// Rebase before pushing: a promotion pull request may have merged while this
	// bootstrap was running, and a rejected push would silently stop persisting
	// state for the rest of the run.
	if out, err := git("pull", "--rebase", "--autostash"); err != nil {
		fmt.Printf("[%s] ⚠️  could not rebase before pushing state: %v\n%s\n", label, err, out)
		return nil
	}
	if out, err := git("push"); err != nil {
		fmt.Printf("[%s] ⚠️  could not push bootstrap state: %v\n%s\n", label, err, out)
	}
	return nil
}

// hubKubeconfigFromBootstrap persists the hub's admin kubeconfig by reading the
// CAPI-generated Secret out of the bootstrap cluster.
//
// Needed because on-prem-join runs BEFORE pivot-move, and pivot-move is what
// normally writes this file. The control plane is up by this point, so the Secret
// already exists.
//
// It writes to k8-secrets/kubeconfig/<cluster>.kubeconfig, not a temp file, and
// the path is also passed to the script through HUB_KUBECONFIG.
//
// Both, because neither alone was enough. home-lab.env assigns HUB_KUBECONFIG
// unconditionally, so the environment variable was discarded; and the value it
// assigns names ONE cluster, so writing to "the canonical path" only agreed with
// it while the hub happened to carry that name. A hub named anything else aborted
// the join with "HUB_KUBECONFIG not found", pointing at a kubeconfig belonging to
// a different and possibly deleted cluster.
//
// provision-flatcar-worker.sh now keeps an explicit HUB_KUBECONFIG across sourcing
// that file, so the variable is what decides. The file write remains because
// pivot-move later rewrites the same path, and because an operator running the
// script by hand has something to point at.
//
// home-lab.env is gitignored (it holds real values), which is why the fix is in
// the script rather than in the file that caused it.
//
// Returns "" if it cannot be read; joinHomeWorkers treats that as fatal rather
// than silently skipping the join.
func (o *Orchestrator) hubKubeconfigFromBootstrap(ctx context.Context, bootstrapKubeconfig string) string {
	// Every failure below used to become the same empty string, and the caller
	// reported one cause for all of them: "could not read the hub kubeconfig from
	// the bootstrap cluster". Five different things produce that, and on
	// 2026-09-18 the actual one was none of them -- kubectl had no context for the
	// bootstrap cluster at all, because a kind cluster left running short-circuits
	// `kind create cluster`, which then writes no kubeconfig entry. The cluster was
	// healthy, the secret was present, and the run stopped saying the secret could
	// not be read. Saying what actually failed costs one line each.
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", bootstrapKubeconfig,
		"-n", constants.NamespaceCAPI,
		"get", "secret", o.ClusterName+"-kubeconfig",
		"-o", "jsonpath={.data.value}",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// [bootstrap] and not a phase name: this is called from on-prem-join and
		// from argocd-install, and a message naming the wrong one sends the reader
		// to the wrong place in the log.
		fmt.Printf("[bootstrap] reading %s-kubeconfig from the bootstrap cluster: %v\n",
			o.ClusterName, err)
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			fmt.Printf("[bootstrap]   kubectl: %s\n", msg)
		}
		// A refused connection to 127.0.0.1 is the kind cluster's API on a port
		// that has moved. kind binds a random host port, so destroying and
		// recreating a cluster of the same name gives it a new one -- and the
		// context in the kubeconfig still names the old. Observed on 2026-09-18:
		// the container was on 62015 while the context said 63123.
		//
		// Named because the remedy is one command and nothing else in the output
		// suggests it, and because the phase that follows -- pivot-move -- cannot
		// fall back the way this caller does. It must reach the bootstrap cluster
		// to move CAPI off it, so a stale port that merely looks cosmetic here
		// stops the run there.
		if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") {
			fmt.Printf("[bootstrap]   The bootstrap cluster's API is not where the kubeconfig says.\n")
			fmt.Printf("[bootstrap]   kind binds a random host port and a recreated cluster gets a\n")
			fmt.Printf("[bootstrap]   new one. Re-point the context and re-run:\n")
			fmt.Printf("[bootstrap]       kind export kubeconfig --name %s\n", o.ClusterName)
		}
		return ""
	}
	if len(out) == 0 {
		fmt.Printf("[bootstrap] secret %s/%s-kubeconfig exists but carries no .data.value\n",
			constants.NamespaceCAPI, o.ClusterName)
		return ""
	}

	decoded, err := base64.StdEncoding.DecodeString(string(out))
	if err != nil {
		fmt.Printf("[bootstrap] %s-kubeconfig is not valid base64: %v\n", o.ClusterName, err)
		return ""
	}

	path := filepath.Join("k8-secrets", "kubeconfig", o.ClusterName+".kubeconfig")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Printf("[bootstrap] cannot create %s: %v\n", filepath.Dir(path), err)
		return ""
	}
	if err := os.WriteFile(path, decoded, 0o600); err != nil {
		fmt.Printf("[bootstrap] cannot write %s: %v\n", path, err)
		return ""
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// hubWorkerSelector identifies a joined home-lab hub worker. provision-flatcar-worker.sh
// applies hub-role=worker for its "hub" target, both via kubelet --node-labels and
// again cluster-side after the join.
//
// Both labels, not either: hub-role=worker says which cluster the node belongs
// to, workload-location=on-prem says it is the tenant's own hardware, and the
// platform's placement rules (ADR-046 §11) require both of them together.
const hubWorkerSelector = "hub-role=worker,workload-location=on-prem"

// joinHomeWorkers brings up the home-lab worker(s) this hub needs before any
// platform workload is deployed. No-op unless the provider asked for home workers.
// checkPlacementCapacityRequested refuses a hybrid bootstrap that has not asked
// for the home workers its own manifests require.
//
// The hybrid environment pins platform-data to nodes labelled
// workload-location=on-prem and node-role.kubernetes.io/worker (ADR-046 §11: both
// selectors are mandatory), and only a home worker carries them. Home workers
// are opt-in, and joinHomeWorkers returns success when they were not asked for,
// so the two settings can disagree and nothing says so.
//
// What that costs is the reason this is a preflight check rather than a comment.
// The bootstrap proceeds for roughly ninety minutes -- provisioning a cluster,
// pivoting, and reconciling two boundaries -- and then stalls at inject-ca-cert
// waiting for a CNPG cluster that reports "Setting up primary" forever, because
// its pod is Pending against a single control-plane node. The message names
// CNPG, the failure is scheduling, and the cause is a flag that was not passed.
func (o *Orchestrator) checkPlacementCapacityRequested() error {
	// On-prem nodes are capacity, whatever the provider.
	if hw, ok := o.Provider.(interface{ OnPremRequested() bool }); ok && hw.OnPremRequested() {
		return nil
	}
	// So are cloud workers. A box with either can schedule; a box with neither
	// cannot, and the control plane does not make up the difference -- it keeps
	// its taint, and every platform workload pins node-role.kubernetes.io/worker,
	// which a control-plane node does not carry (ADR-014).
	if wr, ok := o.Provider.(interface{ PlannedWorkerReplicas() int }); ok && wr.PlannedWorkerReplicas() > 0 {
		return nil
	}
	return fmt.Errorf(
		"this box would have no worker capacity: %s in %s provisions no cloud\n"+
			"workers, and on-prem nodes were not requested.\n\n"+
			"Every platform workload pins node-role.kubernetes.io/worker, which a\n"+
			"control-plane node does not carry, so the cluster would come up with its\n"+
			"control plane Ready and everything else Pending -- roughly ninety minutes\n"+
			"in, at a CNPG cluster that never finishes setting up its primary.\n\n"+
			"Re-run with --on-prem --tailnet-name=<tailnet>, or use an environment\n"+
			"that provisions cloud workers (stg, prod).",
		o.providerName(), o.EnvironmentSlug)
}

func (o *Orchestrator) joinHomeWorkers(ctx context.Context, kubeconfig string) error {
	hw, ok := o.Provider.(interface{ OnPremRequested() bool })
	if !ok || !hw.OnPremRequested() {
		fmt.Println("[on-prem-join] Not a home-worker cell — skipping")
		return nil
	}

	if kubeconfig == "" {
		return fmt.Errorf("could not read the hub kubeconfig from the bootstrap cluster " +
			"(the line above says why);\n" +
			"the home worker cannot join, and pivot would then fail to schedule cert-manager.\n\n" +
			"If kubectl reported no context: a kind cluster left running short-circuits\n" +
			"`kind create cluster`, which then writes no kubeconfig entry, and every\n" +
			"kubectl call afterwards resolves nothing. Restore it and re-run:\n" +
			"    kind export kubeconfig --name <cluster>")
	}

	// Idempotent: provisioning a Flatcar VM takes ~10 minutes, and a resumed
	// bootstrap must not pay that again for a node that is already serving.
	//
	// What "already serving" means is the whole difficulty. Provisioning is
	// destructive -- provision-flatcar-worker.sh deletes the Node object and
	// replaces the VM's disk from the pristine base image before it has
	// established that a rebuild was required -- so anything short of proof that
	// this node needs rebuilding must not reach it.
	switch state, name := o.probeHubWorker(ctx, kubeconfig); state {
	case hubWorkerReady:
		fmt.Printf("[on-prem-join] ✓ %s already Ready — skipping provisioning\n", name)
		return nil

	case hubWorkerUnknown:
		// The hub could not be asked. That is not evidence the node is gone, and
		// it used to read as exactly that: one unreachable API server, or a
		// kubeconfig naming a cluster that had just been pivoted, and the phase
		// went on to destroy a node that was serving perfectly well.
		return fmt.Errorf("the hub's nodes could not be read from %s, so whether the "+
			"on-prem worker is still serving is unknown.\n"+
			"Refusing to provision: that would delete the Node object and rebuild the VM "+
			"from the base image, which is not recoverable if the node was in fact fine.\n"+
			"Check the hub is reachable, then re-run.", kubeconfig)

	case hubWorkerNotReady:
		// Registered but not Ready. A node that has lost its heartbeat usually
		// comes back -- the guest reboots, tailscaled reconnects, the kubelet
		// re-registers -- and the cheap outcome is to let it. Only when it does
		// not is a rebuild the right answer.
		fmt.Printf("[on-prem-join] %s is registered but NotReady — waiting %s for it to recover\n",
			name, formatDuration(onPremRecoveryWindow))
		fmt.Println("[on-prem-join]   (rebuilding is destructive: it deletes the Node and replaces the VM disk)")
		if recovered := o.awaitHubWorkerReady(ctx, kubeconfig, onPremRecoveryWindow); recovered != "" {
			fmt.Printf("[on-prem-join] ✓ %s recovered — skipping provisioning\n", recovered)
			return nil
		}
		fmt.Printf("[on-prem-join] %s did not recover — provisioning it again\n", name)
	}

	script := resolveScriptPath(filepath.Join("scripts", "hybrid", "provision-flatcar-worker.sh"))
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("home-lab worker is required before boundary-01 but %s is missing.\n"+
			"Provision it manually, then re-run:\n"+
			"    ./scripts/hybrid/provision-flatcar-worker.sh --cluster hub", script)
	}

	fmt.Println("[on-prem-join] Running provision-flatcar-worker.sh --cluster hub")
	fmt.Println("[on-prem-join] (Hyper-V VM creation over SSH — this takes several minutes)")

	cmd := exec.CommandContext(ctx, "bash", script, "--cluster", "hub")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "HUB_KUBECONFIG="+kubeconfig)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("home-lab worker provisioning failed: %w\n"+
			"Fix the cause, then re-run this bootstrap — the phase is idempotent and will\n"+
			"skip if the node is already Ready:\n"+
			"    ./scripts/hybrid/provision-flatcar-worker.sh --cluster hub", err)
	}

	// The script has its own Ready gate, but the cluster's view is what the next
	// phase depends on, so confirm it here too.
	if state, name := o.probeHubWorker(ctx, kubeconfig); state == hubWorkerReady {
		fmt.Printf("[on-prem-join] ✓ %s Ready\n", name)
		return nil
	}
	return fmt.Errorf("provisioning reported success but no Ready node carries %s;\n"+
		"platform workloads would have nowhere to schedule (ADR-046 §11)", hubWorkerSelector)
}

// onPremRecoveryWindow is how long a registered-but-NotReady on-prem node is
// given to come back before the phase hands it to the provisioning script.
//
// Short, because this is the cheaper of two waits rather than the whole defence.
// provision-flatcar-worker.sh now has a recovery gate of its own that can do
// what this cannot -- start a VM the workstation left Off -- and waits ~5
// minutes for the node to rejoin before it replaces anything. This window only
// covers the case that needs no intervention at all, a kubelet part-way through
// a restart, so that an ordinary resume does not shell out to the host over a
// node that is seconds from Ready. Anything longer is spent twice.
const onPremRecoveryWindow = 2 * time.Minute

// awaitHubWorkerReady waits for a registered on-prem node to report Ready,
// returning its name, or "" if the window passes without it.
//
// A hub that stops answering ends the wait rather than consuming it: the point
// of waiting is to avoid a needless rebuild, and a probe that cannot reach the
// hub is not evidence for one either way.
func (o *Orchestrator) awaitHubWorkerReady(ctx context.Context, kubeconfig string, window time.Duration) string {
	const every = 15 * time.Second
	deadline := time.Now().Add(window)
	for {
		switch state, name := o.probeHubWorker(ctx, kubeconfig); state {
		case hubWorkerReady:
			return name
		case hubWorkerAbsent:
			// It was registered a moment ago and is not now, so something deleted
			// it. Waiting cannot bring back a Node object nobody is writing.
			return ""
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(every):
		}
	}
}

// hubWorkerState is what one probe of the hub can say about on-prem capacity.
//
// Three outcomes rather than a bool, because the phase must do something
// different for each and two of them used to collapse into the same `false`:
// a node that is registered but NotReady is a node to WAIT for, and a hub that
// cannot be asked is not evidence of anything at all. Both used to mean
// "provision", and provisioning destroys the VM and its disk before it knows
// whether a rebuild was needed -- so a stalled kubelet, or one unreachable
// API server, cost a working node and ten minutes.
type hubWorkerState int

const (
	hubWorkerUnknown  hubWorkerState = iota // the hub could not be asked
	hubWorkerAbsent                         // nothing carries the labels
	hubWorkerNotReady                       // registered, not Ready
	hubWorkerReady
)

// probeHubWorker is the single question both the phase and its postcondition
// ask, so that they cannot disagree.
//
// They used to ask two: the postcondition matched workload-location=on-prem and
// ignored readiness, the phase matched hub-role=worker and required Ready. A
// NotReady node therefore satisfied the postcondition and failed the phase's own
// gate -- so runPhase skipped the phase as "already completed, postcondition
// holds" over a cluster whose only worker could not schedule anything, which is
// the ADR-046 §11 failure the postcondition exists to catch.
//
// provision-flatcar-worker.sh applies both labels together (kubelet
// --node-labels, and again cluster-side after the join), so requiring both is
// the same claim either of them made alone.
func (o *Orchestrator) probeHubWorker(ctx context.Context, kubeconfig string) (hubWorkerState, string) {
	out, err := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"get", "nodes", "-l", hubWorkerSelector,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"=\"}"+
			"{.status.conditions[?(@.type==\"Ready\")].status}{\"\\n\"}{end}",
	).Output()
	if err != nil {
		return hubWorkerUnknown, ""
	}

	state, name := hubWorkerAbsent, ""
	for _, line := range strings.Split(string(out), "\n") {
		n, status, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || n == "" {
			continue
		}
		if status == "True" {
			return hubWorkerReady, n
		}
		state, name = hubWorkerNotReady, n
	}
	return state, name
}

// formatDuration renders a duration for humans reading a bootstrap log: seconds
// below a minute, m/s above it. time.Duration's own String() gives "7m12.3841s",
// which is noisy in a column.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// printTimingSummary reports where end-to-end cluster creation time actually
// went. Phases are listed slowest-first: the point is to answer "what should we
// optimise", which a chronological list buries.
func (o *Orchestrator) printTimingSummary(bs *state.BootstrapState) {
	if len(bs.PhaseTimings) == 0 {
		return
	}

	var worked time.Duration
	for _, t := range bs.PhaseTimings {
		worked += time.Duration(t.Seconds * float64(time.Second))
	}

	ranked := make([]state.PhaseTiming, len(bs.PhaseTimings))
	copy(ranked, bs.PhaseTimings)
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Seconds > ranked[j].Seconds })

	fmt.Println("\n⏱  Bootstrap timing")
	fmt.Printf("   %-28s %8s  %s\n", "PHASE", "TIME", "SHARE")
	for _, t := range ranked {
		d := time.Duration(t.Seconds * float64(time.Second))
		share := 0.0
		if worked > 0 {
			share = 100 * float64(d) / float64(worked)
		}
		fmt.Printf("   %-28s %8s  %4.1f%%\n", t.Phase, formatDuration(d), share)
	}

	fmt.Printf("   %-28s %8s\n", "phases run this invocation", formatDuration(worked))

	// Wall time spans any gap between a failed run and its resume, so it is
	// reported separately rather than presented as the cost of building a cluster.
	if bs.StartedAt != nil && bs.CompletedAt != nil {
		wall := bs.CompletedAt.Sub(*bs.StartedAt)
		fmt.Printf("   %-28s %8s  (%s → %s)\n", "wall clock, first start → end",
			formatDuration(wall),
			bs.StartedAt.Format("15:04:05"), bs.CompletedAt.Format("15:04:05"))
		if wall > worked+30*time.Second {
			fmt.Println("   note: wall clock exceeds phase time — this bootstrap was resumed,")
			fmt.Println("         so it includes time the process was not running.")
		}
	}
}

func (o *Orchestrator) phaseDone(bs *state.BootstrapState, phase state.BootstrapPhase) bool {
	for _, p := range bs.CompletedPhases {
		if p == phase {
			return true
		}
	}
	return false
}

func (o *Orchestrator) markPhaseComplete(bs *state.BootstrapState, phase state.BootstrapPhase) {
	bs.CompletedPhases = append(bs.CompletedPhases, phase)
	// advance current phase
	bs.CurrentPhase = phase
}

// removePhase returns phases with the given entry removed (no-op if absent).
func removePhase(phases []state.BootstrapPhase, phase state.BootstrapPhase) []state.BootstrapPhase {
	out := phases[:0]
	for _, p := range phases {
		if p != phase {
			out = append(out, p)
		}
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 2: Kind cluster
// ──────────────────────────────────────────────────────────────────────────

func (o *Orchestrator) createKindCluster(ctx context.Context, bs *state.BootstrapState) error {
	if o.BootstrapContext != "" {
		fmt.Printf("[bootstrap-create] Using existing context: %s\n", o.BootstrapContext)
		bs.BootstrapContext = o.BootstrapContext
		return nil
	}

	kindMgr := &KindManager{
		ClusterName: o.ClusterName,
		ConfigPath:  o.Provider.KindConfigPath(),
	}

	if kindMgr.Exists(ctx) {
		fmt.Println("[bootstrap-create] Bootstrap cluster already exists")
	} else {
		if err := kindMgr.Create(ctx); err != nil {
			return fmt.Errorf("failed to create Kind cluster: %w", err)
		}
		fmt.Println("[bootstrap-create] ✓ Kind cluster created")
	}

	bs.BootstrapContext = "kind-" + o.ClusterName

	kubeconfig := defaultKubeconfigPath()

	// The cluster exists; its context must now be resolvable in the file every
	// later phase reads. When it is not, nothing here fails -- kubeconfigPaths
	// returns the unpinned path, kubectl resolves no cluster and falls back to
	// localhost:8080, and the run dies three phases later on a connection
	// refused that names neither the file nor the reason. Checked once, here,
	// where the cause is still visible.
	pinned, err := pinKubeconfig(kubeconfig, bs.BootstrapContext)
	if err != nil {
		return fmt.Errorf("the bootstrap cluster was created but context %q is not "+
			"in %s.\n\nkind writes to $KUBECONFIG when it is set, so an exported "+
			"KUBECONFIG pointing somewhere else puts the context there while every "+
			"kubectl call below reads this file. Unset it and re-run, or export the "+
			"file kind actually wrote to: %w",
			bs.BootstrapContext, kubeconfig, err)
	}

	// The PINNED copy is recorded, never the file it was read from.
	//
	// Recording the source looked equivalent and was not. hub-bootstrap.sh exports
	// KUBECONFIG to k8-secrets/kubeconfig/<cluster>.kubeconfig whenever that file
	// already exists -- so on a resumed run kind merged kind-<cluster> INTO the
	// hub's kubeconfig, the check above passed against it, and that path was
	// recorded. pivot-move then calls SaveKubeconfig, which os.WriteFile()s the
	// same path with the hub's kubeconfig: a whole-file overwrite, not a merge.
	// The context this check just proved was destroyed by a later phase of the
	// same run, and every run afterwards resolved the hub for questions about the
	// bootstrap cluster -- "namespaces platform-capi not found", with a healthy
	// kind cluster sitting right there.
	//
	// pinKubeconfig's output is ours: $TMPDIR/hub-bootstrap-<context>.kubeconfig,
	// named for the context, written by nothing else in the pipeline. Recording a
	// path the pipeline itself rewrites is the defect; recording one it does not
	// touch is the fix.
	bs.BootstrapKubeconfig = pinned

	// Create platform-capi namespace
	nsMgr := &NamespaceManager{
		Kubeconfig: kubeconfig,
		Context:    bs.BootstrapContext,
		Namespace:  constants.NamespaceCAPI,
	}
	if err := nsMgr.Create(ctx); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}
	fmt.Println("[bootstrap-create] ✓ Namespace created: platform-capi")
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 4: CAPI initialization
// ──────────────────────────────────────────────────────────────────────────

func (o *Orchestrator) installCAPI(ctx context.Context, kubeconfig, contextName string) error {
	capiInstaller := &capi.OperatorInstaller{
		Kubeconfig: kubeconfig,
		Context:    contextName,
		Namespace:  constants.NamespaceCAPI,
		Providers:  o.Provider.CAPIProviders(),
		Debug:      o.Debug,
	}
	if err := capiInstaller.Install(ctx); err != nil {
		return fmt.Errorf("failed to install CAPI operator: %w", err)
	}
	return o.Provider.OnCAPIInit(ctx, kubeconfig, contextName, constants.NamespaceCAPI)
}

// ──────────────────────────────────────────────────────────────────────────
// Phase 5a: ArgoCD install + seed (pre-worker)
// ──────────────────────────────────────────────────────────────────────────

// installArgoCDAndSeed installs ArgoCD on the hub and establishes the seed
// Application (ADR-055) before the worker joins. This allows Gateway API CRDs
// to sync via ArgoCD before the Cilium operator init container times out.
//
// Uses the hub kubeconfig extracted from the bootstrap cluster's CAPI secret
// (same mechanism as on-prem-join). The hub API is already up after
// cluster-provision.
func (o *Orchestrator) installArgoCDAndSeed(ctx context.Context, kubeconfig string) error {
	// An empty kubeconfig is refused, never passed on.
	//
	// The two consumers below disagree about what "" means. Every kubectl call
	// treats --kubeconfig "" as unset and falls back to $KUBECONFIG, so ArgoCD
	// installs -- somewhere. client-go's loader, which the GitHub-credential step
	// uses, rejects it: "invalid configuration: no configuration has been
	// provided". The observable result was a phase that reported ArgoCD installed
	// and a boundary activated, then failed two steps later with a message about
	// configuration that named nothing. Refusing here says which lookup failed,
	// before anything has been installed against an unknown cluster.
	if strings.TrimSpace(kubeconfig) == "" {
		return fmt.Errorf("no hub kubeconfig: neither the CAPI secret %s-kubeconfig in "+
			"the bootstrap cluster nor the pivot-recorded management kubeconfig could be "+
			"read. The lines above name which one failed and why", o.ClusterName)
	}

	ci := &components.Installer{Kubeconfig: kubeconfig, EnvironmentSlug: o.EnvironmentSlug, GitopsDir: o.GitopsDir, ClusterName: o.ClusterName}
	if err := ci.InstallArgoCD(ctx); err != nil {
		return fmt.Errorf("failed to install ArgoCD: %w", err)
	}
	fmt.Println("[argocd-install] ✓ ArgoCD installed on hub")

	// ADR-055: establish the seed (boundary AppProjects + the Application that
	// owns all boundary content) and open boundary 01. In sequenced mode the
	// remaining boundaries are created inactive and opened by their own phases.
	if err := o.deployBoundary(ctx, kubeconfig, 1); err != nil {
		return err
	}
	fmt.Println("[argocd-install] ✓ 01-platform-infra boundary activated")

	// ArgoCD needs credentials for the git organisation that owns this box's
	// repositories, before any Application can sync -- otherwise the webhook waits
	// below block on "authentication required".
	//
	// This is NOT the platform's repository. A released box reads none of the
	// platform's git (ADR-063); what it does read is its own instance repository,
	// which boundaries 05 and 06 reconcile and which is private (ADR-062). An
	// earlier version of this skipped the credential entirely on a released run,
	// reasoning that a released box reads nothing from git. That confused "reads
	// nothing of the platform's" with "reads nothing", and would have left tenant
	// fleet sync unable to read the repository it exists to reconcile.
	orgURL, err := o.boxOrgURL()
	if err != nil {
		return err
	}
	if err := o.ensureArgoCDGitHubAuth(ctx, kubeconfig, orgURL); err != nil {
		return fmt.Errorf("failed to configure ArgoCD GitHub access: %w", err)
	}

	// The escrow, before hub-operator starts reconciling. It reads
	// these from a Secret at startup, and a Secret that appears later means every
	// backup until then was skipped -- including the first, which is the one
	// covering the window where the box is least recoverable.
	escrow := &components.Installer{Kubeconfig: kubeconfig, EnvironmentSlug: o.EnvironmentSlug,
		GitopsDir: o.GitopsDir, ClusterName: o.ClusterName}
	if err := escrow.InstallEscrowCredentials(ctx); err != nil {
		return err
	}
	fmt.Println("[argocd-install] ✓ ArgoCD configured + seed established")
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Boundary 01: Platform infrastructure (ArgoCD + operators)
// ──────────────────────────────────────────────────────────────────────────

// ──────────────────────────────────────────────────────────────────────────
// Boundary 02: Platform data workloads (CNPG, Redis)
// ──────────────────────────────────────────────────────────────────────────

// ──────────────────────────────────────────────────────────────────────────
// Boundary 03: Platform services (hub Gateway, platform services)
// ──────────────────────────────────────────────────────────────────────────

// deployBoundaryN runs the boundary's declared contract.
//
// One path for all six. Before this there were six hand-written functions, and
// what each asserted was whatever someone had needed to debug: boundaries 02, 05
// and 06 asserted nothing, not by decision but because nobody had been burned
// there yet. What a boundary guarantees is now declared in boundaryContracts and
// the compiler refuses a boundary that declares nothing.
func (o *Orchestrator) deployBoundaryN(ctx context.Context, kubeconfig string, number int) error {
	c, err := contractFor(number)
	if err != nil {
		return err
	}
	return o.runBoundary(ctx, kubeconfig, c)
}

// tailnetCredentialStagedOnHub reports whether the Secret the hub-operator reads
// its tailnet credential from exists on the hub.
//
// Trivially satisfied when on-prem was not requested: a pure-cloud box needs no
// tailnet, and demanding the Secret would fail every hetzner bootstrap.
//
// Presence only, not content. The Secret is legitimately EMPTY on a box with no
// authkey -- that is what renders the ClusterClass's tailscale steps inert -- so
// requiring a non-empty value here would fail a configuration that is correct.
// What must not happen is the Secret being absent, because the operator then
// skips the upload silently.
func (o *Orchestrator) tailnetCredentialStagedOnHub(ctx context.Context, kubeconfig string) error {
	asker, ok := o.Provider.(interface{ OnPremRequested() bool })
	if !ok || !asker.OnPremRequested() {
		return nil
	}
	err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "secret", "tailscale-hybrid-psk", "-n", "platform-capi").Run()
	if err != nil {
		return fmt.Errorf("platform-capi/tailscale-hybrid-psk is not on the hub, so the " +
			"hub-operator has nothing to upload and the tailnet ExternalSecret cannot sync")
	}
	return nil
}

// onPremWorkersPresent reports whether this box has the on-prem capacity it asked
// for.
//
// Trivially satisfied when on-prem was not requested: a hetzner box has no home
// workers by design, and demanding one would fail every pure-cloud bootstrap.
// The claim is only "if you asked for on-prem nodes, at least one is here".
//
// A point-in-time probe, never a wait -- the phase itself owns waiting for a node
// to join. This only decides whether the phase must run again.
func (o *Orchestrator) onPremWorkersPresent(ctx context.Context, kubeconfig string) error {
	// Type assertion, matching how CloudProvider itself asks its driver: only a
	// provider that can have on-prem capacity implements this, and one that
	// cannot is not in error -- it simply has nothing to confirm.
	asker, ok := o.Provider.(interface{ OnPremRequested() bool })
	if !ok || !asker.OnPremRequested() {
		return nil
	}
	switch state, name := o.probeHubWorker(ctx, kubeconfig); state {
	case hubWorkerReady:
		return nil
	case hubWorkerNotReady:
		return fmt.Errorf("%s carries the on-prem labels but is NotReady, so nothing "+
			"the platform pins to it can schedule", name)
	case hubWorkerAbsent:
		return fmt.Errorf("this box asked for on-prem nodes and none has joined")
	default:
		return fmt.Errorf("the hub's nodes could not be read to confirm on-prem capacity")
	}
}

// describeMissingRoles names them rather than counting them, because the name is
// what an operator greps for and the count is not actionable.
func describeMissingRoles(missing []string) string {
	if len(missing) == 1 {
		return fmt.Sprintf("the role %s does not exist", missing[0])
	}
	return fmt.Sprintf("these roles do not exist: %s", strings.Join(missing, ", "))
}

// declaredDatabaseRoles is the set of roles this box's HubEnvironment says the
// platform needs. The names only -- each entry also carries a database and a
// permission list, which belong to the operator that grants them.
func (o *Orchestrator) declaredDatabaseRoles(ctx context.Context, kubeconfig string) ([]string, error) {
	// Cluster-scoped, so no -A and no -n. See the note in the poll loop below:
	// naming a cluster-scoped resource together with --all-namespaces makes
	// kubectl exit 0 and print nothing, which reads as "no roles declared".
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "hubenvironment", "hub-environment",
		"-o", "jsonpath={.spec.database.roles[*].name}").Output()
	if err != nil {
		return nil, fmt.Errorf("could not read the declared database roles: %w", err)
	}
	return strings.Fields(strings.TrimSpace(string(out))), nil
}

// missingDatabaseRoles asks the DATABASE which declared roles are not in it.
//
// An error means the database could not be asked, which is not the same as every
// role being present and must never be read as it.
func (o *Orchestrator) missingDatabaseRoles(ctx context.Context, kubeconfig string) ([]string, error) {
	declared, err := o.declaredDatabaseRoles(ctx, kubeconfig)
	if err != nil {
		return nil, err
	}
	if len(declared) == 0 {
		return nil, nil
	}

	primary, err := o.cnpgPrimary(ctx, kubeconfig)
	if err != nil || primary == "" {
		return nil, fmt.Errorf("the platform database has no primary to ask yet")
	}

	// Every role name, compared in Go. Nothing from the HubEnvironment is
	// interpolated into the statement -- the spec is operator-supplied, and a
	// role name is not a place to start trusting it.
	out, err := o.psql(ctx, kubeconfig, primary, "postgres", "select rolname from pg_roles")
	if err != nil {
		return nil, fmt.Errorf("could not read pg_roles: %w", err)
	}
	present := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			present[name] = true
		}
	}

	var missing []string
	for _, role := range declared {
		if !present[role] {
			missing = append(missing, role)
		}
	}
	return missing, nil
}

// hubIngressAddressResolved reports whether the platform has worked out the
// address every public hostname on this box resolves to.
//
// hub-operator reads it from kube-system/kubeadm-config and publishes it for
// external-dns (see operators/hub-operator/internal/controller/ingress_address.go).
// Unresolved means no hub hostname is published at all: infisical, argocd, auth,
// id, dashboard and api are NXDOMAIN and ACME can issue for none of them.
//
// Asserted rather than assumed because this was a warning once. The bootstrap
// printed "could not resolve hub ingress address", ran twenty more phases and
// reported success, and the operator found out from a browser.
func (o *Orchestrator) hubIngressAddressResolved(ctx context.Context, kubeconfig string) error {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "hubenvironment", "hub-environment", "-o",
		`jsonpath={.status.conditions[?(@.type=="IngressAddressResolved")].status}`).Output()
	if err != nil {
		return fmt.Errorf("the HubEnvironment could not be read to confirm the hub's " +
			"ingress address is known")
	}
	switch strings.TrimSpace(string(out)) {
	case "True":
		return nil
	case "":
		return fmt.Errorf("the hub-operator has not reported IngressAddressResolved.\n" +
			"  An operator that predates this condition never will -- upgrade the bundle,\n" +
			"  which is what carries it")
	default:
		msg, _ := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"get", "hubenvironment", "hub-environment", "-o",
			`jsonpath={.status.conditions[?(@.type=="IngressAddressResolved")].message}`).Output()
		return fmt.Errorf("the hub's ingress address is not resolved: %s",
			strings.TrimSpace(string(msg)))
	}
}

// awaitDatabaseRolesProvisioned waits until the platform's database roles exist
// AND the hub-operator reports it put them there.
//
// Both, because either alone has been wrong on this platform.
//
// The condition alone was wrong on 2026-09-15. A platform database's PVC is
// node-local on the on-prem placement class (local-path); the worker was rebuilt
// from a pristine image, the database came back empty, and CNPG re-ran initdb -- but the
// operator's DatabaseRolesProvisioned condition still said True, stamped ten and
// a half hours earlier against a database that no longer existed. This gate read
// it and printed "✓ database roles provisioned after 1s" over a cluster in which
// hub_zitadel did not exist. Boundary 04 then waited for an identity provider
// that could not authenticate, behind an operator that would never re-provision
// the role because its own record said it already had.
//
// Counting roles alone was the objection that made it a condition in the first
// place, and it was a fair one: provisioning is all-or-nothing, and half of it
// is indistinguishable from all of it by COUNTING. So this does not count. It
// compares against the set the HubEnvironment declares, by name, which is a
// stronger claim than a count and a different claim from the operator's memory.
//
// What it still does not prove is that each role's PASSWORD matches the Secret
// its consumer holds. Existence is necessary, not sufficient; the grants and the
// credential remain the operator's to get right.
func (o *Orchestrator) awaitDatabaseRolesProvisioned(ctx context.Context, kubeconfig string) error {
	const (
		deadline = 45 * time.Minute
		every    = 10 * time.Second
	)
	started := time.Now()
	var lastNudge time.Time

	// Whether the condition was EVER readable, as distinct from never True.
	//
	// The two failures need different messages and previously shared one. A
	// malformed query returns empty for ever and reads exactly like an operator
	// that is still working, so the timeout blamed the operator for a fault in
	// how it was being asked.
	seen := false

	// Whether the DATABASE was ever reachable, and what it last said was missing.
	// Kept apart from `seen` for the same reason `seen` exists at all: "could not
	// ask" and "asked, and the answer was no" need different messages.
	probed := false
	warnedStale := false
	var lastMissing []string

	for {
		// No -A, and no .items[*].
		//
		// HubEnvironment is CLUSTER-SCOPED. Passing a resource name together with
		// --all-namespaces makes kubectl exit 0 and print NOTHING -- it does not
		// error -- so this loop polled for its whole deadline against a condition
		// it could not observe, while the condition had been True since minutes
		// after the boundary began. The failure it then reported ("the
		// hub-operator did not provision the platform's database roles") was
		// indistinguishable from the real fault this gate exists to catch.
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"get", "hubenvironment", "hub-environment", "-o",
			`jsonpath={.status.conditions[?(@.type=="DatabaseRolesProvisioned")].status}`).Output()
		if status := strings.TrimSpace(string(out)); err == nil && status != "" {
			seen = true
		}
		operatorDone := err == nil && strings.TrimSpace(string(out)) == "True"

		// Ask the database itself. The condition says the operator believes it
		// finished; this says the roles are actually there now.
		missing, dbErr := o.missingDatabaseRoles(ctx, kubeconfig)
		if dbErr == nil {
			probed = true
			lastMissing = missing
		}

		if operatorDone && dbErr == nil && len(missing) == 0 {
			fmt.Printf("    ✓ database roles provisioned after %s\n",
				formatDuration(time.Since(started)))
			return nil
		}

		// Said once, and loudly. A True condition over a database that does not
		// have the roles is the signature of a database replaced underneath the
		// operator, and it is not something to discover from a timeout 45 minutes
		// later -- by then the run has been waiting on an identity provider that
		// was never going to start.
		if operatorDone && dbErr == nil && len(missing) > 0 && !warnedStale {
			warnedStale = true
			fmt.Printf("    ⚠️  the hub-operator reports DatabaseRolesProvisioned=True, but %s\n",
				describeMissingRoles(missing))
			fmt.Println("        Its record describes a database that no longer has them — most often")
			fmt.Println("        because the volume behind an application's cluster was replaced. The")
			fmt.Println("        operator will not redo work its own condition says is done; it has to")
			fmt.Println("        be re-driven.")
		}

		// Nudge anything ESO has backed off on.
		//
		// refreshInterval is ESO's retry interval as well as its drift cadence, so
		// an ExternalSecret that attempted a moment before the ClusterSecretStore
		// finished validating waits a full interval before trying again --
		// observed as an attempt at 08:36:48, the store becoming Ready at
		// 08:37:01, and the next attempt due at 09:36 against a fifteen-minute
		// budget. The intervals on the Day-0-critical secrets are shortened for
		// this reason; this makes the wait self-correcting even where they are
		// not, and on a box whose bundle predates that change.
		//
		// Every 30s rather than every poll: the annotation is a write, and ESO
		// needs time to act on it.
		if time.Since(lastNudge) > 30*time.Second {
			o.nudgeStalledExternalSecrets(ctx, kubeconfig)
			lastNudge = time.Now()
		}

		if time.Since(started) > deadline {
			if !seen {
				return fmt.Errorf("the DatabaseRolesProvisioned condition was never readable "+
					"in %v -- not false, ABSENT.\n"+
					"  Either the hub-operator has not reached the phase that publishes it, or\n"+
					"  this check is asking for it wrongly. HubEnvironment is cluster-scoped;\n"+
					"  confirm with:\n"+
					"    kubectl get hubenvironment hub-environment -o yaml | grep -A3 DatabaseRoles",
					deadline)
			}
			if !probed {
				return fmt.Errorf("the platform databases could not be read in %v to confirm "+
					"their roles exist.\n"+
					"  The hub-operator's condition is not enough on its own -- it records what the\n"+
					"  operator did, not what the databases now contain -- so this gate does not\n"+
					"  pass on it alone. Each application owns its own cluster (ADR-112); check\n"+
					"  they are serving:\n"+
					"    kubectl -n %s get clusters.postgresql.cnpg.io", deadline, constants.NamespaceData)
			}
			if len(lastMissing) > 0 {
				return fmt.Errorf("the platform database is missing %s after %v.\n  %s\n"+
					"  Nothing that owns a database can start until they exist: Zitadel's hooks\n"+
					"  fail on a role that does not exist and exhaust their retries against it.\n\n"+
					"  If the hub-operator reports DatabaseRolesProvisioned=True while this says\n"+
					"  they are absent, the database was replaced after it last ran -- the operator\n"+
					"  will not repeat work its own record calls done.",
					describeMissingRoles(lastMissing), deadline,
					o.hubEnvironmentBlocker(ctx, kubeconfig))
			}
			return fmt.Errorf("the hub-operator did not provision the platform's database "+
				"roles within %v.\n  %s\n"+
				"  Nothing that owns a database can start until it does: Zitadel's hooks fail\n"+
				"  on a role that does not exist and exhaust their retries against it",
				deadline, o.hubEnvironmentBlocker(ctx, kubeconfig))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// nudgeStalledExternalSecrets asks ESO to retry any ExternalSecret that is not
// synced, instead of waiting out its refresh interval.
//
// `force-sync` is ESO's own mechanism for this: the annotation's value only has
// to change for the controller to reconcile immediately. Best effort throughout
// -- a nudge that fails costs nothing, because the interval still applies.
func (o *Orchestrator) nudgeStalledExternalSecrets(ctx context.Context, kubeconfig string) {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "externalsecret", "-A", "-o",
		`jsonpath={range .items[?(@.status.conditions[0].reason!="SecretSynced")]}{.metadata.namespace} {.metadata.name}{"\n"}{end}`).Output()
	if err != nil {
		return
	}
	stamp := fmt.Sprintf("%d", time.Now().Unix())
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		_ = exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"annotate", "externalsecret", parts[1], "-n", parts[0],
			"force-sync="+stamp, "--overwrite").Run()
	}
}

// hubEnvironmentBlocker returns the operator's own reason for not having
// finished, which names the phase it is stuck in.
func (o *Orchestrator) hubEnvironmentBlocker(ctx context.Context, kubeconfig string) string {
	// Same correction as the gate above: cluster-scoped, so no -A and no .items[*].
	// This reported "no failing condition" for a query that returned nothing at
	// all, which is why the timeout said the operator had not finished when it had.
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "hubenvironment", "hub-environment", "-o",
		`jsonpath={range .status.conditions[?(@.status=="False")]}{.type}: {.message}{"\n"}{end}`).Output()
	if err != nil {
		return "the HubEnvironment could not be read"
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return "hub-operator reports: " + line
		}
	}
	return "the HubEnvironment reports no failing condition; check the hub-operator's logs"
}

// zitadelServing reports whether the identity provider is serving right now.
//
// One point-in-time probe with no wait in it, so it can be used both as the
// condition awaitZitadelReady polls and as boundary 04's postcondition on
// resume, where a wait would make every resume pay Zitadel's start-up time.
//
// readyReplicas, not the Deployment's existence: Zitadel crash-looping on a
// half-initialised database is a Deployment that exists, has a pod, and serves
// nothing.
func (o *Orchestrator) zitadelServing(ctx context.Context, kubeconfig string) error {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "deployment", "zitadel", "-n", "platform-identity",
		"-o", "jsonpath={.status.readyReplicas}").Output()
	if err != nil {
		return fmt.Errorf("the zitadel deployment could not be read in platform-identity")
	}
	if ready := strings.TrimSpace(string(out)); ready == "" || ready == "0" {
		return fmt.Errorf("the identity provider has no ready replica")
	}
	return nil
}

// awaitZitadelReady waits until the identity provider is serving, and says what
// is wrong when it is not.
//
// Bounded, because "not ready in ten minutes" is an answer an operator can act
// on and an unbounded wait is a hung bootstrap. The message carries the setup
// Job's own last error rather than a pod status, since that is what names the
// cause: a failed migration, a database it cannot reach, or a schema half
// written by a previous attempt.
func (o *Orchestrator) awaitZitadelReady(ctx context.Context, kubeconfig string) error {
	const (
		// Longer than the setup Job's own activeDeadlineSeconds (600s), so this
		// phase outlives the Job and reports the Job's verdict rather than timing
		// out over one that is still retrying. The two are set together; raising
		// one without the other makes the wait meaningless in one direction or
		// the error misleading in the other.
		deadline = 12 * time.Minute
		every    = 15 * time.Second
	)
	started := time.Now()
	fmt.Println("[boundary04] waiting for the identity provider to serve")

	for {
		if err := o.zitadelServing(ctx, kubeconfig); err == nil {
			fmt.Printf("[boundary04] ✓ identity provider ready after %s\n",
				formatDuration(time.Since(started)))
			return nil
		}

		if time.Since(started) > deadline {
			reason := o.zitadelSetupFailure(ctx, kubeconfig)
			return fmt.Errorf("the identity provider did not start within %v.\n"+
				"  %s\n"+
				"  Nothing downstream of it can work: iam-admin-pat is not created, so the\n"+
				"  identity token is not uploaded, so identity-service-credentials never\n"+
				"  resolves and kube-sbt cannot start",
				deadline, reason)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// zitadelSetupFailure returns the setup Job's own last error, or a fallback.
func (o *Orchestrator) zitadelSetupFailure(ctx context.Context, kubeconfig string) string {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"logs", "-n", "platform-identity", "job/zitadel-setup",
		"-c", "zitadel-setup", "--tail=4").CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return "zitadel-setup last said: " + lines[len(lines)-1]
	}

	// The Job may be gone. Its delete policy carries `hook-failed`, because a
	// failed hook Job left in place is read as the hook's result by every later
	// sync and wedges the Application permanently -- so a failure deletes itself,
	// and takes its logs with it.
	//
	// ArgoCD's own record of why the hook failed outlives the Job, so ask it
	// rather than reporting "no readable logs", which says nothing and was what
	// this returned for a box whose hooks had been cleaned up exactly as designed.
	if msg := o.zitadelHookFailure(ctx, kubeconfig); msg != "" {
		return msg
	}
	return "zitadel-setup produced no readable logs and ArgoCD recorded no hook failure; " +
		"check `kubectl get application zitadel -n platform-ops -o jsonpath='{.status.operationState}'`"
}

// zitadelHookFailure returns ArgoCD's own record of a failed PreSync hook.
//
// Survives the Job, which `hook-failed` deletes as soon as it fails.
func (o *Orchestrator) zitadelHookFailure(ctx context.Context, kubeconfig string) string {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "application", "zitadel", "-n", "platform-ops", "-o",
		`jsonpath={range .status.operationState.syncResult.resources[?(@.hookPhase=="Failed")]}{.name}: {.message}{"\n"}{end}`).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return "ArgoCD recorded the hook failure: " + line
		}
	}
	return ""
}

// publicTlsIssuerFor returns the ACME ClusterIssuer for this environment — the
// issuer half of ADR-051's public-TLS policy. The mapping lives HERE because
// the bootstrap is the environment boundary: it already owns environmentSlug,
// and no chart or ApplicationSet may re-derive policy (ADR-047). There is no
// default: an unknown or empty slug fails the bootstrap rather than silently
// inheriting a throwaway value — staging is an explicit ephemeral policy, never
// an accidental default.
func (o *Orchestrator) publicTlsIssuerFor() (string, error) {
	switch o.EnvironmentSlug {
	case "ephemeral":
		// Ephemeral clusters are created and destroyed continuously, so they are
		// the only environment whose certificate churn can realistically exhaust
		// Let's Encrypt's per-domain weekly quota. That risk is what staging is
		// for, and it is confined to where the risk actually exists.
		return "letsencrypt-staging", nil
	case "dev", "stg", "prod":
		// dev is browser-facing: people sign in through it, and the OAuth2 flow
		// redirects between hub and tenant hostnames. A staging certificate makes
		// the browser refuse the tenant host outright, so the redirect chain
		// breaks rather than merely warning — the login looks broken for a reason
		// that has nothing to do with identity. The hub's own dev hostnames
		// already issue from letsencrypt-prod, so staging here bought no quota
		// protection on a long-lived cluster and only left tenant hosts untrusted
		// by the browser that had just trusted the hub they authenticate against.
		return "letsencrypt-prod", nil
	default:
		return "", fmt.Errorf(
			"publicTlsIssuer: no issuer mapping for environment slug %q (expected dev|ephemeral|stg|prod) — refusing to guess TLS policy",
			o.EnvironmentSlug)
	}
}

// hubIngressAddress returns the public IPv4 that fronts :80/:443 for this hub —
// the CAPH-managed control-plane LoadBalancer. Returns "" when it cannot be
// resolved, in which case the chart falls back to its default (publishing the
// controller Service address) and public ingress DNS will be wrong.
//
// Selected by cluster name, never by position. This asked for `items[0]`, which
// was right only while the hub was the sole HetznerCluster in the namespace. It
// is not: every spoke pool this hub provisions adds one beside it, and on
// acme-hub `spoke-pool-hybrid-dev-01-n5r6z` now sits in platform-capi next to
// `acme-hub-gmj6h`. The hub kept winning that race because the names happened to
// sort that way -- rename the hub, or add a spoke whose name sorts earlier, and
// the hub Gateway publishes a SPOKE's address for every hub hostname, which
// resolves, answers, and serves the wrong cluster.
//
// CAPI labels every infrastructure object with the cluster it belongs to, so the
// question has an exact answer and there is no reason to guess at one.
func (o *Orchestrator) hubIngressAddress(ctx context.Context, kubeconfig string) string {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "hetznercluster", "-n", constants.NamespaceCAPI,
		"-l", "cluster.x-k8s.io/cluster-name="+o.ClusterName,
		"-o", "jsonpath={.items[*].spec.controlPlaneEndpoint.host}").Output()
	if err != nil {
		return ""
	}

	// Exactly one, or nothing. Two matches means the label does not identify what
	// it is being asked to identify, and picking either would be the same mistake
	// in a new place.
	hosts := strings.Fields(strings.TrimSpace(string(out)))
	if len(hosts) != 1 {
		return ""
	}
	return hosts[0]
}

// ──────────────────────────────────────────────────────────────────────────
// Upgrade / resume path
// ──────────────────────────────────────────────────────────────────────────

func (o *Orchestrator) handleExistingState(ctx context.Context, stateMgr *state.StateManager, bs *state.BootstrapState) error {
	fmt.Printf("\n[recovery] Found existing state for cluster '%s'\n", o.ClusterName)
	fmt.Printf("[recovery] Last completed phase: %s\n", bs.CurrentPhase)

	// The provider is a property of the CLUSTER, not of this invocation.
	//
	// A resumed bootstrap that is told a different provider than the one the
	// cluster was built with does not switch providers -- it half-applies the new
	// one to a topology shaped by the old. Observed: a hybrid box (one control
	// plane, zero cloud workers, workers expected from the tenant's own hardware)
	// resumed as hetzner. `--on-prem` was therefore absent, on-prem-join reported
	// "Not a home-worker cell — skipping", and the run continued toward deploying
	// the platform onto a single tainted control plane with nowhere to schedule.
	//
	// Nothing compared the two, though the state records the provider and has
	// since it was written. Refused rather than corrected: picking one for the
	// operator would silently discard whichever they meant.
	if recorded := strings.TrimSpace(bs.Provider); recorded != "" {
		if asked := strings.TrimSpace(o.providerName()); asked != "" && asked != recorded {
			return fmt.Errorf("this cluster was built with --provider %s and this run says --provider %s.\n"+
				"  The provider decides the cluster's topology -- how many cloud workers it has,\n"+
				"  and whether its workers come from the tenant's own hardware -- so resuming with\n"+
				"  a different one applies half of it to a shape built for the other.\n\n"+
				"  Re-run with --provider %s, or tear the cluster down and build it as %s.",
				recorded, asked, recorded, asked)
		}
	}

	if o.phaseDone(bs, state.PhaseComplete) {
		// Verified, not trusted. See phasePostconditions: a bootstrap that
		// recorded `complete` while a phase's effect never happened would
		// otherwise refuse every run that could repair it, including the runs
		// carrying the repair.
		unmet := o.unmetPostconditions(ctx, bs, bs.MgmtKubeconfig)
		if len(unmet) == 0 {
			fmt.Println("Cluster already bootstrapped. No further CLI operations permitted per ADR-040.")
			return nil
		}
		fmt.Printf("[recovery] This bootstrap is recorded complete, but %d phase(s) did not leave the cluster in the state they claim:\n", len(unmet))
		fmt.Print(describeUnmet(unmet))
		fmt.Println("[recovery] Withdrawing those records and resuming. Nothing already working is re-done:")
		fmt.Println("[recovery]   every other phase is skipped as usual.")
		withdrawCompletion(bs, unmet)
		if err := stateMgr.Save(bs); err != nil {
			return err
		}
	}

	// Resume from where we left off — the runFresh pipeline will skip
	// completed phases via runPhase.
	fmt.Println("[recovery] Resuming from next phase...")
	if o.Debug {
		fmt.Printf("[DEBUG] Completed phases: %v\n", bs.CompletedPhases)
	}
	return o.runFresh(ctx, stateMgr, bs)
}

// ──────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────

// defaultKubeconfigPath is where kubectl and kind agree the kubeconfig lives.
//
// $KUBECONFIG first, because kind honours it and this did not. An operator with
// it exported -- watching a previous cluster, say -- got a bootstrap in which
// `kind create cluster` wrote the new context into that file while every kubectl
// call here looked for it in ~/.kube/config. The context was therefore never
// found, kubeconfigPaths fell back to the unpinned path, and kubectl resolved
// nothing and tried localhost:8080. The run failed three phases later with
// `dial tcp [::1]:8080: connect: connection refused`, which names neither the
// file nor the variable and reads like a broken cluster.
//
// The FIRST entry of the list, which is the rule kubectl uses for writes and
// therefore where kind put it. Later entries are merged for reads only, so a
// context written to the first and looked for in a later one is the same bug
// again.
func defaultKubeconfigPath() string {
	if env := strings.TrimSpace(os.Getenv("KUBECONFIG")); env != "" {
		if first := strings.Split(env, string(os.PathListSeparator))[0]; first != "" {
			return first
		}
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".kube", "config")
}

// kubeconfigPaths returns the bootstrap cluster's kubeconfig and context.
//
// The kubeconfig is pinned to that context: ~/.kube/config holds every cluster
// an operator has ever touched, and the twenty-three kubectl calls below pass
// the path without the context, so each resolves whatever the operator's shell
// last selected. That is not a hypothetical -- it read a CAPI cluster's status
// from an unrelated local kind cluster and reported that the resource type did
// not exist, and it looked for the hub's kubeconfig Secret in a namespace that
// cluster does not have.
//
// Pinning here rather than at each call site is deliberate: a path that names
// one cluster cannot be used against another, while a --context argument has to
// be remembered twenty-three times and is silently absent when it is not.
func (o *Orchestrator) kubeconfigPaths(bs *state.BootstrapState) (string, string, error) {
	// The file recorded when the bootstrap cluster was created is the authority.
	//
	// Resolving the ambient $KUBECONFIG instead answered a bootstrap-cluster
	// question with the hub: hub-bootstrap.sh exports KUBECONFIG to the hub's
	// kubeconfig once that file exists, which is right for the phases that talk
	// to the hub and wrong for the ones that talk to the bootstrap cluster. The
	// hub's file names one context and it is not kind-<cluster>, so pinning
	// failed, the unpinned path returned the hub, and on-prem-join asked the hub
	// for the CAPI kubeconfig secret -- "namespaces platform-capi not found",
	// because CAPI does not live there until pivot. The kind cluster, the
	// namespace and the secret were all healthy.
	//
	// Empty only before the cluster has been created, where there is nothing
	// recorded yet and the ambient value is what kind itself will write to.
	recorded := bs.BootstrapKubeconfig
	kubeconfig := recorded
	if kubeconfig == "" {
		kubeconfig = defaultKubeconfigPath()
	}
	ctx := bs.BootstrapContext
	if ctx == "" {
		ctx = o.BootstrapContext
	}
	if ctx == "" {
		ctx = "kind-" + o.ClusterName
	}
	pinned, err := pinKubeconfig(kubeconfig, ctx)
	if err == nil {
		return pinned, ctx, nil
	}

	// A RECORDED kubeconfig that no longer resolves its context is a hard failure.
	//
	// bootstrap-create writes this field only after proving the context is in the
	// file, so reaching here means something later unmade that -- the file was
	// deleted, overwritten, or the cluster destroyed underneath the state. The
	// previous behaviour returned the unpinned path, and kubectl then answered
	// bootstrap-cluster questions with whatever cluster that file did name. That
	// is the whole 2026-09-18 failure: the hub answered for the kind cluster and
	// the run died three phases later on "namespaces platform-capi not found",
	// naming neither the file nor the substitution.
	//
	// Silence here buys nothing. Stopping costs one re-run of one phase.
	if recorded != "" {
		return "", "", fmt.Errorf(
			"the bootstrap cluster's kubeconfig no longer resolves context %q.\n\n"+
				"state records %s, written by bootstrap-create once that context was\n"+
				"present. It is not there now, so this run cannot tell the bootstrap\n"+
				"cluster apart from any other cluster that file names.\n\n"+
				"If the kind cluster is still up, re-run the bootstrap-create phase to\n"+
				"re-pin it. If it is gone, tear down and start over -- later phases\n"+
				"cannot be resumed without it: %w",
			ctx, recorded, err)
	}

	// Nothing recorded: the bootstrap cluster has not been created yet, and the
	// ambient value is what kind itself will write to. The calls that need the
	// cluster fail on their own terms rather than here.
	return kubeconfig, ctx, nil
}

// pinKubeconfig writes a kubeconfig naming exactly one cluster.
func pinKubeconfig(kubeconfig, kubeContext string) (string, error) {
	out, err := exec.Command("kubectl",
		"--kubeconfig", kubeconfig, "--context", kubeContext,
		"config", "view", "--minify", "--flatten", "-o", "yaml",
	).Output()
	if err != nil {
		return "", fmt.Errorf("context %q not resolvable in %s: %w", kubeContext, kubeconfig, err)
	}

	// Named for the context and reused across phases, so a resumed run reads the
	// same file rather than accumulating one per invocation.
	path := filepath.Join(os.TempDir(), "hub-bootstrap-"+kubeContext+".kubeconfig")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", fmt.Errorf("failed to write a pinned kubeconfig: %w", err)
	}
	return path, nil
}

func (o *Orchestrator) getArgoCDPassword(ctx context.Context, kubeconfig string) string {
	ci := &components.Installer{Kubeconfig: kubeconfig, EnvironmentSlug: o.EnvironmentSlug, GitopsDir: o.GitopsDir, ClusterName: o.ClusterName}
	pwd, err := ci.GetArgoCDPassword(ctx)
	if err != nil {
		return "<check secret manually>"
	}
	return pwd
}

func (o *Orchestrator) checkKindClusterExists() error {
	cmd := exec.Command("kind", "get", "clusters")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	if strings.Contains(string(out), o.ClusterName) {
		return fmt.Errorf("kind cluster '%s' already exists. Run teardown first", o.ClusterName)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// Shared kubectl / git helpers
// ──────────────────────────────────────────────────────────────────────────

// waitForOperators waits for validating webhook configurations matching the
// built-in platform operators and any extraPatterns provided by the
// provider's OperatorWebhookPatterns() to be established.
//
// The check is a thin wrapper over the health package's
// ValidatingWebhookHealth, composing base + extra patterns into a single
// check. This keeps the wait loop, timeout, and context handling
// consistent with the rest of the bootstrap pipeline.
func waitForOperators(ctx context.Context, kubeconfig string, extraPatterns []string) error {
	patterns := []string{"capi", "cert-manager", "cnpg", "externalsecret"}
	patterns = append(patterns, extraPatterns...)

	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewValidatingWebhookHealth(patterns...),
		},
		Interval: 10 * time.Second,
		Timeout:  20 * time.Minute,
	}
	return waiter.Wait(ctx, kubeconfig)
}

// waitForCRDs polls the API server until critical CRDs are registered in
// resource discovery. Validating webhooks existing does not guarantee the
// CRD is registerable — there is a propagation delay.
func (o *Orchestrator) waitForCRDs(ctx context.Context, kubeconfig string) error {
	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewCRDRegisteredHealth(
				"externalsecrets.external-secrets.io",
				"clusters.postgresql.cnpg.io",
			),
		},
		Interval: 5 * time.Second,
		Timeout:  10 * time.Minute,
		OnCheckPass: func(_ health.HealthChecker) {
			fmt.Println("[platform-deploy] ✓ CRDs queryable")
		},
	}
	return waiter.Wait(ctx, kubeconfig)
}

// waitForOperatorPods polls until critical operator pods are Ready. Webhooks
// and CRDs may exist, but webhook endpoints return 503 until their pods start.
func (o *Orchestrator) waitForOperatorPods(ctx context.Context, kubeconfig string) error {
	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewOperatorPodsHealth("cnpg-system", "app.kubernetes.io/name=cloudnative-pg"),
			health.NewOperatorPodsHealth("platform-ops", "app.kubernetes.io/name=external-secrets"),
		},
		Interval: 5 * time.Second,
		Timeout:  15 * time.Minute,
		OnCheckPass: func(_ health.HealthChecker) {
			fmt.Println("[platform-deploy] ✓ Operator pods Ready")
		},
	}
	return waiter.Wait(ctx, kubeconfig)
}

// ensureArgoCDGitHubAuth provisions the ArgoCD repo-creds secret (GitHub PAT)
// so private-repo apps can sync. Token from GITHUB_TOKEN env or the local
// k8-secrets/github/github-pat-token file.
func (o *Orchestrator) ensureArgoCDGitHubAuth(ctx context.Context, kubeconfig, orgURL string) error {
	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		data, err := os.ReadFile("k8-secrets/github/github-pat-token")
		if err != nil {
			return fmt.Errorf("GITHUB_TOKEN not set and github-pat-token file unreadable: %w", err)
		}
		githubToken = strings.TrimSpace(string(data))
	}
	if githubToken == "" {
		return fmt.Errorf("GITHUB_TOKEN is empty — ArgoCD cannot read this box's own gitops repository")
	}

	ci := &components.Installer{Kubeconfig: kubeconfig, EnvironmentSlug: o.EnvironmentSlug, GitopsDir: o.GitopsDir, ClusterName: o.ClusterName}
	if err := ci.FixArgoCDGitHubAuth(ctx, githubToken, orgURL); err != nil {
		return err
	}
	return nil
}

// platformBoxInstanceRepo is the instance repository for the platform's OWN box.
//
// ADR-062 gives every box its own `<tenant>-gitops` repository and keeps this name
// for the platform's. It lives in the CLI rather than as a chart default on
// purpose: a chart default travels to every cluster that installs the published
// bundle, which is how one box's identity once became the fallback for all of them.
// Nothing reaches this but a run with no tenant repository to read.
const platformBoxInstanceRepo = "https://github.com/soloz-io/fleet-registry"

// instanceRepoURL returns this box's instance repository (ADR-062).
//
// The source is the gitops repository Day-0 is running inside, which is what
// ADR-072 makes Day-0 run from. There is deliberately no default: any default
// would name some other box's repository, and shipping one as a chart default is
// how soloz-io/fleet-registry became the fallback for every box that installed
// the published bundle.
func (o *Orchestrator) instanceRepoURL() (string, error) {
	if o.InstanceRepoURL != "" {
		return o.InstanceRepoURL, nil
	}
	if o.GitopsDir == "" {
		// The platform's own box, bootstrapped from a checkout of this repository
		// rather than from a tenant's (ADR-072). ADR-062 keeps the name
		// `fleet-registry` for exactly this one instance repository, so it is not a
		// default in the sense ADR-075 refuses -- it is this box's own name for
		// itself, and it is reached only when there is no tenant repository to ask.
		//
		// Removing it entirely made every non-tenant bootstrap fail before the seed
		// was applied: boundaries 05 and 06 never rendered, and boundary 01 waited
		// ten minutes for ApplicationSets that could not appear.
		return platformBoxInstanceRepo, nil
	}
	out, err := exec.Command("git", "-C", o.GitopsDir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("cannot read the origin remote of %s: %w.\n"+
			"Day-0 runs inside this box's own repository (ADR-072) and takes the "+
			"instance repository URL from it", o.GitopsDir, err)
	}
	repo := strings.TrimSpace(string(out))
	if repo == "" {
		return "", fmt.Errorf("%s has no origin remote, so this box cannot name "+
			"the repository it reconciles", o.GitopsDir)
	}
	return repo, nil
}

// boxOrgURL returns the git organisation owning this box's repositories, as the
// URL prefix an ArgoCD repo-creds secret is scoped by. Derived from the instance
// repository rather than named, which is what lets a box live in an organisation
// that is not the platform's.
func (o *Orchestrator) boxOrgURL() (string, error) {
	repo, err := o.instanceRepoURL()
	if err != nil {
		return "", err
	}

	// scheme://host/org, dropping the repository name. An SSH remote
	// (git@github.com:org/repo.git) is normalised to the same shape, because that
	// is what ArgoCD matches an HTTPS source against.
	repo = strings.TrimSuffix(repo, ".git")
	if strings.HasPrefix(repo, "git@") {
		repo = "https://" + strings.Replace(strings.TrimPrefix(repo, "git@"), ":", "/", 1)
	}
	parts := strings.Split(repo, "/")
	if len(parts) < 4 {
		return "", fmt.Errorf("cannot read an organisation out of %q: expected a "+
			"repository URL of the form https://host/org/repo", repo)
	}
	return strings.Join(parts[:4], "/"), nil
}

func currentGitBranch() string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ──────────────────────────────────────────────────────────────────────────
// ADR-045 artifact validation
// ──────────────────────────────────────────────────────────────────────────

// adr045GeneratedDirs returns the directories holding registered artifacts, so
// the commit step stages exactly what the registry declares.
func (o *Orchestrator) readADR045Registry() (adr045Registry, error) {
	var reg adr045Registry
	projectRoot, err := os.Getwd()
	if err != nil {
		return reg, fmt.Errorf("get working directory: %w", err)
	}
	_ = projectRoot
	raw, err := platform.ReadFile(filepath.Join("manifests", "generated", "artifacts.yaml"))
	if err != nil {
		return reg, fmt.Errorf("read ADR-045 registry: %w", err)
	}
	if err := yaml.Unmarshal(raw, &reg); err != nil {
		return reg, fmt.Errorf("parse ADR-045 registry: %w", err)
	}

	// {env} is the environment being bootstrapped. The registry writes it
	// because these artifacts live per environment (ADR-045) -- one shared file
	// under base/ meant prod rendered dev's Infisical project -- and the
	// placeholder is expanded here so no caller can forget: the validator, the
	// commit paths and the wait list all read the registry through this.
	//
	// Declared and never expanded, the validator looked for a directory called
	// "{env}" and reported the artifact missing, naming a path no step had ever
	// written.
	if o.EnvironmentSlug == "" {
		return reg, fmt.Errorf("ADR-045 registry declares per-environment " +
			"artifacts but no environment is set; the paths cannot be resolved")
	}
	for i := range reg.Artifacts {
		reg.Artifacts[i].File = strings.ReplaceAll(
			reg.Artifacts[i].File, "{env}", o.EnvironmentSlug)
	}

	// A tenant's repository holds these under the cluster they belong to, not
	// under an environment overlay it does not have (ADR-072). The registry
	// describes the platform's layout, so the paths are rewritten rather than
	// declared twice -- a second registry for tenants would be a second thing to
	// keep correct, and the artifact set is the same either way.
	if o.GitopsDir != "" {
		if o.ClusterName == "" {
			return reg, fmt.Errorf("ADR-045 artifacts belong to one cluster, and " +
				"no cluster is named; their location in the tenant's repository " +
				"cannot be resolved")
		}
		for i := range reg.Artifacts {
			// Everything below generated/, not just the file name.
			//
			// filepath.Base threw the rest away, so an artifact declared at
			// generated/values/infisical-identity.yaml relocated to
			// generated/infisical-identity.yaml -- one directory above where the
			// writers put it and where bundle.yaml's valueFiles reads it. The
			// validator then reported a file that had been written correctly as
			// missing, naming a path nothing writes.
			_, below, ok := strings.Cut(reg.Artifacts[i].File, "/generated/")
			if !ok {
				return reg, fmt.Errorf("ADR-045 artifact %q is not under a "+
					"generated/ directory, so its place in the tenant's repository "+
					"cannot be derived", reg.Artifacts[i].File)
			}
			reg.Artifacts[i].File = filepath.Join(tenant.RegistryDir, "clusters",
				o.ClusterName, "generated", below)
		}
	}
	return reg, nil
}

func (o *Orchestrator) adr045GeneratedDirs() ([]string, error) {
	reg, err := o.readADR045Registry()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var dirs []string
	for _, a := range reg.Artifacts {
		d := filepath.Dir(a.File) + "/"
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs, nil
}

// adr045Consumers returns the Applications that cannot converge until the
// registered artifacts are committed and pushed.
func (o *Orchestrator) adr045Consumers() ([]string, error) {
	reg, err := o.readADR045Registry()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var apps []string
	for _, a := range reg.Artifacts {
		if a.ConsumedBy == "" || seen[a.ConsumedBy] {
			continue
		}
		seen[a.ConsumedBy] = true
		apps = append(apps, a.ConsumedBy)
	}
	return apps, nil
}

// adr045Artifact describes a single required generated artifact.
type adr045Artifact struct {
	File string `yaml:"file"`
	// ConsumedBy names the Application that cannot converge until this file is
	// committed and pushed. Declared rather than derived: an artifact under
	// environments/base is reached by an Application reconciling
	// environments/<slug> through a Kustomize ../base reference, so matching an
	// Application's source path against the artifact's path finds nothing.
	ConsumedBy     string   `yaml:"consumedBy"`
	RequiredFields []string `yaml:"requiredFields"`
}

// adr045Registry is the top-level structure of manifests/generated/artifacts.yaml.
type adr045Registry struct {
	Artifacts []adr045Artifact `yaml:"artifacts"`
}

// validateADR045Artifacts reads manifests/generated/artifacts.yaml and validates
// that every required artifact exists on disk with all required fields populated.
// This runs after PhaseBootstrapInfisicalAPI when the CLI has generated the patches.
func (o *Orchestrator) validateADR045Artifacts(ctx context.Context) error {
	projectRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// Read through the shared reader, which resolves the {env} placeholder the
	// registry writes for per-environment artifacts. This function parsed the
	// file itself and so validated a path with the placeholder still in it,
	// reporting the artifact missing while it sat at the resolved path.
	//
	// Two readers of one registry is how that happened: the fix went into the
	// one this did not use.
	registry, err := o.readADR045Registry()
	if err != nil {
		return err
	}

	if len(registry.Artifacts) == 0 {
		fmt.Println("[adr045-validate] ⚠️  No artifacts registered in artifacts.yaml")
		return nil
	}

	if o.GitopsDir != "" {
		projectRoot = o.GitopsDir
	}

	for _, a := range registry.Artifacts {
		absPath := filepath.Join(projectRoot, a.File)

		// Check file exists
		data, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Errorf("artifact %s: file not found — ensure BootstrapInfisicalAPI completed successfully", a.File)
		}

		// Parse the patch YAML
		var doc map[string]interface{}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("artifact %s: invalid YAML: %w", a.File, err)
		}

		// Verify each required field
		for _, field := range a.RequiredFields {
			parts := strings.Split(field, ".")
			current := doc
			found := true

			for i, part := range parts {
				val, ok := current[part]
				if !ok {
					found = false
					break
				}
				if i == len(parts)-1 {
					// Last part — present and non-empty.
					//
					// A required field is not always a scalar. The gateway's
					// external-dns target is declared as metadata.annotations,
					// a MAP, because the key underneath it
					// (external-dns.alpha.kubernetes.io/target) contains dots and
					// cannot be addressed by a dot-separated path. Asserting
					// val.(string) here rejected that artifact as "missing or
					// empty" while it was present and correct.
					switch v := val.(type) {
					case string:
						found = strings.TrimSpace(v) != ""
					case map[string]interface{}:
						found = len(v) > 0
					case []interface{}:
						found = len(v) > 0
					default:
						found = val != nil
					}
				} else {
					// Intermediate — must be a map
					next, ok := val.(map[string]interface{})
					if !ok {
						found = false
						break
					}
					current = next
				}
			}

			if !found {
				return fmt.Errorf("artifact %s: required field %s is missing or empty", a.File, field)
			}
		}

		fmt.Printf("[adr045-validate]   ✓ %s (%d fields)\n", a.File, len(a.RequiredFields))
	}

	return nil
}

// ──────────────────────────────────────────────────────────────────────────
// ADR-045: auto-commit + wait for ArgoCD
// ──────────────────────────────────────────────────────────────────────────

// commitGeneratedArtifacts validates the ADR-045 artifacts and gets them into
// the tenant's repository, where the phases that consume them can read them.
//
// Failure is fatal, which it was not when this ran after the boundaries. Then,
// nothing later in the run depended on it and "the user can commit manually" was
// a reasonable thing to print. Now boundary 04 reads these values through ArgoCD
// on the next line, so a warning here buys a 45-minute timeout on a condition
// that cannot arrive, reported against the hub-operator rather than against the
// commit that never happened.
func (o *Orchestrator) commitGeneratedArtifacts(ctx context.Context) error {
	fmt.Println("\n[adr045-commit] Validating and committing ADR-045 artifacts...")
	if err := o.validateADR045Artifacts(ctx); err != nil {
		return fmt.Errorf("[adr045-commit] %w", err)
	}

	// Derived from the registry rather than listed again beside it.
	//
	// This was a second hard-coded list of the same paths, and the two drifted
	// the moment one artifact stopped being generated: the registry no longer
	// named manifests/hub-core-services/security/generated/, this list still did,
	// and `git add` on a directory that no longer exists fails the whole commit
	// step with exit status 128 -- taking the artifacts that ARE still generated
	// down with it. One declaration means removing an artifact removes it here
	// too.
	generatedPaths, err := o.adr045GeneratedDirs()
	if err != nil {
		return fmt.Errorf("[adr045-commit] %w", err)
	}

	if err := o.gitCommitArtifacts(ctx, generatedPaths); err != nil {
		return fmt.Errorf("[adr045-commit] the generated artifacts could not be "+
			"committed, and boundary 04 reads them from git: %w\n\n"+
			"  Commit them and re-run; the bootstrap resumes from this phase:\n"+
			"    cd %s\n"+
			"    git add %s\n"+
			"    git commit -m \"chore: bootstrap-generated-gitops-artifacts [skip ci]\"\n"+
			"    git push",
			err, o.GitopsDir, strings.Join(generatedPaths, " "))
	}
	return nil
}

// gitCommitArtifacts runs git add + git commit for the generated artifact
// directories. It only requires local Git — no remote access. If git is
// unavailable or the working tree is dirty, it returns an error but does
// not halt the bootstrap (the user can commit manually).
func (o *Orchestrator) gitCommitArtifacts(ctx context.Context, paths []string) error {
	// Check if git is available
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found: %w", err)
	}

	// In the tenant's repository when bootstrapping one (ADR-072). Day-0 is the
	// single write the platform's tooling makes outside a pull request, and it is
	// bounded: it happens during a bootstrap the tenant invoked, to a repository
	// with no cluster yet, before anything is running to be affected by it.
	git := func(args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, "git", args...)
		if o.GitopsDir != "" {
			c.Dir = o.GitopsDir
		}
		return c
	}

	for _, p := range paths {
		cmd := git("add", p)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git add %s: %w\n%s", p, err, out)
		}
	}

	// git commit (idempotent — fails cleanly if nothing to commit)
	// Asked of git's index, not of its prose.
	//
	// This matched the string "nothing to commit", which is one of at least two
	// things git says: a tree with nothing staged and no untracked files reports
	// "nothing to commit, working tree clean", but the same tree with an untracked
	// file reports "nothing added to commit but untracked files present" -- no
	// substring in common. So a resumed run whose artifacts were already committed
	// failed here, and the message it failed with was git explaining that there
	// was nothing wrong.
	//
	// `git diff --cached --quiet` exits 0 when the index holds no changes. It is
	// the same question, asked of the only thing that actually knows, and it does
	// not change wording between git versions or locales.
	if out, err := git("diff", "--cached", "--quiet").CombinedOutput(); err == nil {
		_ = out
		// Nothing new to commit is the resume case, and it does NOT mean there is
		// nothing to do: the run that committed these may have failed to push, and
		// ArgoCD reads the remote. Falling through to the push is what lets a
		// resumed run finish the job the last one started.
		fmt.Println("[adr045-commit]   Nothing new to commit; ensuring the remote has it")
	} else {
		cmd := git("commit", "-m",
			"chore: bootstrap-generated-gitops-artifacts [skip ci]")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git commit: %w\n%s", err, out)
		}
		fmt.Println("[adr045-commit]   ✓ Generated artifacts committed locally")
	}

	// Try git pull --rebase before pushing to avoid non-fast-forward errors
	// (e.g. if another agent or user pushed commits to this branch while we were bootstrapping)
	pullCtx, pullCancel := context.WithTimeout(ctx, 60*time.Second)
	defer pullCancel()
	// Their own timeouts, so they take the directory explicitly rather than
	// through the helper above -- which is how they were missed when the rest of
	// this function moved to the tenant's repository, and would have committed a
	// tenant's artifacts locally and pushed the platform's repository instead.
	pullCmd := exec.CommandContext(pullCtx, "git", "pull", "--rebase")
	if o.GitopsDir != "" {
		pullCmd.Dir = o.GitopsDir
	}
	if out, err := pullCmd.CombinedOutput(); err != nil {
		fmt.Printf("[adr045-commit]   ⚠️  Auto-pull (rebase) failed, continuing to push: %s\n", strings.TrimSpace(string(out)))
	}

	// The push is the point of the whole step, so its failure is returned.
	//
	// It was a warning, from when this ran after every phase that could care. It
	// no longer does: ArgoCD renders what is in the REMOTE, so an artifact that
	// is committed locally and not pushed is, to every consumer of it, absent.
	// Reporting that as a warning and continuing is what turns a one-line git
	// error into a 45-minute timeout somewhere else.
	pushCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	pushCmd := exec.CommandContext(pushCtx, "git", "push")
	if o.GitopsDir != "" {
		pushCmd.Dir = o.GitopsDir
	}
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("[adr045-commit]   ✓ Generated artifacts pushed")

	return nil
}

// waitForArgoCDAppsHealthy polls ArgoCD Application resources until all
// specified apps report Sync=Synced and Health=Healthy, or the context
// expires. Uses kubectl with the provided kubeconfig.
func (o *Orchestrator) waitForArgoCDAppsHealthy(ctx context.Context, kubeconfig string, appNames []string) error {
	// helper to check a single app
	checkApp := func(app string) (synced, healthy bool, err error) {
		syncCmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"get", "application", app, "-n", "platform-ops",
			"-o", "jsonpath={.status.sync.status}")
		syncOut, syncErr := syncCmd.Output()
		if syncErr != nil {
			return false, false, fmt.Errorf("get sync status: %w", syncErr)
		}
		synced = strings.TrimSpace(string(syncOut)) == "Synced"

		healthCmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"get", "application", app, "-n", "platform-ops",
			"-o", "jsonpath={.status.health.status}")
		healthOut, healthErr := healthCmd.Output()
		if healthErr != nil {
			return false, false, fmt.Errorf("get health status: %w", healthErr)
		}
		healthy = strings.TrimSpace(string(healthOut)) == "Healthy"
		return synced, healthy, nil
	}

	getStatus := func(app string) string {
		syncOut, _ := exec.CommandContext(context.Background(), "kubectl",
			"--kubeconfig", kubeconfig,
			"get", "application", app, "-n", "platform-ops",
			"-o", "jsonpath={.status.sync.status}").Output()
		healthOut, _ := exec.CommandContext(context.Background(), "kubectl",
			"--kubeconfig", kubeconfig,
			"get", "application", app, "-n", "platform-ops",
			"-o", "jsonpath={.status.health.status}").Output()
		return fmt.Sprintf("sync=%s health=%s",
			strings.TrimSpace(string(syncOut)), strings.TrimSpace(string(healthOut)))
	}

	pollTicker := time.NewTicker(15 * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			var failures []string
			for _, app := range appNames {
				synced, healthy, _ := checkApp(app)
				if !synced || !healthy {
					failures = append(failures, fmt.Sprintf("%s: %s", app, getStatus(app)))
				}
			}
			if len(failures) > 0 {
				return fmt.Errorf("timeout waiting for ArgoCD apps:\n  %s",
					strings.Join(failures, "\n  "))
			}
			return ctx.Err()

		case <-pollTicker.C:
			allHealthy := true
			for _, app := range appNames {
				synced, healthy, err := checkApp(app)
				if err != nil {
					fmt.Printf("[adr045-commit]   ⏳ %s: error (%v), retrying...\n", app, err)
					allHealthy = false
					continue
				}
				if !synced || !healthy {
					fmt.Printf("[adr045-commit]   ⏳ %s: %s\n", app, getStatus(app))
					allHealthy = false
				}
			}
			if allHealthy {
				return nil
			}
		}
	}
}
