// Package tenant implements ADR-062 onboarding: creating a tenant's
// <tenant>-gitops repository and rendering it from the template the platform
// holds.
//
// Scaffolding happens once. Everything after it is a proposal against the
// rendered repository (ADR-064), which is the whole of the difference from the
// platform this pattern is taken from: kubefirst renders a customer's repository
// and never returns to it, so a fix it publishes reaches nothing already running.
package tenant

import (
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"io/fs"
	"sort"
	"strconv"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/platform"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Spec is what onboarding is given. Every field is an identifier or a location.
//
// No field carries secret material, and the renderer refuses a token value that
// looks like one: a secret substituted here would be committed to the tenant's
// repository and would survive in its history after any later correction.
type Spec struct {
	TenantID    string
	GitOrg      string
	Domain      string
	Provider    string
	Region      string
	// Subdomain is the DNS label every hostname on this box sits under, or empty
	// for the apex. Declared, never derived: see hubDomain.
	Subdomain string
	// Workers is how many cloud workers this box starts with.
	//
	// A pointer so "not given" is distinguishable from "zero". Zero is a real
	// answer -- a development box whose capacity comes from the tenant's own
	// hardware (ADR-075) -- and defaulting an unset flag to it would silently
	// strip workers from a box that needs them.
	Workers *int
	// BoxID is the logical hub: this box, for as long as it exists (ADR-111 §1).
	//
	// Separate from ClusterName because the two have different lifetimes. The box
	// owns the escrow path and the DNS records; the cluster owns the WAL archive
	// prefix and the CAPI object names. Pinned at scaffolding and never changed,
	// for the same reason dns.ownerId is not: everything already written under it
	// is orphaned by a change.
	BoxID string
	// ClusterName is the tenant's control plane, the first cluster in the box.
	ClusterName string
	// BundleVersion is the platform version this box starts on.
	BundleVersion string
	// PlatformRevision is the platform branch a development box reads its chart
	// from. Ignored by a released box, which names a published chart instead.
	PlatformRevision string
	// PlatformRepoURL is the platform's repository. A scaffolded box does not
	// resolve it at runtime (ADR-063); it is recorded so a tenant can find the
	// source of what it runs.
	PlatformRepoURL string
	// PrivateRegistry says this tenant runs images the platform cannot pull
	// anonymously, so a registry credential is part of what the box needs.
	//
	// Declared rather than inferred: a tenant running public images must never be
	// asked for one, and the platform cannot tell from outside which they run.
	// ADR-066 makes workloads the tenant's, and this is the one credential that
	// is irreducibly theirs.
	PrivateRegistry bool

	// BundleRegistry is where published bundles are pulled from, without a
	// scheme: ArgoCD pulls a scheme-less registry host as an OCI artefact and
	// passes anything else to `helm pull --repo`, which does not speak OCI.
	BundleRegistry string
	// Private controls repository visibility at creation.
	Private bool
}

// RepoName is the repository this tenant's box reconciles from.
func (s Spec) RepoName() string { return s.TenantID + "-gitops" }

func (s Spec) repoURL() string {
	return fmt.Sprintf("https://github.com/%s/%s", s.GitOrg, s.RepoName())
}

func (s Spec) validate() error {
	missing := []string{}
	for name, v := range map[string]string{
		"tenant": s.TenantID, "box": s.GitOrg, "domain": s.Domain,
		"provider": s.Provider,
		"cluster": s.ClusterName, "bundle-version": s.BundleVersion,
		"platform-repo": s.PlatformRepoURL, "bundle-registry": s.BundleRegistry,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// The version is the one an unreleased build cannot supply, and the
		// reason is worth stating: a developer running from source has no
		// published version to pin, and the alternatives are naming one or
		// scaffolding a repository that cannot bootstrap.
		for _, m := range missing {
			if m == "bundle-version" {
				return fmt.Errorf("no bundle version.\n\n"+
					"A released CLI pins the version it carries. This build is unreleased, so\n"+
					"it has none to offer and will not guess: a repository pinned to a version\n"+
					"that does not resolve bootstraps into an Application that cannot load its\n"+
					"source.\n\n"+
					"Pass --bundle-version with a published version, or use a released CLI.\n"+
					"Published versions: https://github.com/kamarkat/kamarkat/releases\n\n"+
					"To test a change to the platform without publishing anything, pass\n"+
					"--bundle-version development: the box reads its chart from the branch\n"+
					"you are on rather than from the registry, and is bootstrapped from a\n"+
					"local clone instead of a workflow.%s",
					otherMissing(missing))
			}
		}
		return fmt.Errorf("missing required values: %s", strings.Join(missing, ", "))
	}
	// No topology check here. The supported matrix is keyed by a WORKLOAD
	// environment (ADR-111 Revision 7 §4) and the management cluster has none, so
	// there is nothing to look up: `kakt tenant add-cluster` asks it of each
	// workload cluster's own declaration instead.
	return nil
}

// RequireEscrow refuses a box that would have nowhere to keep its master keys.
//
// Checked after the repository is created rather than before: the repository is
// not the thing at risk, and a tenant who has scaffolded and not yet obtained an
// escrow should keep what they have rather than start again.
//
// Required rather than warned about because the cost of skipping lands entirely in
// the future, on someone who did not make the choice. A box without an escrow
// behaves identically for months; the difference appears on the day the cluster is
// gone, and on that day the escrow can no longer be added (ADR-076).
func (s Secrets) RequireEscrow() error {
	if s.hasEscrow() {
		return nil
	}
	return fmt.Errorf("this box has no escrow, and will not be dispatched without one.\n\n" +
		"hub-operator copies its Infisical master keys -- the root secret without\n" +
		"which its secret store cannot be decrypted -- to an Infisical you control.\n" +
		"The box's own cannot hold them: they are the keys that decrypt it.\n\n" +
		"Infisical Cloud is the usual answer: https://app.infisical.com\n" +
		"Create a project, add a machine identity with write access to it, and pass\n" +
		"--escrow-url, --escrow-project-id, --escrow-client-id and\n" +
		"--escrow-client-secret, or answer the prompts.")
}

// VerifyEscrow proves the escrow works before a box is built on it.
//
// Separate from RequireEscrow because they answer different questions and have
// different costs: presence is a property of the arguments and needs nothing,
// usability is a property of someone else's account and needs a request. Callers
// do both; only this one reaches the network.
//
// Presence was the whole check anywhere: four non-empty values, never exercised. An
// Infisical project has a TYPE, and only a secret-manager project accepts what an
// escrow does -- a kms project authenticates perfectly and refuses every read and
// write. Nothing noticed until the hub-operator tried it, forty minutes into a
// bootstrap, and the box stalled at boundary-04 waiting on database roles that
// could not be created.
//
// The cost of accepting an unusable escrow is the same as accepting none, and
// RequireEscrow already refuses none. Verifying costs one request and moves the
// failure to the moment the value is given, which is the only moment it can be
// corrected cheaply.
func (s Secrets) VerifyEscrow() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The env is what the escrow client reads, and what the box will be given.
	for k, v := range map[string]string{
		"INFISICAL_ESCROW_URL":           s.EscrowURL,
		"INFISICAL_ESCROW_PROJECT_ID":    s.EscrowProjectID,
		"INFISICAL_ESCROW_CLIENT_ID":     s.EscrowClientID,
		"INFISICAL_ESCROW_CLIENT_SECRET": s.EscrowClientSecret,
	} {
		restore, err := setEnvFor(k, v)
		if err != nil {
			return err
		}
		defer restore()
	}

	// "" because there is no cluster yet to compare against; the same-Infisical
	// refusal applies to a running box and cannot be evaluated here.
	if _, err := escrow.NewEscrowClient(ctx, ""); err != nil {
		return fmt.Errorf("the escrow this box would use does not work.\n\n%w\n\n"+
			"Nothing is scaffolded on an escrow that cannot hold the master keys: the\n"+
			"box behaves identically for months and the difference appears on the day\n"+
			"the cluster is gone (ADR-076).", err)
	}
	return nil
}

