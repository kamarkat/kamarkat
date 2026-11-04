package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
)

// ──────────────────────────────────────────────────────────────────────────
// ADR-055: Boundary Activation as the Day-0 Gating Mechanism
//
// Boundary CONTENT is reconciled from Git by a single seed Application.
// Boundary ACTIVATION is Day-0 state: a deny sync window on the boundary's
// AppProject. Opening a boundary removes that window; nothing else changes.
//
// The two authorities act on disjoint objects — the seed Application owns the
// ApplicationSets, the CLI owns the AppProjects' sync windows — so neither can
// overwrite the other. That is what makes the pre-ADR-055 failure mode (a
// second render clobbering a correct ApplicationSet) unrepresentable rather
// than merely unlikely.
// ──────────────────────────────────────────────────────────────────────────

// GatingMode selects how a cluster is created. Mode and environment are
// independent inputs: every environment class may be created in either mode.
type GatingMode string

const (
	// GatingSequenced is the default. Every boundary is inactive at creation
	// and is activated in phase order behind the readiness gates that already
	// precede it. Deterministic ordering; a failure is attributable to a phase.
	GatingSequenced GatingMode = "sequenced"

	// GatingConverged creates no inactive boundaries. All boundaries reconcile
	// concurrently and converge through ArgoCD retry. No ordering guarantee; a
	// failure is attributable only to the Application that failed.
	GatingConverged GatingMode = "converged"
)

// ValidGatingMode reports whether s names a mode, and normalises it.
func ValidGatingMode(s string) (GatingMode, error) {
	switch GatingMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", GatingSequenced:
		return GatingSequenced, nil
	case GatingConverged:
		return GatingConverged, nil
	default:
		return "", fmt.Errorf(
			"invalid gating mode %q (expected sequenced|converged) — refusing to guess how to create the cluster", s)
	}
}

// seedAppName is the single Day-0 object that hands boundary content to ArgoCD.
const seedAppName = "platform-boundaries"

// boundaryCount is the number of gated boundaries (ADR-021, ADR-047, ADR-051).
const boundaryCount = 6

// boundaryProject returns the AppProject that scopes boundary n and carries its
// activation state.
func boundaryProject(n int) string {
	return fmt.Sprintf("boundary-%02d", n)
}

// boundaryDescriptions are used only for operator legibility in the ArgoCD UI.
var boundaryDescriptions = map[int]string{
	1: "Boundary 01 — platform infrastructure (ArgoCD, operators, CRDs)",
	2: "Boundary 02 — platform data workloads (CNPG, Redis)",
	3: "Boundary 03 — platform services (Infisical, hub Gateway, platform APIs)",
	4: "Boundary 04 — tenant services (identity, billing, auth proxy)",
	5: "Boundary 05 — fleet provisioning (ADR-047)",
	6: "Boundary 06 — public tenant TLS (ADR-051)",
}

