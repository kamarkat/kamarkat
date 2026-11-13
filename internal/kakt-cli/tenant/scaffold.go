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
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"io"
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
	TenantID string
	GitOrg   string
	// PlatformAdmin is Infisical's id for the one human this box accepts.
	PlatformAdmin string
	Domain        string
	Provider      string
	Region        string
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
	// GitopsRepo is this box's repository name, inside GitOrg, and
	// ManagementCluster is the name its first management cluster takes. Both are
	// minted with BoxID by DeriveBoxIdentity for a new box, and read from the
	// box's declaration for one that exists.
	GitopsRepo        string
	ManagementCluster string
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

	// The box's escrow, as names: a url, a project and the organisation that
	// issued it. Carried on the Spec rather than read from the operator's store
	// inside the renderer, because a cluster declared later is rendered from
	// what the BOX recorded (registry/box.yaml) and not from whatever the
	// machine doing the declaring has in its own store. Creating a box fills
	// them with WithEscrowCoordinates; declaring a cluster fills them from the
	// box's declaration.
	EscrowURL       string
	EscrowProjectID string
	EscrowOrgID     string
	// Private controls repository visibility at creation.
	Private bool
}

// RepoName is the repository this tenant's box reconciles from.
//
// Read, never computed. It used to be `<tenant>-gitops`, which made two boxes of
// one tenant impossible to hold in one organisation -- and a replacement box
// needs its own repository (ADR-111 Revision 9 §4). The name is minted once with
// the rest of the box's identity and recorded in the box's own declaration, so
// every later command reads the same answer.
func (s Spec) RepoName() string { return s.GitopsRepo }

func (s Spec) repoURL() string {
	return fmt.Sprintf("https://github.com/%s/%s", s.GitOrg, s.RepoName())
}

