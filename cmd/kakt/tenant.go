package main

import (
	"bufio"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

var (
	scaffoldSpec            tenant.Spec
	scaffoldTemplate        string
	scaffoldOut             string
	scaffoldDryRun          bool
	scaffoldProviderToken   string
	scaffoldGitopsToken     string
	scaffoldTailscaleKey    string
	scaffoldEscrowURL       string
	scaffoldEscrowClientID  string
	scaffoldEscrowSecret    string
	scaffoldEscrowProjectID string
	scaffoldLocal           bool
	// -1 rather than 0, because 0 is a real worker count and the default depends
	// on the environment, which is not known when flags are declared.
	scaffoldWorkers int
	// workloadClusterWorkers is read through the -1 sentinel above rather than
	// written straight onto the struct, so "not passed" survives to Add().
	workloadClusterWorkers int
	scaffoldNoPrompt       bool
)

func newTenantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Onboard and maintain tenant boxes",
	}
	cmd.AddCommand(newTenantScaffoldCmd())
	cmd.AddCommand(newTenantSetGitopsTokenCmd())
	cmd.AddCommand(newAddWorkloadClusterCmd())
	return cmd
}

var workloadCluster tenant.WorkloadCluster

// readTokenValue obtains a credential without it landing anywhere durable.
//
// Echo is disabled when stdin is a terminal, so a pasted token does not remain
// on screen or in the scrollback. When stdin is a pipe it is read straight
// through, which is what a password manager or a CI step does.
func readTokenValue(cmd *cobra.Command) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}

	// Say what this value IS, not what permission it needs. The permission
	// belongs to the gh login performing the write, and describing it here sent
	// an operator to mint a new PAT when the one they had was fine.
	fmt.Print("\nGITOPS_TOKEN\n" +
		"  The credential application builds will use to commit a chart version\n" +
		"  into this tenant's GitOps repository. Needs write access to that\n" +
		"  repository; it is stored, not used to store itself.\n\n" +
		"Token (not echoed): ")
	b, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// newTenantSetGitopsTokenCmd republishes the organisation credential an