// setEnvFor sets one variable and returns a function restoring what was there.
func setEnvFor(key, value string) (func(), error) {
	prev, had := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		return nil, fmt.Errorf("set %s: %w", key, err)
	}
	return func() {
		if had {
			_ = os.Setenv(key, prev)
			return
		}
		_ = os.Unsetenv(key)
	}, nil
}

// supportedMatrix is the environment/provider combinations a workload cluster may
// declare. It mirrors `supportedMatrix` in the environment-manager chart's
// values.yaml, which is the authority.
//
// Duplicated deliberately rather than read from the chart: this runs before any
// repository exists, on a CLI that may be released and carrying an embedded chart
// it must not have to render to answer a question about its own flags. The
// duplication is asserted by TestScaffoldMatrixMatchesTheChart.
var supportedMatrix = map[string][]string{
	"dev": {"hetzner", "hybrid"},
	// stg moved from hybrid to hetzner: hybrid is development-only. Staging
	// rehearses production, and production is hetzner -- on hybrid it was
	// rehearsing a tailnet-joined, node-local-storage topology no tenant runs.
	"stg":  {"hetzner"},
	"prod": {"hetzner"},
}

// supportedCombination refuses a combination the platform supports no topology
// for, at the moment the cluster is declared.
//
// It is asked HERE because this is the first and only place the whole combination
// is known before anything is built. The chart's own assertSupported used to ask
// it at render time, inside ArgoCD, against the HUB's environment -- which stopped
// answering the question when the hub stopped having one (ADR-111 Revision 7), and
// surfaced as a boundary whose ApplicationSets never appear, ten minutes into a
// bootstrap and after the servers exist.
func supportedCombination(environment, provider string) error {
	providers, ok := supportedMatrix[environment]
	if !ok {
		return fmt.Errorf("unsupported environment %q. A workload cluster may declare: %s",
			environment, strings.Join(sortedEnvironments(), ", "))
	}
	for _, p := range providers {
		if p == provider {
			return nil
		}
	}
	return fmt.Errorf("%s on %s is not a supported combination.\n\n"+
		"The platform supports no topology for it, so the cluster would be built from\n"+
		"sources nobody maintains -- and ArgoCD reports an Application that manages\n"+
		"nothing as Healthy.\n\n"+
		"Supported: %s\n\n"+
		"Pass --environment or --provider to choose one.",
		environment, provider, strings.Join(supportedCombinations(), ", "))
}