// Validate reports what this box cannot be created without.
//
// Exported so the command can ask BEFORE it collects credentials or reaches the
// escrow: a run with no --tenant should say so at once, not after a network
// round trip and a page of inventory.
func (s Spec) Validate() error {
	missing := []string{}
	for name, v := range map[string]string{
		"tenant": s.TenantID, "box": s.GitOrg, "domain": s.Domain,
		"provider": s.Provider, "region": s.Region,
		// The minted names are NOT asked about here. They are not operator
		// inputs: a new box derives them from one timestamp and a continuation
		// reads them from the box's own declaration, so an empty one is a caller
		// that skipped that step rather than a flag somebody forgot -- which
		// RenderBox reports as what it is.
		"bundle-version": s.BundleVersion,
		"platform-repo":  s.PlatformRepoURL, "bundle-registry": s.BundleRegistry,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	// A DEVELOPMENT BOX NEEDS A BRANCH, and there is no sensible default: it
	// reads its charts from the platform's repository at a revision, and the
	// one this used to assume ("main") is a real branch, so the box resolved
	// and reconciled the wrong content.
	if s.BundleVersion == versions.DevelopmentBundle && strings.TrimSpace(s.PlatformRevision) == "" {
		return fmt.Errorf("a development box reads its charts from a branch, and this run could not\n" +
			"determine which one.\n\n" +
			"The branch is taken from the platform checkout that supplied the template.\n" +
			"Run from a checkout, or name it:\n\n" +
			"  --platform-revision <branch>\n\n" +
			"It is not defaulted. `main` is a branch that resolves, so a box pinned to it\n" +
			"bootstraps and reconciles something other than the change you are testing.")
	}

	// Sorted, not map order. Go randomises map iteration, so two runs of the
	// same mistake printed the same values in different orders -- which reads
	// as the answer having changed between them.
	sort.Strings(missing)
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
	return fmt.Errorf("this box has no escrow, and will not be dispatched without one.\n\n"+
		"hub-operator copies its Infisical master keys -- the root secret without\n"+
		"which its secret store cannot be decrypted -- to an Infisical you control.\n"+
		"The box's own cannot hold them: they are the keys that decrypt it.\n\n"+
		"Infisical Cloud is the usual answer: https://app.infisical.com\n\n"+
		"Create one and these settings are recorded for you, so scaffolding finds\n"+
		"them without any being passed:\n\n"+
		"  kakt login\n"+
		"  kakt escrow init --name <project-name>\n\n"+
		"To use an escrow you already have, pass --escrow-url, --escrow-project-id,\n"+
		"--escrow-client-id and --escrow-client-secret, or answer the prompts.%s",
		escrow.MoveHint("INFISICAL_ESCROW_PROJECT_ID", "INFISICAL_ESCROW_CLIENT_ID"))
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

// WithAdminCredential fills the administrator of this box's OWN Infisical from
// the box's escrow, generating it the first time. It reports whether this call
// generated it.
//
// Not prompted, and the reasoning is in ADR-065 rather than in convenience. The
// value has to exist in three places -- the escrow, the tenant's repository, and
// eventually the cluster -- and in none of them is a human the source: Day-0
// creates the account with it, re-enters Infisical with it on a re-run, and a
// successor box restores a database whose user row already holds its hash. What a
// prompt added was a password chosen under pressure and remembered nowhere.
//
// The escrow is read BEFORE anything is generated. A repository secret cannot be
// read back, so a second scaffold run has no way to discover what the first one
// chose; without this, re-running would mint a new password and push it over the
// one the live Infisical was bootstrapped with, locking Day-0 out of the secret
// store it is meant to administer.
func (s Secrets) WithAdminCredential(ctx context.Context, boxID, clusterName, sessionEmail string) (Secrets, bool, error) {
	for k, v := range map[string]string{
		"INFISICAL_ESCROW_URL":           s.EscrowURL,
		"INFISICAL_ESCROW_PROJECT_ID":    s.EscrowProjectID,
		"INFISICAL_ESCROW_CLIENT_ID":     s.EscrowClientID,
		"INFISICAL_ESCROW_CLIENT_SECRET": s.EscrowClientSecret,
	} {
		restore, err := setEnvFor(k, v)
		if err != nil {
			return s, false, err
		}
		defer restore()
	}
	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return s, false, err
	}

	// The environment's address wins over the session's, so an operator
	// scaffolding a box somebody else administers can say so.
	email := strings.TrimSpace(s.InfisicalAdminEmail)
	if email == "" {
		email = strings.TrimSpace(sessionEmail)
	}

	cred, generated, err := escrow.ResolveAdminCredential(ctx,
		store, escrow.ForCluster(boxID, clusterName), email)
	if err != nil {
		return s, false, err
	}
	s.InfisicalAdminEmail = cred.Email
	s.InfisicalAdminPassword = cred.Password
	return s, generated, nil
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

// boxTokens are facts about the BOX, substituted into its own declaration and
// into the workflows that act on it.
//
// They are what a cluster declared later is rendered from, which is why they are
// written down rather than recomputed: `kakt tenant add-management` reads
// registry/box.yaml and composes a cluster's values from it, so a second cluster
// declared on another machine, months later, cannot pick up a different escrow,
// a different platform branch or a different provider than the box it joins.
func (s Spec) boxTokens() map[string]string {
	return map[string]string{
		"<BOX_ID>":             s.BoxID,
		"<GITOPS_REPO>":        s.GitopsRepo,
		"<MANAGEMENT_CLUSTER>": s.ManagementCluster,
		"<DOMAIN>":             strings.TrimSpace(strings.TrimSuffix(s.Domain, ".")),
		"<SUBDOMAIN>":          strings.TrimSpace(strings.Trim(s.Subdomain, ".")),
		// Defaults a cluster declaration starts from, and the dispatch defaults
		// of the workflow that bootstraps one.
		"<CLOUD_PROVIDER>": s.Provider,
		"<CLOUD_REGION>":   s.Region,
		"<WORKER_COUNT>":   strconv.Itoa(s.workerCount()),
		// The version the workflows pin the platform's reusable workflow to
		// (ADR-072), and what a cluster's chart source is composed from.
		"<BUNDLE_VERSION>":    s.BundleVersion,
		"<PLATFORM_REVISION>": s.PlatformRevision,
		// The box's escrow and its one human administrator, as NAMES. See
		// clusterTokens for why committing them is what makes a box operable
		// from more than the machine that created it.
		"<ESCROW_URL>":              s.EscrowURL,
		"<ESCROW_PROJECT_ID>":       s.EscrowProjectID,
		"<ESCROW_ORG_ID>":           s.EscrowOrgID,
		"<PLATFORM_ADMIN_IDENTITY>": strings.TrimSpace(s.PlatformAdmin),
	}
}

// WithEscrowCoordinates fills in where this box's master keys are kept, from
// this operator's own store (or the environment, which is how CI supplies it).
//
// One place reads the store: creating the box. Everything a box does afterwards
// reads the coordinates the box itself recorded, which is what lets a second
// workstation operate it -- see clusterTokens.
func (s Spec) WithEscrowCoordinates() Spec {
	s.EscrowURL = escrowCoordinate("INFISICAL_ESCROW_URL")
	s.EscrowProjectID = escrowCoordinate("INFISICAL_ESCROW_PROJECT_ID")
	s.EscrowOrgID = escrowCoordinate("INFISICAL_ESCROW_ORG_ID")
	return s
}

// RequireEscrowOrganization refuses a box that would declare no Infisical
// organization.
//
// The organization is part of the escrow's identity, not decoration. A box
// names one human by Infisical's own id, and that id is issued by one
// organization and means nothing in another -- so a declaration that records
// the administrator without the organization that issued them cannot be
// checked, and hubaccess refuses rather than skipping the comparison.
//
// Every box rendered before this carried an empty organizationId, because the
// value had one reader and no writer. `kakt escrow init` records it now, which
// is why this refusal names that command: the fix is to run it, not to type an
// id in.
func (s Spec) RequireEscrowOrganization() error {
	if strings.TrimSpace(s.EscrowOrgID) != "" {
		return nil
	}
	return fmt.Errorf(`this box has no Infisical organization to declare, and will not be created without one.

A box names one human administrator by Infisical's own id for them. That id is
issued by an organization and is meaningless in any other, so the organization
is recorded with it -- otherwise the box cannot check the session that presents
it, and escrow-backed access would be granted on an unverified match.

It is read from the escrow you already have, never typed:

  kakt login
  kakt escrow init --name <project name>

records INFISICAL_ESCROW_ORG_ID alongside the escrow's other settings, from the
organization your session belongs to.%s`,
		escrow.MoveHint("INFISICAL_ESCROW_ORG_ID", "INFISICAL_ESCROW_PROJECT_ID"))
}

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
		"<ESCROW_URL>":        s.EscrowURL,
		"<ESCROW_PROJECT_ID>": s.EscrowProjectID,
		// The organization the escrow project belongs to, and the one human the
		// box accepts. Names, like the coordinates above: an id grants nothing,
		// and Infisical decides what that identity may read.
		"<ESCROW_ORG_ID>":           s.EscrowOrgID,
		"<PLATFORM_ADMIN_IDENTITY>": strings.TrimSpace(s.PlatformAdmin),
	}
}