// application build needs.
//
// It exists because scaffold's attempt is non-fatal by design: a token without
// the organisation's "Secrets: write" permission leaves the box complete and the
// credential unset, and the only recovery was the `gh secret set` line scaffold
// printed in a warning. That makes the tenant perform a step ADR-084 assigns to
// the platform, once per box, from a message they saw days earlier.
func newTenantSetGitopsTokenCmd() *cobra.Command {
	var org, repo, gitopsRepo, token string
	var allRepos bool
	cmd := &cobra.Command{
		Use:          "set-gitops-token",
		Short:        "Publish the organisation credential application builds use",
		SilenceUsage: true,
		Long: `Publish GITOPS_TOKEN at the organisation level, so every application
repository the tenant owns inherits it (ADR-084).

Scaffold does this already. This re-runs it for a box whose scaffold-time token
lacked the organisation's "Secrets: write" permission -- the case where the
scaffold succeeded, printed a warning, and left application builds unable to pin
a chart version.

The token needs that one fine-grained permission, not admin:org.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" && org == "" {
				return fmt.Errorf("pass --org (organisation-wide) or --repo owner/name (one repository)")
			}
			if strings.TrimSpace(token) == "" {
				// Never an argument: a token passed as a flag value is in shell
				// history and in the process table.
				//
				// Prompted when a person is typing, piped when they are not.
				// Reading stdin unconditionally is what this did first, and on a
				// terminal that is an unannounced block: no prompt, no cursor,
				// nothing to indicate it wants input or that Ctrl-D ends it. It
				// reads as a hang, which is the worst thing a credential command
				// can do -- the operator cannot tell whether it is waiting or
				// has already made a remote call.
				v, err := readTokenValue(cmd)
				if err != nil {
					return err
				}
				token = v
			}
			// The LENGTH, never the value. An operator who pasted nothing sees
			// a number that is obviously wrong; one who pasted correctly sees a
			// number that matches what they hold.
			n := len(strings.TrimSpace(token))

			if repo != "" {
				if err := tenant.PublishRepoGitopsToken(cmd.Context(), repo, token); err != nil {
					return err
				}
				fmt.Printf("\n  GITOPS_TOKEN published on %s (%d characters).\n\n", repo, n)
				return nil
			}

			// Whether the organisation form will actually deliver, checked
			// BEFORE publishing. On the free plan an organisation secret does
			// not reach private repositories: the write succeeds, the secret is
			// listed against the repository, and the workflow receives an empty
			// string. Publishing without saying so produces a credential that
			// looks present everywhere except where it is read.
			reaches, plan, perr := tenant.OrgSecretsReachPrivateRepos(cmd.Context(), org)
			if perr != nil {
				fmt.Printf("  note: %v\n", perr)
			}

			if !reaches && !allRepos {
				return fmt.Errorf("the %s organisation is on the %s plan, where an organisation "+
					"secret is delivered only to PUBLIC repositories.\n\n"+
					"Publishing it here would succeed, appear in each repository's list of "+
					"available organisation secrets, and arrive in every private repository's "+
					"workflow as an empty string -- with nothing at any layer reporting that it "+
					"was withheld.\n\n"+
					"Publish per repository instead:\n"+
					"  kakt tenant set-gitops-token --repo <owner>/<name>\n"+
					"  kakt tenant set-gitops-token --org %s --discover --gitops-repo <owner>/<name>\n\n"+
					"The second covers every repository that exists TODAY. One created later is "+
					"not covered and must be given the credential when it is created -- which is "+
					"the inheritance this plan does not provide.", org, plan, org)
			}

			if allRepos {
				if gitopsRepo == "" {
					return fmt.Errorf("--gitops-repo owner/name is required: it is what identifies " +
						"which repositories write to THIS box, and without it the only alternative " +
						"is publishing the credential to every repository in the organisation")
				}
				repos, err := tenant.ReposNeedingGitopsToken(cmd.Context(), org, gitopsRepo)
				if err != nil {
					return err
				}
				if len(repos) == 0 {
					return fmt.Errorf("no repository in %s has a workflow that reads "+
						"secrets.GITOPS_TOKEN and names %s.\n\n"+
						"Nothing needs the credential yet. An application repository needs it once "+
						"its build commits a chart version here", org, gitopsRepo)
				}
				var failed int
				for _, r := range repos {
					if err := tenant.PublishRepoGitopsToken(cmd.Context(), r, token); err != nil {
						fmt.Printf("  FAILED  %-44s %v\n", r, err)
						failed++
						continue
					}
					fmt.Printf("  set     %s\n", r)
				}
				fmt.Printf("\n  GITOPS_TOKEN published on %d of %d repository/repositories that write\n",
					len(repos)-failed, len(repos))
				fmt.Printf("  to %s (%d characters).\n", gitopsRepo, n)
				fmt.Printf("  A repository created later is NOT covered: run this again, or use --repo.\n\n")
				if failed > 0 {
					return fmt.Errorf("%d repository/repositories did not receive the credential", failed)
				}
				return nil
			}

			if err := tenant.PublishOrgGitopsToken(cmd.Context(), org, token); err != nil {
				return err
			}
			fmt.Printf("\n  GITOPS_TOKEN published on the %s organisation (%d characters).\n", org, n)
			fmt.Printf("  Every application repository inherits it, including ones created later.\n\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "Git organisation to publish the credential on")
	cmd.Flags().StringVar(&repo, "repo", "", "Publish on ONE repository (owner/name) instead of the organisation")
	cmd.Flags().BoolVar(&allRepos, "discover", false, "Find the repositories whose workflows write to --gitops-repo and publish on those")
	cmd.Flags().StringVar(&gitopsRepo, "gitops-repo", "", "This box's GitOps repository (owner/name), used to identify which repositories write to it")
	cmd.Flags().StringVar(&token, "token", "", "The token; omit to read it from stdin, which keeps it out of shell history")
	cmd.MarkFlagsMutuallyExclusive("repo", "org")
	cmd.MarkFlagsMutuallyExclusive("repo", "discover")
	return cmd
}

// newAddWorkloadClusterCmd declares an additional workload cluster in a
// repository that already holds a management cluster.
//
// One management cluster per repository, as many workload clusters as it
// declares -- kubefirst's layout, where registry/clusters/NAME/ exists once per
// cluster and the management cluster's ArgoCD reaches into each.
//
// The name is given, not derived. It was a literal shipped in the bundle, so
// every box provisioned a workload cluster under the same name and every
// identity derived from it collided across boxes -- the CNPG archive prefix most
// damagingly, where barman refused each new box's WALs with "Expected empty
// archive" and no backup ever completed anywhere.
func newAddWorkloadClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-cluster",
		Short: "Declare an additional workload cluster in a tenant's repository",
		Long: `Declare a workload cluster and the Application that creates it.

Writes clusters/<name>/ -- the platform this cluster runs, addressed to this
cluster -- and clusters/<mgmt>/<name>-infrastructure.yaml, the Application that
brings it into being, addressed to the management cluster. Commit and push, and
the management cluster provisions it.

The name is this cell's id (ADR-047): the SpokePool composition derives
metadata.labels.cluster-name from it and the fleet ApplicationSets select on it, so
choose something meaningful -- it names the cluster for its whole life.`,
		RunE: runAddWorkloadCluster,
	}
	f := cmd.Flags()
	f.StringVar(&workloadCluster.GitopsDir, "gitops-dir", ".", "the tenant repository")
	f.StringVar(&workloadCluster.MgmtCluster, "mgmt-cluster", "", "the management cluster that creates this one (required)")
	f.StringVar(&workloadCluster.Name, "name", "", "the workload cluster, and this cell's id (required)")
	f.StringVar(&workloadCluster.Provider, "provider", "hetzner", "cloud provider")
	f.StringVar(&workloadCluster.Region, "region", "hel1", "cloud region")
	f.StringVar(&workloadCluster.Environment, "environment", "dev", "environment slug")
	f.StringVar(&workloadCluster.BundleVersion, "bundle-version", "", "platform version this cluster starts on (required)")
	f.StringVar(&workloadCluster.TenantID, "tenant", "", "the tenant that owns it (required)")
	f.StringVar(&workloadCluster.GitopsRepoURL, "gitops-repo-url", "", "this repository's URL (required)")
	f.StringVar(&workloadCluster.PlatformRepoURL, "platform-repo-url", "", "the platform repository URL")
	f.StringVar(&workloadCluster.BundleRegistry, "bundle-registry", "ghcr.io/kamarkat/charts", "the chart registry")
	f.StringVar(&workloadCluster.ChartSource, "chart-source", "", "chart source, as the management cluster was scaffolded with")
	f.StringVar(&workloadCluster.PublicTLSIssuer, "public-tls-issuer", "letsencrypt-prod", "ACME issuer for public certificates")
	// -1 is "no opinion", so an explicit --workers is distinguishable from the
	// default. hybrid runs on the tenant's own hardware alone and has no cloud
	// workers at all; silently ignoring a number someone typed would let them
	// believe they had bought capacity they do not have.
	f.IntVar(&workloadClusterWorkers, "workers", -1,
		"cloud worker nodes this cluster starts with (default: 2 on hetzner). "+
			"Not applicable on hybrid, which runs on your own premises alone (ADR-075)")

	for _, required := range []string{"mgmt-cluster", "name", "bundle-version", "tenant", "gitops-repo-url"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

func runAddWorkloadCluster(cmd *cobra.Command, args []string) error {
	// The sentinel resolved against the provider, here rather than in Add(), so
	// the struct that reaches the tenant package carries a real number.
	//
	// hybrid gets no cloud workers at all (ADR-075): its capacity is the home
	// workers named in the claim, and this flag has nothing to spend. A number
	// typed here would be silently dropped by the hybrid composition -- which
	// carries no replicas patch -- so it is refused instead of ignored.
	switch {
	case workloadCluster.Provider == "hybrid" && workloadClusterWorkers > 0:
		return fmt.Errorf("--workers %d is not applicable on hybrid: a hybrid cluster runs on "+
			"the nodes on your own premises and buys none (ADR-075).\n\n"+
			"Its capacity is the home-workers list in "+
			"registry/clusters/%s/infrastructure/spokepool-hybrid.yaml, which you fill in with "+
			"your own hardware and provision with scripts/hybrid/provision-flatcar-worker.sh.\n\n"+
			"Re-run without --workers, then edit that list", workloadClusterWorkers, workloadCluster.Name)
	case workloadCluster.Provider == "hybrid":
		workloadCluster.Workers = 0
	case workloadClusterWorkers < 0:
		workloadCluster.Workers = 2
	default:
		workloadCluster.Workers = workloadClusterWorkers
	}

	written, err := workloadCluster.Add()
	if err != nil {
		return err
	}
	fmt.Printf("✓ workload cluster %q declared\n", workloadCluster.Name)
	for _, p := range written {
		fmt.Printf("  %s\n", p)
	}
	if workloadCluster.Provider == "hybrid" {
		fmt.Printf("\n! %s has NO capacity yet.\n", workloadCluster.Name)
		fmt.Println("  A hybrid cluster runs on your own premises alone, and the claim ships")
		fmt.Println("  home-workers: '[]'. Until you name real hardware there, its only node is")
		fmt.Println("  a tainted control plane: every workload pends, ArgoCD's sync waves never")
		fmt.Println("  go Healthy, and bootstrap fails much later reporting a missing database.")
		fmt.Printf("  Edit registry/clusters/%s/infrastructure/spokepool-hybrid.yaml, then run\n",
			workloadCluster.Name)
		fmt.Println("  scripts/hybrid/provision-flatcar-worker.sh for each node.")
	}

	fmt.Println("\nCommit and push; the management cluster provisions it.")
	return nil
}

func newTenantScaffoldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scaffold",
		Short: "Create and render a tenant's <tenant>-gitops repository",
		Long: `Create <org>/<tenant>-gitops and render it from the platform template.

Onboarding, as ADR-062 defines it. The repository is created in the TENANT's
organisation and is theirs; the platform holds a repository-scoped credential
that can propose changes to it and nothing else. No cloud credential is involved:
the tenant runs Day-0 against its own account.

Scaffolding happens once. Every change after it is a pull request against the
rendered repository (ADR-064).

--dry-run renders to a local directory and creates nothing, which is how to see
what a tenant would receive before any repository exists.`,
		RunE: runTenantScaffold,
	}
	f := cmd.Flags()
	f.StringVar(&scaffoldSpec.TenantID, "tenant", "", "tenant identifier (required)")
	f.StringVar(&scaffoldSpec.GitOrg, "org", "", "the tenant's git organisation (required)")
	f.StringVar(&scaffoldSpec.Domain, "domain", "", "the tenant's base domain (required)")
	f.StringVar(&scaffoldSpec.Provider, "provider", "hetzner", "cloud provider")
	f.StringVar(&scaffoldSpec.Region, "region", "hel1", "cloud region")
	f.StringVar(&scaffoldSpec.Environment, "environment", "dev", "environment slug")
	// The DNS label this box sits under, declared rather than derived from the
	// environment (ADR-051 amendment 2026-09-18). Empty publishes on the apex.
	// kubefirst's SubdomainName, and the reason its management cluster needs no
	// environment of its own.
	f.StringVar(&scaffoldSpec.Subdomain, "subdomain", "",
		"DNS label every hostname on this box sits under; empty publishes on the apex")
	f.IntVar(&scaffoldWorkers, "workers", -1,
		"cloud worker nodes this box starts with (default: 2, every environment). "+
			"Pass 0 to run on nodes on your own premises alone, which then have to "+
			"be there (ADR-075)")
	f.StringVar(&scaffoldSpec.ClusterName, "cluster", "", "control plane cluster name (default <tenant>-hub)")
	f.StringVar(&scaffoldSpec.BoxID, "box-id", "", "the logical hub, pinned forever (default: the cluster name, this box's first incarnation)")
	// Defaults to the version this binary carries (ADR-068). "main" was the old
	// default and is a branch name where a chart version belongs: a repository
	// scaffolded on it pins a version that does not resolve, and the workflow it
	// carries calls a platform workflow at a tag named vmain.
	//
	// A released binary therefore needs no version flag. An unreleased one has no
	// version to offer and says so rather than inventing one.
	f.StringVar(&scaffoldSpec.BundleVersion, "bundle-version", defaultBundleVersion(),
		"platform bundle version this box starts on (default: the version this CLI carries)")
	// Platform facts rather than tenant decisions, so they are hidden. They stay
	// flags because a platform developer testing against a fork needs them, and
	// removing them would mean editing the source to do that.
	f.StringVar(&scaffoldSpec.PlatformRepoURL, "platform-repo", "https://github.com/kamarkat/kamarkat", "where the bundle is sourced from")
	f.StringVar(&scaffoldSpec.PlatformRevision, "platform-revision", "",
		"the platform branch a development box reads its chart from (default: the branch you are on). Ignored by a released box")
	f.BoolVar(&scaffoldLocal, "local", false,
		"clone the repository and print the bootstrap command instead of dispatching a workflow. For testing the platform end to end without a release")
	f.StringVar(&scaffoldSpec.BundleRegistry, "bundle-registry", "ghcr.io/kamarkat/charts",
		"registry published bundles are pulled from (no scheme)")
	f.BoolVar(&scaffoldSpec.Private, "private", true, "create the repository private")
	f.BoolVar(&scaffoldSpec.PrivateRegistry, "private-registry", false,
		"this tenant runs private images; a registry credential becomes required (ADR-066)")
	// The tenant's own credentials. Flags so automation can supply them; prompted
	// when a terminal is present and they are absent; and when neither, the run
	// stops after creating the repository and says what remains.
	//
	// Never defaulted from the platform's environment. A token this machine
	// happens to hold is the platform's, and writing it into a tenant's
	// repository would make the platform's access the tenant's access.
	f.StringVar(&scaffoldProviderToken, "provider-token", "",
		"the tenant's cloud API token; their clusters are created with it")
	f.StringVar(&scaffoldEscrowURL, "escrow-url", "",
		"an Infisical the TENANT controls and this box does not host, holding its master keys (ADR-076). Usually https://app.infisical.com")
	f.StringVar(&scaffoldEscrowProjectID, "escrow-project-id", "", "the project in that Infisical the backup is written to")
	f.StringVar(&scaffoldEscrowClientID, "escrow-client-id", "", "a machine identity with write access to that project")
	f.StringVar(&scaffoldEscrowSecret, "escrow-client-secret", "", "the matching machine identity client secret")
	f.StringVar(&scaffoldTailscaleKey, "tailscale-authkey", "",
		"a Tailscale auth key; the hybrid provider's control plane and home workers join the tenant's tailnet with it (ADR-046)")
	f.StringVar(&scaffoldGitopsToken, "gitops-token", "",
		"write access to the tenant's repository, so Day-0 can commit what it generates")
	f.BoolVar(&scaffoldNoPrompt, "no-prompt", false,
		"never prompt; stop after creating the repository if a secret is missing")

	f.StringVar(&scaffoldTemplate, "template", "manifests/tenants/gitops-template", "template to render")

	for _, hidden := range []string{"platform-repo", "bundle-registry", "template"} {
		_ = f.MarkHidden(hidden)
	}
	f.StringVar(&scaffoldOut, "out", "", "render here instead of a temporary directory")
	f.BoolVar(&scaffoldDryRun, "dry-run", false, "render only; create and push nothing")
	return cmd
}

// ADR-068: "Overriding a source requires naming its version." A build may be
// pointed at another registry or repository; doing so without also stating the
// version to request is refused rather than defaulted.
//
// The default is the version THIS BINARY carries, and the overridden source is
// not known to hold it. Defaulting produces a repository pinned to a version the
// registry it names may never have published, and the failure surfaces at the
// first Application that cannot load its source -- long after the run that
// chose it reported success.
func refuseUndeclaredSourceOverride(cmd *cobra.Command) error {
	f := cmd.Flags()
	if f.Changed("bundle-version") {
		return nil
	}
	for _, name := range []string{"bundle-registry", "platform-repo"} {
		if !f.Changed(name) {
			continue
		}
		return fmt.Errorf(
			"--%s points at a source this build did not publish to, so the "+
				"version it would request (%q, the version this CLI carries) is "+
				"not known to be there. Pass --bundle-version naming a version "+
				"that source holds (ADR-068)",
			name, defaultBundleVersion())
	}
	return nil
}

func runTenantScaffold(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	if err := refuseUndeclaredSourceOverride(cmd); err != nil {
		return err
	}

	// A development box reads the platform chart from the branch under test, so
	// the change being tested is the change the cluster reconciles. Resolved here
	// rather than in the renderer: this is the only place that knows a working
	// tree is present, and a released scaffold ignores it entirely.
	if scaffoldSpec.PlatformRevision == "" {
		if out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
			if b := strings.TrimSpace(string(out)); b != "" && b != "HEAD" {
				scaffoldSpec.PlatformRevision = b
			}
		}
	}

	if scaffoldSpec.ClusterName == "" && scaffoldSpec.TenantID != "" {
		scaffoldSpec.ClusterName = scaffoldSpec.TenantID + "-hub"
	}

	// The box's id starts as its first cluster's name, and the two diverge the
	// first time the hub is replaced (ADR-111 §1). Written into the tenant's
	// repository as its own value rather than left to be derived later: once a
	// successor exists, nothing can recompute which name the escrow was keyed by.
	if scaffoldSpec.BoxID == "" {
		scaffoldSpec.BoxID = scaffoldSpec.ClusterName
	}
	// Only when actually passed. The sentinel keeps "zero workers" distinguishable
	// from "no opinion", so the environment's own default survives.
	if scaffoldWorkers >= 0 {
		scaffoldSpec.Workers = &scaffoldWorkers
	}

	dir := scaffoldOut
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "tenant-scaffold-"); err != nil {
			return err
		}
		if !scaffoldDryRun {
			defer os.RemoveAll(dir)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	if err := tenant.Render(scaffoldTemplate, dir, scaffoldSpec); err != nil {
		return err
	}
	fmt.Printf("[scaffold] rendered %s into %s\n", scaffoldSpec.RepoName(), dir)

	// Before the dry-run return, so the run that exists to show what would be
	// created also shows that it could not bootstrap.
	if w := scaffoldSpec.CapacityWarning(); w != "" {
		fmt.Printf("\n[scaffold] ⚠  %s\n\n", w)
	}

	if scaffoldDryRun {
		fmt.Println("[scaffold] dry run: no repository created, nothing pushed")
		return printTree(dir)
	}

	root, err := os.Getwd()
	if err != nil {
		return err
	}
	cred, err := tenant.TokenFromEnv(root)
	if err != nil {
		return err
	}
	// The KIND of credential, never its source. Naming the source printed the
	// filesystem path of a secret file to the terminal and to whatever collects
	// it; naming nothing at all made the repository appear out of nowhere and the
	// prompts that follow read as though they had authorised it. They had not:
	// this is the platform's credential and it creates the repository, while the
	// secrets asked for below are the tenant's and are only stored in it.
	//
	// A failure still names the path, because there it is the fix rather than an
	// aside.
	kind := "app installation token"
	if !cred.IsApp {
		kind = "personal access token"
	}
	fmt.Printf("[scaffold] authenticating as the platform (%s)\n", kind)

	existed, err := tenant.CreateRepo(ctx, scaffoldSpec, cred)
	if err != nil {
		return err
	}
	if existed {
		// Resumable rather than fatal: a run that failed after creating the
		// repository but before pushing must be able to continue. Publish refuses
		// once the repository has commits, so a repository that was only created
		// is resumed and one that was scaffolded is not overwritten.
		fmt.Printf("[scaffold] %s/%s already exists; continuing\n", scaffoldSpec.GitOrg, scaffoldSpec.RepoName())
	} else {
		fmt.Printf("[scaffold] created %s/%s\n", scaffoldSpec.GitOrg, scaffoldSpec.RepoName())
	}

	if err := tenant.Publish(ctx, dir, scaffoldSpec, cred); err != nil {
		return err
	}
	fmt.Printf("[scaffold] pushed to %s\n", "https://github.com/"+scaffoldSpec.GitOrg+"/"+scaffoldSpec.RepoName())

	// The repository can bootstrap itself, but only with the tenant's own
	// secrets: the workflow runs under them and the platform holds neither
	// (ADR-065, ADR-072). Asked for here rather than left as a follow-up, because
	// a scaffolded repository that cannot bootstrap looks finished.
	secrets := tenant.Secrets{
		ProviderToken:      scaffoldProviderToken,
		GitopsToken:        scaffoldGitopsToken,
		TailscaleAuthkey:   scaffoldTailscaleKey,
		EscrowURL:          scaffoldEscrowURL,
		EscrowClientID:     scaffoldEscrowClientID,
		EscrowClientSecret: scaffoldEscrowSecret,
		EscrowProjectID:    scaffoldEscrowProjectID,
	}
	if !scaffoldNoPrompt {
		var err error
		if secrets, err = secrets.Prompt(scaffoldSpec); err != nil {
			return err
		}
	}

	w := bufio.NewWriter(os.Stdout)
	// The escrow is required (ADR-076), and its absence is reported before the
	// dispatch decision so a tenant sees one account of what is missing rather
	// than discovering the second thing after fixing the first.
	if err := secrets.RequireEscrow(); err != nil {
		tenant.HandoverInstructions(scaffoldSpec, secrets, w)
		w.Flush()
		return err
	}

	// Usable, not merely supplied -- and after the presence check, so a box with
	// no escrow gets that refusal's guidance rather than a connection error.
	//
	// An Infisical project has a TYPE, and only a secret-manager project accepts
	// what an escrow does; a kms project authenticates perfectly and then refuses
	// every read and write. Checked here, where the value was just given and can
	// still be corrected, rather than by the hub-operator forty minutes into a
	// bootstrap with the box stalled on database roles that cannot be created.
	if err := secrets.VerifyEscrow(); err != nil {
		return err
	}

	// What this box's own configuration calls for, and only that. A development
	// box is not asked for object storage; a tenant running public images is
	// never asked for a registry credential. Asking for either anyway would turn
	// an implementation detail of the platform into an onboarding requirement.
	if err := secrets.RequireSelectedCapabilities(scaffoldSpec); err != nil {
		tenant.HandoverInstructions(scaffoldSpec, secrets, w)
		w.Flush()
		return err
	}

	// Where this box's own telemetry goes, if anywhere. Said plainly and
	// carrying no consequence: every capability ships to every tenant, and an
	// unconfigured destination withholds nothing.
	fmt.Fprintf(w, "\n%s\n", secrets.ObservabilityDestination(scaffoldSpec))

	// Provider-aware: a hybrid box also needs a tailnet key, and dispatching
	// without one starts a run the workflow refuses on purpose.
	if !secrets.CompleteFor(scaffoldSpec.Provider) {
		// Not an error. A repository exists at this point, so what is owed is an
		// accurate account of where this stopped and what completes it.
		tenant.HandoverInstructions(scaffoldSpec, secrets, w)
		return nil
	}

	if err := tenant.SetSecrets(ctx, scaffoldSpec, secrets); err != nil {
		return err
	}

	// --local stops here and hands the operator the bootstrap instead of running
	// it in the tenant's CI. The repository, its secrets and its contents are
	// identical either way -- what differs is only where Day-0 executes, so what
	// is tested locally is the same box a workflow would build (ADR-072).
	//
	// This exists because the alternative is a published release per change: a
	// version is consumed by any release that begins publishing it (ADR-063), so
	// testing a one-line fix by the dispatch path burns a version number.
	if scaffoldLocal {
		return tenant.LocalHandover(ctx, scaffoldSpec, secrets, os.Stdout)
	}

	if err := tenant.Dispatch(ctx, scaffoldSpec); err != nil {
		return err
	}

	repo := scaffoldSpec.GitOrg + "/" + scaffoldSpec.RepoName()
	fmt.Printf("\n[scaffold] bootstrap dispatched on %s\n", repo)
	fmt.Printf("  https://github.com/%s/actions/workflows/bootstrap-cluster.yml\n\n", repo)
	fmt.Printf("It runs for a few hours. Follow it with:\n")
	fmt.Printf("  gh run watch --repo %s\n", repo)
	return nil
}

func printTree(dir string) error {
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Println("  ", rel)
		return nil
	})
}

// defaultBundleVersion is the version this binary carries, or empty when it
// carries none.
//
// Empty rather than a guess: an unreleased build has no published version to
// offer, and scaffolding a repository pinned to something that does not resolve
// produces a box that cannot bootstrap and says nothing about why until the
// first Application fails to load its source.
func defaultBundleVersion() string {
	if versions.IsReleaseBuild() {
		return versions.BundleVersion
	}
	return ""
}