func sortedEnvironments() []string {
	out := make([]string, 0, len(supportedMatrix))
	for env := range supportedMatrix {
		out = append(out, env)
	}
	sort.Strings(out)
	return out
}

func supportedCombinations() []string {
	var out []string
	for _, env := range sortedEnvironments() {
		providers := append([]string(nil), supportedMatrix[env]...)
		sort.Strings(providers)
		for _, p := range providers {
			out = append(out, env+"+"+p)
		}
	}
	return out
}

// tenantTokens are facts about the tenant, true of every cluster in its box.
// They are substituted everywhere, including in the templates the tenant keeps.
func (s Spec) tenantTokens() map[string]string {
	return map[string]string{
		"<TENANT_ID>":         s.TenantID,
		"<GIT_ORG>":           s.GitOrg,
		"<GITOPS_REPO_URL>":   s.repoURL(),
		"<PLATFORM_REPO_URL>": s.PlatformRepoURL,
		// The same repository as an owner/name slug, which is what `gh` takes.
		// Derived rather than carried as a second field: two fields naming one
		// repository is two things to keep in step, and a pre-flight workflow
		// pointed at a different platform than the bundle it judges would fetch
		// a CLI and release notes belonging to something else.
		"<PLATFORM_REPO_SLUG>": platformRepoSlug(s.PlatformRepoURL),
		"<BUNDLE_REGISTRY>":    s.BundleRegistry,
		"<DOMAIN_NAME>":        s.Domain,
	}
}

// RegistryDir is the directory a tenant's clusters and fleets live under.
//
// "registry", as in kubefirst, whose gitops repository holds registry/ beside a
// templates/ directory that renders into it. ADR-071 names kubefirst as the
// reference this platform is measured against, and a directory that means the
// same thing should be called the same thing -- a reader who knows one should
// not have to learn the other's names to find the same file.
//
// Checked against konstructio/gitops-template, 2026-11-05: the template ships
// registry/environments/ and templates/{mgmt,workload-cluster,workload-vcluster,
// workload-host-vcluster}. There is no registry/clusters/ in it -- that
// directory appears only once a workload cluster has been created from a
// template, which is the same arrangement Render uses here. This comment said
// the template held both, which it does not.
const RegistryDir = "registry"

// clusterTokens are facts about ONE cluster, and are substituted only into the
// instance being hydrated.
//
// The templates a tenant keeps must retain them. Substituting them everywhere
// leaves templates/workload-cluster hard-coded to the control plane that happened to
// be rendered first, so a tenant adding its second cluster gets a copy of its
// first -- same name, same environment, same region. The template would still
// look correct, and the collision would appear as two clusters fighting over one
// set of resources.
func (s Spec) clusterTokens() map[string]string {
	return map[string]string{
		"<BUNDLE_VERSION>":    s.BundleVersion,
		"<PUBLIC_TLS_ISSUER>": publicTLSIssuer(),
		"<HUB_DOMAIN>":        hubDomain(s.Subdomain, s.Domain),
		// The parts as well as the join: the chart composes from these, and a
		// person edits them rather than a pre-joined string (kubefirst stores
		// DomainName and SubdomainName the same way).
		"<DOMAIN>":         strings.TrimSpace(strings.TrimSuffix(s.Domain, ".")),
		"<SUBDOMAIN>":      strings.TrimSpace(strings.Trim(s.Subdomain, ".")),
		"<CHART_SOURCE>":   s.chartSource(),
		"<WORKER_COUNT>":   strconv.Itoa(s.workerCount()),
		"<BOX_ID>":         s.BoxID,
		"<CLUSTER_NAME>":   s.ClusterName,
		"<CLOUD_PROVIDER>": s.Provider,
		"<CLOUD_REGION>":   s.Region,
		// The box's escrow, as NAMES. A url and a project id grant nothing, and
		// writing them here is what lets a second workstation reach this box: a
		// developer can authenticate as themselves and still not know which
		// Infisical or which project holds the box's artefacts, and until now the
		// answer existed only in k8-secrets/infisical/ on the machine that ran
		// `kakt escrow init`. The credentials stay where they are.
		"<ESCROW_URL>":        escrowCoordinate("INFISICAL_ESCROW_URL"),
		"<ESCROW_PROJECT_ID>": escrowCoordinate("INFISICAL_ESCROW_PROJECT_ID"),
	}
}