// escrowCoordinate reads one escrow setting for the render.
//
// From the operator's store under the home directory, or the environment --
// never from the checkout this command happens to be run inside. See
// internal/platform/escrow/store.go for why that changed.
func escrowCoordinate(name string) string { return escrow.StoredValue(name) }

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
	// No default. Validate refuses a development box without one, because the
	// value this used to fall back to -- "main" -- is a branch that exists and
	// resolves, so a box pinned to it looked correct and reconciled something
	// other than the change being tested.
	return s.PlatformRevision
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

// RenderBox writes the tenant's repository content into dst: a box, with no
// clusters.
//
// WHY NO CLUSTERS. A box outlives every cluster in it (ADR-111 §1), so the box
// is the thing that is created and a cluster is a declaration added to it --
// `kakt tenant add-management` for the management cluster, `kakt tenant
// add-cluster` for a workload one. This used to render the first management
// cluster in the same pass, which made the box's own identity a field inside
// that cluster's values file: a repository therefore could not exist without a
// cluster, and the one fact that must outlive every cluster was stored in the
// first one. registry/box.yaml holds it now.
//
// Three substitutions, by lifetime rather than by file:
//
//   - tenant facts everywhere, non-strict: the templates the tenant keeps are
//     supposed to still contain the tokens they have not been asked about yet.
//   - the box's own declaration, strict: it is what every later command reads,
//     and an unresolved token there is a box that cannot be read at all.
//   - the workflows, strict: they pin the platform's reusable workflow to this
//     box's version, and an unsubstituted pin is a reference to a tag named
//     "v<BUNDLE_VERSION>", which GitHub reports as a missing workflow rather
//     than as a rendering fault.
//
// templates/mgmt is KEPT. It used to be consumed here and deleted, which left a
// successor incarnation to be rendered by copying the predecessor's directory;
// now it is the template the first management cluster is rendered from, and it
// stays for the same reason templates/workload-cluster does -- a box whose
// clusters have all been removed can declare another.
//
// What is deliberately NOT copied is the platform's own content. kubefirst
// copies it, which is why nothing propagates to a rendered repository
// afterwards; here the bundle is sourced, so the platform can keep maintaining
// it (ADR-063).
func RenderBox(templateDir, dst string, s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	// The identity must already exist. A new box mints it with
	// DeriveBoxIdentity, a continuation reads it with SpecFromDecl, and there is
	// no third way to obtain one -- so an empty name here is a caller that
	// skipped both, not an operator mistake.
	for name, v := range map[string]string{
		"boxId": s.BoxID, "gitopsRepo": s.GitopsRepo,
		"managementClusterName": s.ManagementCluster,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("this box has no identity: %s is empty, so nothing minted or read one", name)
		}
	}
	tenantTokens, boxTokens := s.tenantTokens(), s.boxTokens()
	for _, set := range []map[string]string{tenantTokens, boxTokens} {
		for token, value := range set {
			if secretish(value) {
				return fmt.Errorf("refusing to render %s: value looks like secret material, and every token is an identifier or a location", token)
			}
		}
	}

	if err := copyPlatformTree(templateDir, dst); err != nil {
		return fmt.Errorf("copy template: %w", err)
	}

	if err := substitute(dst, tenantTokens, false); err != nil {
		return err
	}
	decl := filepath.Join(dst, RegistryDir, BoxDeclFile)
	if _, err := os.Stat(decl); err != nil {
		return fmt.Errorf("the template declares no box: %s is missing", filepath.Join(RegistryDir, BoxDeclFile))
	}
	if err := substitute(decl, boxTokens, true); err != nil {
		return err
	}
	// Rewritten atomically, because this file is the box's identity and it is
	// written before the first remote call. A declaration half-written by an
	// interrupted run is a box that cannot be continued: the repository may
	// already exist under a name nothing can now read back.
	if err := rewriteAtomically(decl); err != nil {
		return err
	}

	// A DEVELOPMENT BOX CARRIES NO BOOTSTRAP WORKFLOW.
	//
	// The workflow calls the platform's reusable workflow by tag and downloads
	// the released CLI for the version this box pins -- and `development` is not
	// a version: there is no tag, no release and no published binary, so GitHub
	// refuses the `uses:` line before a step runs and reports a missing
	// workflow. Rendered anyway, the repository offers a dispatch that cannot
	// work and says nothing about why.
	//
	// Wiring one would be worse than leaving it out. CI would have to read the
	// platform's SOURCE at a branch and build it, which is access no tenant has
	// (ADR-065) -- a path that only ever works for the platform's own team,
	// inside the tenant's repository.
	//
	// So a development box is bootstrapped locally, and the same CLI enforces
	// the same contracts either way: `bootstrap-mgmt` runs the identical
	// orchestrator, admission check and escrow requirement that the workflow
	// runs through the released binary. What differs is only where it executes
	// (ADR-072), which is the property the local path exists to preserve.
	if s.BundleVersion == versions.DevelopmentBundle {
		if err := os.Remove(filepath.Join(dst, ".github", "workflows", "bootstrap-cluster.yml")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	workflows := filepath.Join(dst, ".github", "workflows")
	if _, err := os.Stat(workflows); err == nil {
		return substitute(workflows, boxTokens, true)
	}
	return nil
}

// DevelopmentBox reports whether this box reads its charts from a branch rather
// than from a published version, and is therefore bootstrapped locally.
func (s Spec) DevelopmentBox() bool { return s.BundleVersion == versions.DevelopmentBundle }

// IsDevelopmentBox reports the same for a box read back from its declaration.
func (b BoxDecl) IsDevelopmentBox() bool { return b.BundleVersion == versions.DevelopmentBundle }

// templateWalk chooses which tree the template is read from.
//
// ADR-068's rule stands and is the default: a development build reads the
// WORKING TREE, so an edit to the template takes effect without publishing.
// THE EXCEPTION IS THIS ONE CALLER, and the reason is a conflict between two
// rules that are both right.
//
// A box's checkout is created in the working directory, and a box is only
// created in an EMPTY directory -- because anything else there ends up beside
// the box, and inside a repository's checkout it becomes untracked content in
// that repository. An empty directory cannot also contain the platform's
// template. So a development build run the way it is meant to be run can never
// resolve the template from the working tree, and the failure reads as a
// missing file rather than as the two rules meeting.
//
// Every build carries the template, so the binary's own copy is the answer: for
// a locally built CLI it is the template of the checkout it was built from, kept
// current by the hook that verifies the embedded tree. The working tree still
// wins when it is there, which keeps the development loop that ADR-068 protects:
// edit the template, run from the checkout, see the change.
//
// Reported rather than silent -- TemplateOrigin is what the command prints.
func templateSource(src string) (func(string, fs.WalkDirFunc) error, func(string) ([]byte, error)) {
	if !filepath.IsAbs(src) && !platform.Exists(src) && platform.Carries(src) {
		return platform.WalkCarried, platform.ReadCarried
	}
	return platform.WalkDir, platform.ReadFile
}

// TemplateRevision is the branch of the platform checkout that supplied the
// template, and whether there is one to ask.
//
// A development box reads its charts from a branch, so that branch has to be
// known. It used to be read from the WORKING DIRECTORY -- which worked while a
// box was rendered from inside the platform's own checkout, and stopped the day
// a box had to be initialized in an empty directory. The detection failed
// silently and the box pinned "main", so a platform change under test was not
// what the cluster reconciled.
//
// Asked of the template's own tree instead: that is the checkout whose content
// is being rendered, so its branch is the one the box should follow. The tree
// the binary CARRIES has no branch -- it is a copy, not a checkout -- and that
// is reported as unknown rather than guessed.
func TemplateRevision(src string) (string, bool) {
	dir := src
	if !filepath.IsAbs(dir) {
		if !platform.Exists(src) {
			return "", false
		}
		wd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		dir = filepath.Join(wd, src)
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", false
	}
	rev := strings.TrimSpace(string(out))
	if rev == "" || rev == "HEAD" {
		return "", false
	}
	return rev, true
}

// TemplateOrigin says which tree a render will read the template from, for the
// one line a command prints before rendering.
func TemplateOrigin(src string) string {
	switch {
	case filepath.IsAbs(src):
		return src
	case !platform.Exists(src) && platform.Carries(src):
		return src + " (the copy this binary carries; the working directory has none)"
	default:
		return src + " (the working directory)"
	}
}

// rewriteAtomically replaces a file with its own contents through a temporary
// file and a rename, so a reader sees either the whole file or the old one.
func rewriteAtomically(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, info.Mode()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// githubAPI is where repository calls go. GITHUB_API_URL is what a GitHub
// Enterprise host sets, and what the Actions runner exports.
func githubAPI() string {
	if v := strings.TrimSpace(os.Getenv("GITHUB_API_URL")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://api.github.com"
}

// RemoteHasCommits reports whether the box's repository already has history.
//
// Asked before a push and before a resume: an empty repository is one a run
// created and did not finish, and a repository with history is either this box's
// or somebody else's -- which RemoteBoxID is what distinguishes.
func RemoteHasCommits(ctx context.Context, s Spec, cred Credential) (bool, error) {
	remote := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		cred.Token, s.GitOrg, s.RepoName())
	out, err := exec.CommandContext(ctx, "git", "ls-remote", "--heads", remote).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git ls-remote: %w: %s", err,
			strings.ReplaceAll(string(out), cred.Token, "***"))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// RemoteBoxID reads the boxId the repository itself declares.
//
// The question a continuation has to answer before it writes anything: is the
// repository under this name the box I am continuing? Read from the repository
// rather than inferred from its name, because a name is not an identity -- two
// boxes could be given one by accident, and the declaration is what says which
// box a repository is.
//
// An empty answer is not a match. A repository that declares no box, or whose
// declaration cannot be read, is one this command must not write to.
func RemoteBoxID(ctx context.Context, s Spec, cred Credential) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s/%s",
		githubAPI(), s.GitOrg, s.RepoName(), RegistryDir, BoxDeclFile)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	authorize(req, cred)
	req.Header.Set("Accept", "application/vnd.github.raw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read %s/%s from %s/%s: %s",
			RegistryDir, BoxDeclFile, s.GitOrg, s.RepoName(), resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	m := boxIDLine.FindSubmatch(body)
	if m == nil {
		return "", nil
	}
	return string(m[1]), nil
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
	// ONE SOURCE for the whole copy: the walk and the reads must agree, or the
	// walk lists the carried tree's files and each read looks for them in a
	// working directory that does not have them.
	walk, read := templateSource(src)
	return walk(src, func(p string, d fs.DirEntry, err error) error {
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
		b, err := read(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// 0o644 for everything, and no exception for shell scripts.
		//
		// An embedded FS reports its own permissions rather than the
		// repository's, so preserving the source mode would give a released run
		// different modes from a development one for the same file. The .sh
		// exception that used to be here existed for one script the template
		// shipped; a box's repository ships none, and nothing it carries is
		// meant to be executed from it (TestScaffoldedRepoShipsNoScripts).
		return os.WriteFile(target, b, 0o644)
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
//
// BOTH ARRIVE IN ONE VARIABLE. GITHUB_ORG_SETUP_TOKEN names the access -- create
// this tenant's repository in their organisation -- and the two implementations
// of it are told apart from the token itself, below. Two variables asked an
// operator to classify their own credential before the CLI would accept it, and
// the classification changed nothing but a line of output.
// There is no "is it an App token" field. It was set from which variable the
// value arrived in, read in one place, and printed; Kind reads the token itself,
// so nothing has to be told.
type Credential struct {
	Token  string
	Source string
}

// Kind is what to call this credential in output, read from the token itself.
//
// GitHub prefixes an App installation token `ghs_`, and both personal access
// token formats `ghp_` (classic) and `github_pat_` (fine-grained). An unknown
// prefix is reported as neither rather than guessed: enterprise and proxy hosts
// issue their own, and a confident wrong label is worse than a plain one.
func (c Credential) Kind() string {
	switch {
	case strings.HasPrefix(c.Token, "ghs_"):
		return "app installation token"
	case strings.HasPrefix(c.Token, "ghp_"), strings.HasPrefix(c.Token, "github_pat_"):
		return "personal access token"
	default:
		return "organisation setup credential"
	}
}

// platformTokenPath is where this operator keeps that credential, under their
// own store rather than beside a checkout (internal/kakt-cli/opstore).
const platformTokenPath = "github/github-pat-token"

// PlatformTokenEnv names the ACCESS, not its implementation: the credential that
// creates this tenant's repository in their organisation. Deliberately not
// GITHUB_TOKEN, which belongs to GitHub Actions inside a workflow and to tools
// that demand that exact name -- a value of ours under that name is read by
// whatever else happens to be in the environment.
const PlatformTokenEnv = "GITHUB_ORG_SETUP_TOKEN"

// TokenFromEnv resolves the credential the way the rest of the CLI does, so
// onboarding needs no separate setup on a machine that can already bootstrap.
//
// Takes nothing. It used to take the repository root, because the file it read
// was `<root>/k8-secrets/github/github-pat-token` -- a credential whose location
// depended on which directory the command ran in, and which a deleted clone took
// with it.
func TokenFromEnv() (Credential, error) {
	if t := strings.TrimSpace(os.Getenv(PlatformTokenEnv)); t != "" {
		return Credential{Token: t, Source: PlatformTokenEnv}, nil
	}
	// The operator's store (internal/kakt-cli/opstore), not a checkout.
	p, _ := opstore.Path(platformTokenPath)
	b, err := os.ReadFile(p)
	if err != nil {
		// A copy still sitting in a checkout is named, because the move to the
		// store is the whole reason a machine that worked last week does not now.
		// Named, not read: two copies go stale in one of them.
		moved := ""
		if old := opstore.LegacyLocation(platformTokenPath); old != "" {
			moved = fmt.Sprintf("\n\nThere is one in %s, which is no longer read. Move it:\n\n"+
				"  mkdir -p %s && mv %s %s", old, filepath.Dir(p), old, p)
		}
		// Says what to do, not only what is missing. Scaffolding is often the
		// first command anyone runs against this platform, and an error naming a
		// variable without saying where its value comes from is a support round
		// trip.
		return Credential{}, fmt.Errorf(
			"no credential for setting up the tenant's organisation.\n\n"+
				"Scaffolding creates a repository in the tenant's organisation, so it needs\n"+
				"a credential that may do that, in %s or in\n%s\n\n"+
				"Either kind works and you do not have to say which it is:\n\n"+
				"  an installation token for a GitHub App the tenant granted. Preferred,\n"+
				"  because the access is then the tenant's to revoke (ADR-062).\n\n"+
				"  a personal access token with the `repo` AND `workflow` scopes, from\n"+
				"  https://github.com/settings/tokens\n"+
				"  `workflow` is not implied by `repo`, and this repository carries the\n"+
				"  workflow that bootstraps the box.%s", PlatformTokenEnv, p, moved)
	}
	return Credential{Token: strings.TrimSpace(string(b)), Source: p}, nil
}

// CreateRepo creates <org>/<tenant>-gitops. It reports whether the repository
// already existed, because onboarding must be resumable: a run that failed after
// creating the repository has to be able to continue rather than requiring the
// operator to delete it first.
func CreateRepo(ctx context.Context, s Spec, cred Credential) (existed bool, err error) {
	api := githubAPI()
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
			"commit", "-q", "-m", "Create box " + s.RepoName()},
		{"remote", "add", "origin", remote},
		{"push", "-q", "-u", "origin", "main"},
		// The push URL carries the credential, and this directory is now a
		// working checkout: the next command runs inside it to declare the box's
		// first management cluster. Left as it was, the token would sit in
		// .git/config of a directory nobody treats as secret.
		{"remote", "set-url", "origin",
			fmt.Sprintf("https://github.com/%s/%s.git", s.GitOrg, s.RepoName())},
	}
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			// The remote carries the credential, so it is replaced rather than
			// printed: a failing push would otherwise write the token into logs
			// and into whatever collects them.
			clean := strings.ReplaceAll(string(out), cred.Token, "***")
			if msg := workflowScopeRefusal(clean, s, dir); msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("git %s: %w: %s", args[0], err, clean)
		}
	}
	return nil
}

// workflowScopeRefusal recognises the one push failure that is a scope and not
// a mistake, and answers it.
//
// A box's repository carries the workflow that bootstraps it (ADR-072), and
// GitHub refuses to let a personal access token create any file under
// .github/workflows/ unless the token carries the `workflow` scope -- a scope
// that is not implied by `repo`. The raw refusal is a git error naming a path,
// which reads as a repository problem rather than a credential one.
//
// Reported with what the state actually is: the repository exists and has no
// commits, so nothing is lost and the continuation finishes the box.
func workflowScopeRefusal(out string, s Spec, dir string) string {
	if !strings.Contains(out, "without `workflow` scope") &&
		!strings.Contains(out, "without 'workflow' scope") {
		return ""
	}
	return fmt.Sprintf(`the credential cannot create this repository's workflows.

GitHub refuses a personal access token that writes under .github/workflows/
unless the token carries the `+"`workflow`"+` scope, and `+"`repo`"+` does not imply it. Your
box's repository carries the workflow that bootstraps it (ADR-072), so the push
stopped there.

  %s/%s exists and has NO commits. Nothing was pushed and no secret was set.

Give the credential the scope, then continue this box -- its identity is already
written and is reused:

  1. https://github.com/settings/tokens  ->  edit the token, check `+"`workflow`"+`
     (or create a new one with `+"`repo`"+` and `+"`workflow`"+`, and put it in %s)
  2. kakt box init --resume %s

An App installation token avoids this: the App's Workflows permission is granted
once by the organisation, and is preferred anyway (ADR-062).`,
		s.GitOrg, s.RepoName(), mustStorePath(), dir)
}

// mustStorePath names where this operator keeps the platform credential, for a
// message that is useless without it.
func mustStorePath() string {
	p, err := opstore.Path(platformTokenPath)
	if err != nil {
		return "~/.kamarkat/" + platformTokenPath
	}
	return p
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