// renderBoundaryAppProject renders one boundary AppProject.
//
// The policy is deliberately permissive and matches what these Applications
// already have under the `default` project. Introducing the project boundary
// and tightening its policy are two changes with different blast radii; this
// is the first. Narrowing sourceRepos/destinations/resource kinds is follow-up
// work and must be validated against a live boundary before it lands.
//
// When gated, the project carries a deny sync window. ArgoCD evaluates windows
// continuously, so a boundary stays inactive until the window is removed — the
// Applications exist and report divergence but are not permitted to sync.
// The schedule/duration pair is the documented encoding for a window that is
// always active: a window opens every minute and lasts a day.
func renderBoundaryAppProject(n int, gated bool) string {
	var window string
	if gated {
		window = `
  # ADR-055: boundary activation state. Present = inactive. Owned by Day-0 and
  # removed when this boundary's phase opens it. Deliberately NOT reconciled
  # from Git: this field is the Day-0/Day-1 lifecycle boundary (ADR-040).
  # '* * * * *' for 24h is the encoding for "always active" — a window opens
  # every minute and outlives the next, so the deny never lapses.
  syncWindows:
    - kind: deny
      schedule: '* * * * *'
      duration: 24h
      applications:
        - '*'
      manualSync: false
      timeZone: UTC`
	}

	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: %s
  namespace: platform-ops
spec:
  description: %s
  sourceRepos:
    - '*'
  destinations:
    - namespace: '*'
      server: '*'
  clusterResourceWhitelist:
    - group: '*'
      kind: '*'
  namespaceResourceWhitelist:
    - group: '*'
      kind: '*'%s
`, boundaryProject(n), boundaryDescriptions[n], window)
}

// renderSeedApplication renders the single Application that owns all boundary
// ApplicationSets. Once applied it reconciles continuously with self-heal, so a
// boundary element added in Git reaches the cluster without any bootstrap action
// — which is the defect ADR-055 exists to remove.
//
// The matrix values are carried as Helm parameters on the Application rather
// than passed to a one-shot render. They are the cluster's identity, fixed at
// creation, and being part of a reconciled object they survive rather than
// having to be re-supplied by whoever last ran a render.
// infisicalCoordinates returns the PKI project and machine-identity client id
// that this hub's ClusterIssuers authenticate to Infisical with.
//
// They are read back from the infisical-auth Secret the secrets phase writes
// into platform-security, rather than carried forward in memory, so a resumed
// bootstrap that skips that phase still renders a complete seed.
//
// This replaces a Kustomize patch that the secrets phase wrote into
// manifests/hub-core-services/security/generated/ and committed. That placed one
// cluster's PKI project inside the types repository, which ADR-062 forbids and
// which made the component impossible to publish: every cluster pulling the
// chart would have received this cluster's project.
//
// Empty strings are returned when the Secret cannot be read. The consuming chart
// requires both through values.schema.json, so the boundary refuses to render
// rather than installing an issuer pointed at nothing.
func (o *Orchestrator) infisicalCoordinates(ctx context.Context, kubeconfig string) (projectID, clientID string) {
	read := func(key string) string {
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"get", "secret", "infisical-auth", "-n", "platform-security",
			"-o", "jsonpath={.data."+key+"}").Output()
		if err != nil {
			return ""
		}
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(dec))
	}
	return read("projectId"), read("client-id")
}

// readSeedBundleVersion returns the bundle version already recorded on this
// cluster's seed Application, or "" when there is none to read.
//
// An unreadable seed and an absent one are the same answer here on purpose:
// both mean nothing on the cluster claims a version, and the caller then seeds
// the build's. The failure this guards against is overwriting a version that IS
// there, which requires having read it.
func readSeedBundleVersion(ctx context.Context, kubeconfig string) string {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"-n", "platform-ops", "get", "application", seedAppName,
		"-o", `jsonpath={.spec.source.helm.parameters[?(@.name=="bundleVersion")].value}`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// bundleRegistry is where a released bundle is published. It matches
// environment-manager's own default, and is here rather than passed because the
// seed and the chart must name the same registry -- a seed pointing at one
// registry and a chart at another would resolve two different distributions.
// Where a released bundle is published.
//
// No oci:// scheme: ArgoCD pulls a scheme-less registry host as an OCI artefact
// and passes anything else to `helm pull --repo`, which does not speak OCI.
//
// The seed supplies this to the chart rather than letting the chart default it.
// A published chart carries the default it was built with, so correcting the
// default only reaches clusters that install a later bundle -- and a cluster
// already running the defective one cannot be repaired by a reseed, which is
// the operation that exists to repair exactly this. The CLI knows which
// registry it fetched from; the chart should not have to guess.
const bundleRegistry = "ghcr.io/kamarkat/charts"

func renderSeedApplication(envRevision, envSlug, provider, topology, hubIngressAddress, publicTlsIssuer, oidcIssuer, oidcJwksURL, infisicalProjectID, infisicalClientID, bundleVersion, instanceRepoURL, hubDomain string, oidcScopes []string) string {
	scopes := ""
	for _, sc := range oidcScopes {
		scopes += fmt.Sprintf("\n        - %q", sc)
	}
	// Where the seed reads the chart from follows the same rule as everything
	// the chart goes on to render (ADR-068): an unreleased build sources from
	// the working tree, a released one from the published distribution.
	//
	// The seed used to name git unconditionally, and a released run therefore
	// asked git for a chart that only renders when it carries the descriptors
	// copied in at package time. The render failed, no ApplicationSet was ever
	// created, and the boundary reported that it had been activated but never
	// rendered -- true, and pointing at the boundary rather than at the seed.
	source := fmt.Sprintf(`    repoURL: https://github.com/kamarkat/kamarkat
    targetRevision: %q
    path: manifests/argocd/environment-manager`, envRevision)
	if bundleVersion != versions.DevelopmentBundle {
		source = fmt.Sprintf(`    repoURL: %q
    targetRevision: %q
    chart: environment-manager`, bundleRegistry, bundleVersion)
	}

	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
  namespace: platform-ops
spec:
  project: default
  source:
%s
    helm:
      parameters:
        - name: environmentRevision
          value: %q
        - name: bundleVersion
          value: %q
        - name: bundleRegistry
          value: %q
        - name: instanceRepoURL
          value: %q
        - name: hubDomain
          value: %q
        - name: environmentSlug
          value: %q
        - name: provider
          value: %q
        - name: topology
          value: %q
        - name: hubIngressAddress
          value: %q
        - name: publicTlsIssuer
          value: %q
        - name: oidcIssuer
          value: %q
        - name: oidcJwksUrl
          value: %q
        - name: infisical.fleet.projectId
          value: %q
        - name: infisical.fleet.clientId
          value: %q
      valuesObject:
        oidcScopes:%s
  destination:
    server: https://kubernetes.default.svc
    namespace: platform-ops
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - ServerSideApply=true
`, seedAppName, source, envRevision, bundleVersion, bundleRegistry, instanceRepoURL, hubDomain, envSlug, provider, topology, hubIngressAddress, publicTlsIssuer, oidcIssuer, oidcJwksURL,
		infisicalProjectID, infisicalClientID, scopes)
}