// escrowCoordinate reads one of the box's escrow names from wherever this machine
// keeps them, the way the escrow client itself reads them.
//
// Empty is tolerated here and refused later, by the command that needs it: a box
// scaffolded before its escrow exists is a supported order of operations (the
// escrow is enrolled with `kakt escrow init`, and `kakt escrow enroll` fills the
// rest in), while a scaffold that failed on an absent escrow would make the
// escrow a precondition for having a repository at all.
func escrowCoordinate(name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		b, err := os.ReadFile(filepath.Join(dir, "k8-secrets", "infisical", name))
		if err == nil {
			return strings.TrimSpace(string(b))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// workerCount is how many cloud workers this box starts with.
//
// Two in every environment. Development briefly defaulted to zero, so that its
// capacity would come from the tenant's own hardware (ADR-075) -- the right end
// state, reached the wrong way: it made on-prem hardware a precondition for the
// simplest box the platform can build, and scaffolding with no arguments produced
// a repository whose bootstrap was refused for having nowhere to schedule.
//
// A tenant who wants that arrangement asks for it with --workers 0, which is why
// the pointer above distinguishes an explicit zero from an absent flag.
func (s Spec) workerCount() int {
	if s.Workers != nil {
		return *s.Workers
	}
	return 2
}

// CapacityWarning reports that this box, as scaffolded, has nowhere to run
// anything -- or returns "" when it does.
//
// A box needs worker capacity from somewhere: cloud workers, or nodes on the
// tenant's own premises. A development box defaults to no cloud workers, so one
// scaffolded without on-prem details has neither. Its bootstrap is refused at
// pre-flight, which is the correct place to stop but the wrong place to find out:
// by then a repository exists, secrets are set, and someone is watching a workflow.
//
// Said here instead, in the output of the command that made the choice.
func (s Spec) CapacityWarning() string {
	if s.workerCount() > 0 {
		return ""
	}
	return fmt.Sprintf(
		"This box has no worker capacity: %s provisions no cloud workers, so its\n"+
			"capacity must come from nodes on your own premises.\n\n"+
			"  Bootstrap it with on-prem nodes:  dispatch with on-prem=true and your tailnet\n"+
			"  Or give it cloud workers instead: re-scaffold with --workers 2,\n"+
			"                                    or edit workers in registry/clusters/%s/values.yaml\n\n"+
			"Without one of those the bootstrap is refused before anything is built.",
		s.Provider, s.ClusterName)
}

// chartSource renders where this cluster's platform content comes from.
//
// Two shapes, not two values. A released box names a published chart in the
// registry and tells that chart which version it is. A development box names the
// platform's repository at a branch and a path, because there is no published
// chart to name -- which is the same rule ADR-068 applies to the platform's own
// seed, and the reason the platform can be tested end to end without consuming a
// version (ADR-063: a version is consumed by any release that begins publishing it).
//
// A tenant never runs the development shape. It exists so that changing the
// platform and testing the change do not require a release each time.
func (s Spec) chartSource() string {
	if s.BundleVersion == versions.DevelopmentBundle {
		return `repoURL: ` + s.PlatformRepoURL + `
      targetRevision: ` + s.developmentRevision() + `
      path: manifests/argocd/environment-manager
      helm:
        # The generated values file does not exist until Day-0 writes it, and
        # helm EXITS on a values file it cannot open -- so without this the seed
        # Application cannot render at all and boundary 01 never appears. That
        # is not a race that ordering fixes: the file is written by
        # bootstrap-infisical-api, which runs after the boundaries it would
        # unblock.
        ignoreMissingValueFiles: true
        valueFiles:
          # Generated first, this cluster's own values second: Helm lets the
          # later file win, so a fact Day-0 recorded never silently beats
          # something the tenant wrote here deliberately.
          #
          # The generated file carries this box's Infisical PKI coordinates
          # (ADR-045). Without them the security boundary emits no ClusterIssuer
          # and every certificate naming infisical-fleet-issuer stays pending --
          # the argocd-agent mTLS identities never issue, and
          # the Support Agent can never enrol, because its client certificate IS
          # its enrolment.
          - $values/registry/clusters/` + s.ClusterName + `/generated/values/platform-pki-values.yaml
          # The box's own Infisical organisation and projects. hub-environment
          # renders hub-bootstrap-config from these, and is its only writer.
          - $values/registry/clusters/` + s.ClusterName + `/generated/values/infisical-identity.yaml
          # The address every public hostname on this box resolves to -- the CAPH
          # control-plane load balancer, which exists only once the cluster does.
          # Day-0 records it here; the hub Gateway and its routes take their
          # external-dns target from it. Without it external-dns finds no target,
          # publishes nothing, and every hub hostname is NXDOMAIN.
          - $values/registry/clusters/` + s.ClusterName + `/generated/values/gateway-dns-target.yaml
          - $values/registry/clusters/` + s.ClusterName + `/values.yaml`
	}
	return `repoURL: ` + s.BundleRegistry + `
      chart: environment-manager
      targetRevision: ` + s.BundleVersion + `
      helm:
        parameters:
          # The same version as targetRevision above, and it must stay the same.
          #
          # targetRevision decides which chart is pulled; this tells that chart
          # which version it is. The chart cannot work it out -- its own
          # Chart.version is rewritten at package time and its default is
          # "development" -- so a chart that is never told stays in development
          # mode, sourcing every boundary from the platform's git repository
          # instead of from the distribution it was published in.
          #
          # That is not a subtle degradation: the boundary ApplicationSets render
          # a git generator against a repository the tenant cannot read, one
          # failing generator takes the whole ApplicationSet with it, and the
          # bootstrap stops at "0 of 3 Applications generated" ten minutes after
          # the cluster was otherwise finished.
          #
          # Renovate rewrites both lines in one match (see renovate.json), so a
          # promotion cannot move one and leave the other behind.
          - name: bundleVersion
            value: "` + s.BundleVersion + `"
        # The generated values file does not exist until Day-0 writes it, and
        # helm EXITS on a values file it cannot open -- so without this the seed
        # Application cannot render at all and boundary 01 never appears. That
        # is not a race that ordering fixes: the file is written by
        # bootstrap-infisical-api, which runs after the boundaries it would
        # unblock.
        ignoreMissingValueFiles: true
        valueFiles:
          # Generated first, this cluster's own values second: Helm lets the
          # later file win, so a fact Day-0 recorded never silently beats
          # something the tenant wrote here deliberately.
          #
          # The generated file carries this box's Infisical PKI coordinates
          # (ADR-045). Without them the security boundary emits no ClusterIssuer
          # and every certificate naming infisical-fleet-issuer stays pending --
          # the argocd-agent mTLS identities never issue, and
          # the Support Agent can never enrol, because its client certificate IS
          # its enrolment.
          - $values/registry/clusters/` + s.ClusterName + `/generated/values/platform-pki-values.yaml
          # The box's own Infisical organisation and projects. hub-environment
          # renders hub-bootstrap-config from these, and is its only writer.
          - $values/registry/clusters/` + s.ClusterName + `/generated/values/infisical-identity.yaml
          # The address every public hostname on this box resolves to -- the CAPH
          # control-plane load balancer, which exists only once the cluster does.
          # Day-0 records it here; the hub Gateway and its routes take their
          # external-dns target from it. Without it external-dns finds no target,
          # publishes nothing, and every hub hostname is NXDOMAIN.
          - $values/registry/clusters/` + s.ClusterName + `/generated/values/gateway-dns-target.yaml
          - $values/registry/clusters/` + s.ClusterName + `/values.yaml`
}

// developmentRevision is the platform branch a development box reads.
//
// Whatever branch the operator is on, so a change under test is the change the
// cluster reconciles. Defaults to main when it cannot be determined -- a detached
// checkout, or a copy of the tree with no git.
func (s Spec) developmentRevision() string {
	if s.PlatformRevision != "" {
		return s.PlatformRevision
	}
	return "main"
}

// publicTLSIssuer is the ACME issuer this environment's public certificates come
// from.
//
// Written into the values file at scaffold time because the chart refuses to
// default it, and refuses for a good reason: an empty issuer produces an
// ApplicationSet that applies cleanly and a child Application that cannot render,
// which ArgoCD reports as Healthy while managing nothing -- leaving whatever
// certificate was issued last in place, including a staging one no browser
// trusts. The chart's message says the environment bootstrap owns this policy.
//
// For a tenant box, scaffolding IS that bootstrap: it is where the environment is
// chosen, and the tenant's seed passes a values file and no parameters, so a value
// the chart requires and scaffolding does not write is a value nothing supplies.
// That gap stalled a real bootstrap at boundary-01 for ten minutes with the seed
// rendering no ApplicationSets at all.
// The hub's certificate policy, which is the hub's own and not a workload
// environment's (ADR-111 Revision 7 §3).
//
// It took an environment and returned letsencrypt-staging for "ephemeral",
// trading browser trust for Let's Encrypt's per-registered-domain rate limit on
// a certificate that outlives its cluster by minutes. That branch was already
// unreachable from here -- "ephemeral" is not a key in supportedMatrix, so
// scaffolding refused it before this was called -- and with the hub declaring no
// environment there is no input left to branch on.
func publicTLSIssuer() string {
	return "letsencrypt-prod"
}

// hubDomain is the base domain this box publishes on, and the authority every
// public host derives from (ADR-051): id., api., infisical., argocd.
//
// Environment-prefixed except in production, which uses the apex -- the same
// convention ReadHubZone documents for the platform's own overlays, where "a
// production overlay deliberately does not patch spec.domain".
//
// Written by scaffolding because the chart cannot default it. It travelled as the
// platform's own literal inside agentgateway's config until this existed, so a
// tenant's gateway trusted the PLATFORM's Zitadel as its OIDC issuer and fetched
// signing keys from it -- broken, and the runtime dependency on the vendor that
// ADR-065 and ADR-066 exist to prevent.
// hubDomain composes the domain every public hostname on this box derives from.
//
// SUBDOMAIN, not environment. It read the environment and returned
// "<env>.<domain>" unless the environment was prod -- which made the box's DNS a
// function of its environment, and therefore made the MANAGEMENT cluster have an
// environment, because it has hostnames. Everything environment-shaped followed
// from that: the box's Infisical slug, nine packaged chart variants, and a spoke
// inheriting its hub's environment rather than declaring its own.
//
// kubefirst reaches the same "dev.example.com" shape without any of it. Its
// management cluster has no environment and its platform hostnames carry none
// (argocd.<domain>, vault.<domain>); an install-wide SubdomainName is applied as
// fmt.Sprintf("%s.%s", SubdomainName, DomainName), and workloads carry the
// environment as a name suffix -- metaphor-development.<domain> -- never as a DNS
// level. ADR-051's amendment of 2026-09-18 adopts that separation.
//
// No hostname changes: a box that declared environment dev and now declares
// subdomain dev publishes exactly what it published before.
func hubDomain(subdomain, domain string) string {
	domain = strings.TrimSpace(strings.TrimSuffix(domain, "."))
	subdomain = strings.TrimSpace(strings.Trim(subdomain, "."))
	if subdomain == "" {
		return domain
	}
	return subdomain + "." + domain
}

func (s Spec) allTokens() map[string]string {
	all := s.tenantTokens()
	for k, v := range s.clusterTokens() {
		all[k] = v
	}
	return all
}

// secretish rejects a value that looks like credential material. A token is an
// identifier or a location; anything long and high-entropy, or spelled like a
// key, is a value someone has passed to the wrong place.
func secretish(v string) bool {
	l := strings.ToLower(v)
	for _, marker := range []string{"-----begin", "secret", "password", "token", "private_key", "privatekey"} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}

// Render writes the tenant's repository content into dst.
//
// The shape it produces is the one this platform's reference implementation
// arrived at: a template directory consumed once and removed, and one retained
// for the clusters the tenant adds later. What is deliberately NOT copied is the
// platform's own content. kubefirst copies it, which is why nothing propagates
// to a rendered repository afterwards; here the bundle is sourced, so the
// platform can keep maintaining it (ADR-063).
func Render(templateDir, dst string, s Spec) error {
	if err := s.validate(); err != nil {
		return err
	}
	for token, value := range s.allTokens() {
		if secretish(value) {
			return fmt.Errorf("refusing to render %s: value looks like secret material, and every token is an identifier or a location", token)
		}
	}

	if err := copyPlatformTree(templateDir, dst); err != nil {
		return fmt.Errorf("copy template: %w", err)
	}

	// Hydrate the control plane from its template, then remove the template that
	// produced it. It is consumed once, at onboarding; a tenant adding a cluster
	// later renders templates/workload-cluster, which is why that one is kept.
	src := filepath.Join(dst, "templates", "mgmt")
	if err := copyDir(src, filepath.Join(dst, RegistryDir, "clusters", s.ClusterName)); err != nil {
		return fmt.Errorf("hydrate control plane: %w", err)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("remove consumed template: %w", err)
	}

	// Tenant facts everywhere; cluster facts only where a cluster is named. Only
	// those places are checked for leftovers, because a retained template is
	// supposed to still contain them.
	if err := substitute(dst, s.tenantTokens(), false); err != nil {
		return err
	}
	if err := substitute(filepath.Join(dst, RegistryDir, "clusters", s.ClusterName), s.clusterTokens(), true); err != nil {
		return err
	}

	// The workflows too. They carry the first cluster's facts as dispatch
	// defaults, and the bundle version as the pin on the platform workflow they
	// call -- an unsubstituted pin is a workflow reference to a tag named
	// "v<BUNDLE_VERSION>", which GitHub reports as a missing workflow rather than
	// as a scaffolding fault.
	workflows := filepath.Join(dst, ".github", "workflows")
	if _, err := os.Stat(workflows); err == nil {
		return substitute(workflows, s.clusterTokens(), true)
	}
	return nil
}

// copyTree copies the template, resolved through the platform package so a
// released binary reads the template it carries and an unreleased one the
// working tree (ADR-063, ADR-068).
//
// Walking through fs.WalkDir rather than filepath.Walk is what makes both cases
// one code path: an embedded FS and a directory present the same interface, so
// the scaffolder cannot acquire a dependency on being run inside a checkout --
// which is what it had, and what made the published binary unusable.
func copyPlatformTree(src, dst string) error {
	return platform.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := platform.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// 0o644 rather than the source mode: an embedded FS reports its own
		// permissions, not the repository's, so preserving them would give a
		// released run different modes from a development one for the same file.
		//
		// Shell scripts are the exception, and are derived from the NAME rather
		// than from the source mode for the same reason. A scaffolded script
		// that arrives non-executable reads as broken -- the tenant runs
		// ./scripts/<x>.sh, gets "permission denied", and has no reason to
		// think that is their own filesystem rather than a defect in what they
		// were given.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(target, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(target, b, mode)
	})
}

// substitute replaces tokens across the rendered tree and refuses to leave one
// behind. An unsubstituted token is not cosmetic: it reaches the tenant's
// repository as a literal, and the Application that carries it fails to resolve
// a repository or a version with an error naming neither.
func substitute(root string, tokens map[string]string, strict bool) error {
	var leftover []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := string(b)
		for token, value := range tokens {
			out = strings.ReplaceAll(out, token, value)
		}
		if strict {
			for _, line := range strings.Split(out, "\n") {
				if t := unresolvedToken(line); t != "" {
					leftover = append(leftover, fmt.Sprintf("%s: %s", p, t))
				}
			}
		}
		return os.WriteFile(p, []byte(out), info.Mode())
	})
	if err != nil {
		return err
	}
	if len(leftover) > 0 {
		return fmt.Errorf("unsubstituted tokens remain:\n  %s", strings.Join(leftover, "\n  "))
	}
	return nil
}

// unresolvedToken finds a <UPPER_CASE> token, which is the template's spelling
// and cannot occur in rendered YAML for any other reason.
func unresolvedToken(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] != '<' {
			continue
		}
		j := strings.IndexByte(line[i:], '>')
		if j < 0 {
			return ""
		}
		inner := line[i+1 : i+j]
		if inner != "" && inner == strings.ToUpper(inner) && !strings.ContainsAny(inner, " /\"'") {
			return "<" + inner + ">"
		}
		i += j
	}
	return ""
}