// applySeed establishes the Day-0 seed: the six boundary AppProjects and the
// seed Application. It is idempotent and safe to call from every boundary phase.
//
// Ordering is forced and load-bearing. The AppProjects must exist before the
// seed Application creates any ApplicationSet, because the generated
// Applications name those projects — and because in sequenced mode the deny
// windows must be in place before the first Application can attempt to sync.
func (o *Orchestrator) applySeed(ctx context.Context, kubeconfig string) error {
	gated := o.gatingMode() == GatingSequenced

	var projects strings.Builder
	for n := 1; n <= boundaryCount; n++ {
		projects.WriteString("---\n")
		projects.WriteString(renderBoundaryAppProject(n, gated))
	}
	if err := kubectlApplyStdin(ctx, kubeconfig, projects.String()); err != nil {
		return fmt.Errorf("failed to apply boundary AppProjects: %w", err)
	}

	return o.applySeedApplication(ctx, kubeconfig)
}

// applySeedApplication renders and applies the seed Application alone.
//
// Separated from the AppProjects because the two have different lifetimes. The
// AppProjects carry boundary activation state, and in sequenced mode restating
// them re-applies every deny sync window — correct during a bootstrap that
// activates each boundary afterwards, and a fleet-wide freeze on a cluster that
// is already running. The seed Application carries no such state and can be
// restated at any time.
func (o *Orchestrator) applySeedApplication(ctx context.Context, kubeconfig string) error {
	// The identity provider tenants authenticate against (ADR-050). Derived from
	// the environment's own zone rather than configured separately: the issuer IS
	// an endpoint this bootstrap already computes, and a second source for it is a
	// way for the two to disagree.
	//
	// Supplied here because it is environment identity, not fleet data — a fleet
	// able to set it could point its login flow at an issuer this platform does
	// not trust.
	//
	// .ID (Zitadel), not .Auth (Hydra). The organisation is the tenant in Zitadel:
	// it OWNS the user rather than describing it, so a user with no tenant is not
	// expressible and a token either carries an organisation or is rejected. Under
	// Hydra the tenant was a claim written onto an identity, which is what allowed
	// identities to exist with none and produced repeated "Token missing required
	// tenant_id claim" failures at the BFF.
	//
	// Ory keeps serving auth.<zone> and is not removed here. Rolling back is
	// pointing this line at .Auth again, which is why the endpoint set carries
	// both rather than renaming one.
	zone, err := ReadHubZone(".", o.EnvironmentSlug)
	if err != nil {
		return fmt.Errorf("failed to read hub zone for the OIDC issuer: %w", err)
	}
	oidcIssuer := "https://" + DeriveHubEndpoints(zone).ID

	// Zitadel publishes its signing keys at /oauth/v2/keys and returns 404 for
	// the conventional /.well-known/jwks.json. Passed explicitly rather than
	// left to the chart's default, which is Hydra's path: appending the
	// conventional suffix to a Zitadel issuer fails as
	//
	//   Token validation failed: Expected 200 OK from the JSON Web Key Set HTTP response
	//
	// which names neither the path nor the provider and reads as an unreachable
	// issuer. Tied to the same issuer variable so the two cannot disagree.
	oidcJwksURL := oidcIssuer + "/oauth/v2/keys"

	// Scopes the issuer needs beyond openid/profile/email, rendered as a YAML
	// list into the seed's values.
	//
	// Supplied here rather than defaulted in the chart because these identifiers
	// are the issuer's vocabulary, and a platform template that named them would
	// have to be edited to change provider (ADR-059).
	//
	// Omitting one is SILENT: the token still verifies, it simply arrives without
	// the claim that scope mints, and the failure surfaces much later as a
	// missing tenant or an empty role list rather than as a login error.
	oidcScopes := []string{
		// tenancy — the issuer emits the owning organisation only when asked
		"urn:zitadel:iam:user:resourceowner",
		// authorisation — project roles granted to the user in that organisation
		"urn:zitadel:iam:org:project:roles",
	}

	publicTlsIssuer, err := o.publicTlsIssuerFor()
	if err != nil {
		return err
	}

	// Resolved for the CLI-rendered seed only. On a tenant box (ADR-072) the
	// address is NOT carried in from Day-0 at all: hub-operator reads it from the
	// HetznerCluster in-cluster and publishes it, so a bundle upgrade alone
	// delivers the fix. See operators/hub-operator -- ingress address.
	hubIngressAddress := o.hubIngressAddress(ctx, kubeconfig)
	// The address is passed to the chart as hubIngressAddress, which the gateway
	// component reads as its external-dns target. It used to be written into the
	// repository as a Kustomize patch and committed -- one cluster's
	// load-balancer address in the types repository, inherited by every other
	// cluster that rendered from it, and the reason that component could not be
	// published (ADR-062, ADR-063).

	envRevision := "main"
	if b := currentGitBranch(); b != "" && b != "main" {
		envRevision = b
	}

	// Day-0 seeds the version; the cluster owns it afterwards (ADR-068).
	//
	// This function is the seed renderer, and it runs again after Day-0: to
	// supply the Infisical coordinates discovered later in the pipeline, to
	// resume an interrupted bootstrap, and through `hub reseed`. Writing the
	// build's version unconditionally would return a cluster promoted to a newer
	// bundle (ADR-064) to whatever the operator's binary happened to carry --
	// a downgrade with no diff, no error, and nothing recording what moved it.
	//
	// So a version already on the cluster wins, and the build's is used only
	// when there is none: the first apply, which is what seeding means.
	bundleVersion := versions.BundleVersion
	existing := readSeedBundleVersion(ctx, kubeconfig)
	switch {
	case o.BundleVersionOverride != "":
		// Asked for by name. Reported against what it replaces, because the
		// number that changed is the one thing an operator needs to see and the
		// one thing a silent success would hide.
		if existing != "" && existing != o.BundleVersionOverride {
			fmt.Printf("[seed] moving cluster from bundle %s to %s (requested)\n",
				existing, o.BundleVersionOverride)
		} else {
			fmt.Printf("[seed] pinning bundle %s (requested)\n", o.BundleVersionOverride)
		}
		bundleVersion = o.BundleVersionOverride
	case existing != "":
		if existing != bundleVersion {
			fmt.Printf("[seed] cluster runs bundle %s; leaving it (this build carries %s)\n",
				existing, bundleVersion)
		}
		bundleVersion = existing
	}

	infisicalProjectID, infisicalClientID := o.infisicalCoordinates(ctx, kubeconfig)
	if infisicalProjectID == "" || infisicalClientID == "" {
		fmt.Println("[seed] warning: infisical-auth unreadable; the security boundary will refuse to render until it is")
	}

	// A tenant's cluster is seeded with the Application its own repository
	// declares, not one rendered here (ADR-072). The chart, the version and the
	// values all come from that repository, which is what makes it authoritative
	// rather than merely present -- and what makes ADR-064's promotion take
	// effect, since the field a proposal changes is then the field the cluster
	// reads.
	if o.GitopsDir != "" {
		// The hub's ingress address, recorded in the tenant's repository before the
		// declaration that reads it is applied. See gateway_dns_target.go: on a
		// tenant box the seed is the one that repository declares, so a value the
		// CLI injects into its OWN rendered seed reaches nothing.
		if err := o.writeGatewayDNSTarget(ctx, kubeconfig); err != nil {
			return err
		}
		return o.applyTenantSeed(ctx, kubeconfig)
	}

	// Every box declares its own instance repository (ADR-062). There is no
	// default to fall back to: a fallback would be some other box's repository,
	// which is exactly how one box's identity became every box's.
	instanceRepo, err := o.instanceRepoURL()
	if err != nil {
		return err
	}

	// The zone read above IS the hub domain: ADR-051 makes the environment
	// overlay the system of record, and every public host derives from it. The
	// platform's own box takes it from there; a tenant box takes it from the
	// values file its repository holds, which is why this path is the only one
	// that has to name it.
	seed := renderSeedApplication(envRevision, o.EnvironmentSlug, o.providerName(),
		o.Topology, hubIngressAddress, publicTlsIssuer, oidcIssuer, oidcJwksURL,
		infisicalProjectID, infisicalClientID, bundleVersion, instanceRepo, zone, oidcScopes)
	if err := kubectlApplyStdin(ctx, kubeconfig, seed); err != nil {
		return fmt.Errorf("failed to apply the seed Application: %w", err)
	}
	return nil
}