// Credential is how the platform authenticates to the tenant's git host.
//
// ADR-062 makes the App installation the platform's only credential, and it is
// scoped to repositories: it can create one and open pull requests against it,
// and reaches no cloud account, cluster or secret store. A personal token is
// accepted because that is what exists before an App is registered, and it is
// the same authority by a weaker route -- it should not outlive onboarding of
// the first real tenant.
type Credential struct {
	Token  string
	IsApp  bool
	Source string
}

// TokenFromEnv resolves the credential the way the rest of the CLI does, so
// onboarding needs no separate setup on a machine that can already bootstrap.
func TokenFromEnv(repoRoot string) (Credential, error) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_APP_TOKEN")); t != "" {
		return Credential{Token: t, IsApp: true, Source: "GITHUB_APP_TOKEN"}, nil
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return Credential{Token: t, Source: "GITHUB_TOKEN"}, nil
	}
	p := filepath.Join(repoRoot, "k8-secrets", "github", "github-pat-token")
	b, err := os.ReadFile(p)
	if err != nil {
		// Says what to do, not only what is missing. Scaffolding is often the
		// first command anyone runs against this platform, and an error naming a
		// variable without saying where its value comes from is a support round
		// trip.
		return Credential{}, fmt.Errorf(
			"no GitHub credential.\n\n"+
				"Scaffolding creates a repository in the tenant's organisation, so it needs\n"+
				"one of these, in order of preference:\n\n"+
				"  GITHUB_APP_TOKEN  an installation token for the App the tenant granted.\n"+
				"                    Preferred: the access is the tenant's to revoke (ADR-062).\n"+
				"  GITHUB_TOKEN      a personal access token with repo scope.\n"+
				"                    Create one at https://github.com/settings/tokens\n"+
				"  %s\n"+
				"                    the same token, read from disk.", p)
	}
	return Credential{Token: strings.TrimSpace(string(b)), Source: p}, nil
}