// applyTenantSeed applies the Application a tenant's repository declares for
// this cluster.
//
// Nothing is rendered and nothing is substituted. What the repository says is
// what the cluster gets, because a seed the platform adjusted on the way through
// would leave the repository describing something other than what runs.
func (o *Orchestrator) applyTenantSeed(ctx context.Context, kubeconfig string) error {
	if o.ClusterName == "" {
		return fmt.Errorf("cannot seed from %s: no cluster name, so there is no "+
			"declaration to choose", o.GitopsDir)
	}

	// Both, and in this order.
	//
	// bundle.yaml first so the platform Application exists immediately and the
	// boundary phases below have something to wait on -- routing it through
	// ArgoCD first would mean waiting for a sync before Day-0 could continue.
	//
	// root.yaml second, and it is the point: it makes ArgoCD the owner of
	// bundle.yaml from here on, so a merged promotion moves the cluster. Without
	// it the platform Application was applied once and reconciled by nothing, and
	// ADR-064's promotion stopped at the pull request. Applying both is safe --
	// the root adopts the Application the first apply just created, because they
	// name the same object.
	for _, f := range []string{"bundle.yaml", "root.yaml"} {
		path := filepath.Join(o.GitopsDir, tenant.RegistryDir, "clusters", o.ClusterName, f)
		seed, err := os.ReadFile(path)
		if err != nil {
			// root.yaml postdates the repositories scaffolded before it. A box
			// without one still reconciles what it has; it just does not pick up
			// promotions until it is re-scaffolded, which is what it had before.
			if f == "root.yaml" && os.IsNotExist(err) {
				fmt.Printf("[seed] %s has no root.yaml: promotions will not reconcile "+
					"on their own until this repository is re-scaffolded (ADR-064)\n",
					o.GitopsDir)
				continue
			}
			return fmt.Errorf("the tenant repository declares no cluster %q: %w.\n"+
				"A cluster is bootstrapped from the declaration its own repository "+
				"holds (ADR-072); scaffold it there before bootstrapping it",
				o.ClusterName, err)
		}

		if bytes.Contains(seed, []byte("<")) && bytes.Contains(seed, []byte(">")) {
			if m := unsubstituted.Find(seed); m != nil {
				return fmt.Errorf("%s still carries the placeholder %s: it is a "+
					"template rather than a hydrated cluster, and applying it would "+
					"create an Application naming nothing", path, m)
			}
		}

		fmt.Printf("[seed] applying the declaration in %s\n", path)
		if err := kubectlApplyStdin(ctx, kubeconfig, string(seed)); err != nil {
			return fmt.Errorf("failed to apply %s: %w", path, err)
		}
	}
	return nil
}

// unsubstituted matches a scaffolding placeholder. A template copied into
// clusters/ without hydration renders an Application whose source names a
// literal <BUNDLE_VERSION>, which ArgoCD accepts and then cannot resolve.
var unsubstituted = regexp.MustCompile(`<[A-Z_]{3,}>`)

// ReapplySeed brings an existing cluster's seed Application to what the
// renderer above defines.
//
// The seed Application is written once, at bootstrap. Adding a parameter to
// renderSeedApplication therefore changes nothing on a cluster already running:
// the live Application keeps the parameter set it was created with, the
// environment-manager chart renders without the new value, and every
// Application generated from it fails comparison. That failure does not present
// as a missing parameter — it presents as ComparisonError, which ArgoCD reports
// as sync status Unknown, and auto-sync only fires on OutOfSync. The change
// appears pushed, the Application appears healthy, and nothing deploys. This is
// how the per-tenant gateway sat undeployed after being committed.
//
// The alternative is patching the live Application by hand, which loses the fix
// the moment anyone reads the repository as the source of truth. This exists so
// the renderer stays the single definition of the seed and an existing cluster
// can be brought to it — the same manifest, applied, rather than a second one
// typed at a terminal.
//
// Only the seed Application is applied. Boundary activation is Day-0 state
// (ADR-040, ADR-055) and restating it here would re-gate boundaries that have
// been opened.
func (o *Orchestrator) ReapplySeed(ctx context.Context, kubeconfig string) error {
	return o.applySeedApplication(ctx, kubeconfig)
}