// CreateRepo creates <org>/<tenant>-gitops. It reports whether the repository
// already existed, because onboarding must be resumable: a run that failed after
// creating the repository has to be able to continue rather than requiring the
// operator to delete it first.
func CreateRepo(ctx context.Context, s Spec, cred Credential) (existed bool, err error) {
	api := "https://api.github.com"
	if v := strings.TrimSpace(os.Getenv("GITHUB_API_URL")); v != "" {
		api = strings.TrimSuffix(v, "/")
	}
	client := &http.Client{Timeout: 30 * time.Second}

	get, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/%s", api, s.GitOrg, s.RepoName()), nil)
	authorize(get, cred)
	if resp, err := client.Do(get); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true, nil
		}
	}

	body, _ := json.Marshal(map[string]any{
		"name":        s.RepoName(),
		"private":     s.Private,
		"description": fmt.Sprintf("Infrastructure for the %s box, maintained by proposal", s.TenantID),
		"auto_init":   false,
	})
	post, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/orgs/%s/repos", api, s.GitOrg), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	authorize(post, cred)
	post.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(post)
	if err != nil {
		return false, fmt.Errorf("create repository: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		return false, fmt.Errorf("create %s/%s: %s (%s)", s.GitOrg, s.RepoName(), resp.Status, msg.Message)
	}
	return false, nil
}