// ReadSeedIdentity returns the environment, provider and topology recorded on a
// cluster's existing seed Application.
//
// These three name the cluster. Re-supplying them by hand to restate the seed
// invites supplying one of them differently, and the failure is quiet: topology
// is a path segment, so a value that merely looks reasonable repoints the spoke
// pool ApplicationSet at a directory that does not exist, and the Applications
// generated from it go missing rather than erroring. Reading them back from the
// cluster removes the opportunity.
func ReadSeedIdentity(ctx context.Context, kubeconfig string) (envSlug, provider, topology string, err error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"-n", "platform-ops", "get", "application", seedAppName,
		"-o", `jsonpath={range .spec.source.helm.parameters[*]}{.name}={.value}{"\n"}{end}`)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to read the seed Application %q: %w: %s",
			seedAppName, err, strings.TrimSpace(stderr.String()))
	}

	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			values[name] = value
		}
	}

	envSlug, provider = values["environmentSlug"], values["provider"]
	if envSlug == "" || provider == "" {
		return "", "", "", fmt.Errorf(
			"the seed Application %q records no environmentSlug or provider — refusing to guess the cluster's identity", seedAppName)
	}
	// topology is legitimately empty on a single-topology cluster, so its
	// absence is not an error and must not be defaulted.
	return envSlug, provider, values["topology"], nil
}

// activateBoundary opens boundary n by removing its deny sync window.
//
// Idempotent by construction: a merge patch setting the field to null removes it
// if present and is a no-op if absent, so a resumed bootstrap re-opening an
// already-open boundary mutates nothing. This is the contrast with the previous
// mechanism, where re-entering a phase restated the entire boundary.
func (o *Orchestrator) activateBoundary(ctx context.Context, kubeconfig string, n int) error {
	if o.gatingMode() == GatingConverged {
		// Nothing was ever closed. Boundary phases remain in the sequence and
		// still report, so the bootstrap's account of itself is unchanged.
		return nil
	}
	// Cumulative: boundaries 1..n, not n alone.
	//
	// applySeed runs at the START of every boundary phase and restates all six
	// AppProjects with their deny window, which re-freezes every boundary
	// activated so far. Opening only boundary n then leaves exactly one boundary
	// open — the current one — and the previous ones closed behind it.
	//
	// During the run that is invisible, because each phase does its work while
	// its own boundary is open. It surfaces afterwards: the bootstrap finishes
	// with boundaries 01-05 denied and only 06 open, so nothing in them can ever
	// sync again. A boundary 01 Application that needs to pick up a credential
	// rotated later sits OutOfSync-but-Healthy for ever, with the deny window as
	// the only evidence and nothing pointing at it.
	//
	// Re-opening 1..n restores the invariant the sequence is supposed to express:
	// every boundary reached so far is open, and the ones beyond n are not.
	for i := 1; i <= n; i++ {
		if err := patchWithTransportRetry(ctx, kubeconfig, boundaryProject(i)); err != nil {
			return fmt.Errorf("failed to activate boundary %02d: %w", i, err)
		}
	}
	return nil
}

// patchWithTransportRetry opens one boundary, retrying while the fault is the
// connection rather than the request.
//
// The hub's API server is reached over the public internet -- the kubeconfig
// names the CAPH load balancer, not the tailnet -- so a single kubectl can fail
// on the network while the cluster is perfectly healthy. One did:
//
//	[boundary03] ✗ failed after 47s
//	failed to activate boundary 01: exit status 1
//	Unable to connect to the server: net/http: TLS handshake timeout
//
// at a moment when the control plane's four static pods had zero restarts
// between them. Ninety minutes of bootstrap ended on a handshake.
//
// Retried rather than given a longer timeout, which was the first instinct and
// is the wrong lever twice over. TLSHandshakeTimeout is a client-go transport
// constant with no kubectl flag (--request-timeout governs the request, not the
// handshake), and a handshake that has not completed in ten seconds is not one
// that needed eleven -- waiting longer makes the failure slower, not rarer.
//
// Safe to repeat by construction, and the function above says why: the patch
// sets syncWindows to null, which removes the field if present and does nothing
// if absent. Re-running it on an already-open boundary mutates nothing.
func patchWithTransportRetry(ctx context.Context, kubeconfig, project string) error {
	const attempts = 5

	var lastOut []byte
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"patch", "appproject", project, "-n", "platform-ops",
			"--type", "merge", "-p", `{"spec":{"syncWindows":null}}`)
		out, err := cmd.CombinedOutput()
		if err == nil {
			if attempt > 1 {
				fmt.Printf("[seed] ✓ %s opened on attempt %d\n", project, attempt)
			}
			return nil
		}
		lastOut, lastErr = out, err

		// A boundary that does not exist, or a patch the API server rejects,
		// fails the same way five times.
		if !transientTransportError(string(out)) {
			break
		}
		if attempt < attempts {
			delay := time.Duration(attempt*attempt) * time.Second
			fmt.Printf("[seed] %s: %s -- retrying in %s (attempt %d/%d)\n",
				project, strings.TrimSpace(string(out)), delay, attempt, attempts)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return fmt.Errorf("%w\n%s", lastErr, lastOut)
}

// transientTransportError reports whether a kubectl failure was the connection
// rather than the request. Conservative on purpose: anything not recognised here
// is reported immediately rather than retried into a slow, confusing failure.
func transientTransportError(out string) bool {
	for _, sig := range []string{
		"TLS handshake timeout",
		"Unable to connect to the server",
		"connection refused",
		"connection reset by peer",
		"i/o timeout",
		"EOF",
		"no route to host",
		"Client.Timeout exceeded",
		"context deadline exceeded",
		"etcdserver: request timed out",
		"etcdserver: leader changed",
		"the server was unable to return a response",
		"apiserver is shutting down",
	} {
		if strings.Contains(out, sig) {
			return true
		}
	}
	return false
}

// gatingMode returns the configured mode, defaulting to sequenced. The default
// is deliberate: converged creation is requested by name or it does not occur.
func (o *Orchestrator) gatingMode() GatingMode {
	if o.Gating == GatingConverged {
		return GatingConverged
	}
	return GatingSequenced
}

// deployBoundary establishes the seed (idempotent) and opens boundary n. This is
// the operation that replaced "render a subset of the chart and apply it". The
// phase, its position, its preconditions and its reporting are unchanged.
func (o *Orchestrator) deployBoundary(ctx context.Context, kubeconfig string, n int) error {
	if err := o.applySeed(ctx, kubeconfig); err != nil {
		return err
	}
	return o.activateBoundary(ctx, kubeconfig, n)
}