func authorize(r *http.Request, c Credential) {
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// Publish initialises the rendered directory as a repository and pushes it.
//
// The history starts here. The template's history is not carried across: it is
// the platform's, it describes files this repository does not contain, and a
// tenant reading `git log` should see its own box being created rather than the
// platform's development.
func Publish(ctx context.Context, dir string, s Spec, cred Credential) error {
	remote := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		cred.Token, s.GitOrg, s.RepoName())

	// A repository that already has history is not scaffolded over.
	//
	// This check used to be a claim: the caller said pushing was "refused below
	// if it already has history", and nothing below refused it. What actually
	// stopped it was GitHub rejecting a non-fast-forward push, which is a
	// different thing -- it produced a raw git error advising `git pull`, which
	// would merge a tenant's box into a freshly rendered template. Safe by
	// accident, and unreadable.
	//
	// ADR-062: scaffolding happens once and the repository is a starting state
	// rather than a fork. Re-rendering one that exists is not a resume, it is a
	// second starting state for a box that already has one.
	ls := exec.CommandContext(ctx, "git", "ls-remote", "--heads", remote)
	out, err := ls.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote: %w: %s", err,
			strings.ReplaceAll(string(out), cred.Token, "***"))
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("%s/%s already has commits, so it is not scaffolded again.\n"+
			"Scaffolding produces a starting state, not a fork (ADR-062), and pushing\n"+
			"this render over an existing box would replace declarations its clusters\n"+
			"are reconciling.\n\n"+
			"  To re-scaffold from scratch: delete the repository, then run this again.\n"+
			"  To inspect what would be written: re-run with --dry-run --out <dir>.\n"+
			"  To bootstrap the box that is already there: dispatch its own\n"+
			"  bootstrap-cluster workflow rather than scaffolding.",
			s.GitOrg, s.RepoName())
	}

	steps := [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"-c", "user.name=zero-ops", "-c", "user.email=platform\nzero-ops.local",
			"commit", "-q", "-m", "Scaffold " + s.RepoName()},
		{"remote", "add", "origin", remote},
		{"push", "-q", "-u", "origin", "main"},
	}
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			// The remote carries the credential, so it is replaced rather than
			// printed: a failing push would otherwise write the token into logs
			// and into whatever collects them.
			return fmt.Errorf("git %s: %w: %s", args[0], err,
				strings.ReplaceAll(string(out), cred.Token, "***"))
		}
	}
	return nil
}

// copyDir copies a directory already on disk.
//
// Kept separate from copyPlatformTree because the two look alike and are not:
// this one moves content the render has already produced -- hydrating a cluster
// from the template the tenant keeps -- and its source is an absolute path in
// the destination, not a repository-relative platform path. Routing it through
// the platform resolver made a released build look for a temporary directory
// inside its embedded tree.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode())
	})
}

// otherMissing names what else is absent, so a developer fixing the version does
// not then discover the next omission one run later.
func otherMissing(missing []string) string {
	var rest []string
	for _, m := range missing {
		if m != "bundle-version" {
			rest = append(rest, m)
		}
	}
	if len(rest) == 0 {
		return ""
	}
	return "\n\nAlso missing: " + strings.Join(rest, ", ")
}

// platformRepoSlug reduces a platform repository URL to the owner/name form the
// GitHub CLI takes. An unrecognisable URL yields the empty string, so the
// workflow step that uses it fails loudly rather than querying a default
// repository that has nothing to do with this platform.
func platformRepoSlug(repoURL string) string {
	u := strings.TrimSuffix(strings.TrimSpace(repoURL), ".git")
	u = strings.TrimSuffix(u, "/")
	if i := strings.Index(u, "github.com"); i >= 0 {
		u = strings.TrimLeft(u[i+len("github.com"):], ":/")
	} else {
		return ""
	}
	parts := strings.Split(u, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