// awaitBoundaryInventory holds a boundary PHASE until the boundary has generated
// the Applications it declares (ADR-061).
//
// Deliberately NOT called from deployBoundary. That function also runs inside
// Phase 5a, where it plants the seed and opens boundary 01 — and at that point
// the ApplicationSets cannot exist yet, because the seed Application can only
// render them once ArgoCD holds credentials for the platform repository, which
// Phase 5a provisions AFTER activating the boundary. Waiting there deadlocks:
// the gate waits for a generation that is waiting for the credentials the gate
// is blocking. Each boundary phase calls this itself, at a point where
// generation is actually possible.
//
// This gate exists because boundary inventory is no longer carried inside the
// applied ApplicationSet. When the elements were literal, applying the seed made
// them exist and there was nothing to wait for. Composed from descriptors, the
// inventory is the result of a repository read that can return nothing — and a
// generator that returns nothing is not an error, so without this the phase
// reports success over an empty boundary.
//
// It runs in BOTH creation modes and matters in both, differently. Sequenced:
// without it the failure surfaces later, as a subsequent phase timing out on a
// component whose Application was never created, which attributes the fault to
// the component and breaks the per-phase attribution ADR-055 says this mode
// buys. Converged: nothing was ever gated, so an Application that was never
// generated is indistinguishable from one that has not converged yet, and the
// bootstrap can complete having silently omitted a boundary.
//
// The gate only reads. Day-0 still mutates activation and nothing else.
func (o *Orchestrator) awaitBoundaryInventory(ctx context.Context, kubeconfig string, n int) error {
	waiter := &health.HealthWaiter{
		Checkers: []health.HealthChecker{
			health.NewBoundaryInventoryChecker(fmt.Sprintf("%02d", n), ""),
		},
		Interval: 10 * time.Second,
		// Generation is a repository read and a reconcile, not a deployment, so
		// this is bounded well below the readiness waits that follow it. A
		// boundary that has not produced its Applications in ten minutes has a
		// problem that waiting longer does not fix.
		Timeout: 10 * time.Minute,
	}
	return waiter.Wait(ctx, kubeconfig)
}

func kubectlApplyStdin(ctx context.Context, kubeconfig, manifest string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader([]byte(manifest))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// refreshArgoCDSchemaCache restarts the Argo CD application controller so it
// re-reads the API server's OpenAPI document.
//
// The controller loads that document once, when it starts, and uses it to build
// the structured-merge types every diff needs. A CRD registered afterwards is
// absent from that copy, and every Application managing the new kind fails to
// diff with
//
//	unable to resolve parseableType for GroupVersionKind: nutgraf.in/v1alpha1, Kind=AINativeSaaS
//
// It does NOT self-heal: the cache is refreshed on restart, not on CRD events,
// and the failure was still present more than an hour after the CRD appeared.
//
// This is structural on a hub rather than a race. The controller is installed in
// boundary 01; Crossplane composes AINativeSaaS and SpokePool from XRDs applied
// in boundary 03, and the Hub Operator registers HubEnvironment, so the types a
// later boundary manages are always invented after the controller booted.
//
// It is not a diff-strategy problem, which is worth stating because it presents
// as one: with server-side diff the message names serverSideDiff, and turning
// that off only changes it to "error calculating structured merge diff" with the
// same cause. Both strategies read the same cached schema.
//
// Idempotent and cheap: a rolling restart of a stateless controller that
// re-syncs in about half a minute, and a no-op for a cache that is already
// current.
func (o *Orchestrator) refreshArgoCDSchemaCache(ctx context.Context, kubeconfig string) error {
	fmt.Println("[boundary] Refreshing ArgoCD's API schema cache (new CRDs registered since it started)...")
	restart := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"rollout", "restart", "statefulset/argocd-application-controller",
		"-n", "platform-ops")
	if out, err := restart.CombinedOutput(); err != nil {
		return fmt.Errorf("restart argocd-application-controller: %w\n%s", err, out)
	}
	wait := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"rollout", "status", "statefulset/argocd-application-controller",
		"-n", "platform-ops", "--timeout=600s")
	if out, err := wait.CombinedOutput(); err != nil {
		return fmt.Errorf("wait for argocd-application-controller: %w\n%s", err, out)
	}
	fmt.Println("[boundary] ✓ ArgoCD schema cache refreshed")
	return nil
}
