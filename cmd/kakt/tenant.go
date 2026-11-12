package main

import (
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// workloadClusterWorkers is read through a -1 sentinel rather than written
// straight onto the struct, so "not passed" survives to Add(): 0 is a real
// worker count (ADR-075) and the default is not known when flags are declared.
var workloadClusterWorkers int

func newTenantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Onboard and maintain tenant boxes",
	}
	cmd.AddCommand(newTenantSetGitopsTokenCmd())
	cmd.AddCommand(newAddManagementClusterCmd())
	cmd.AddCommand(newAddWorkloadClusterCmd())
	cmd.AddCommand(newRemoveWorkloadClusterCmd())
	cmd.AddCommand(newClusterStatusCmd())
	cmd.AddCommand(newAddAppCmd())
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
	fmt.Print("\nGITOPS_REPO_WRITE_TOKEN\n" +
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
		Long: `Publish GITOPS_REPO_WRITE_TOKEN at the organisation level, so every application
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
				fmt.Printf("\n  GITOPS_REPO_WRITE_TOKEN published on %s (%d characters).\n\n", repo, n)
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
						"secrets.GITOPS_REPO_WRITE_TOKEN and names %s.\n\n"+
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
				fmt.Printf("\n  GITOPS_REPO_WRITE_TOKEN published on %d of %d repository/repositories that write\n",
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
			fmt.Printf("\n  GITOPS_REPO_WRITE_TOKEN published on the %s organisation (%d characters).\n", org, n)
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

Writes registry/clusters/<name>/ -- the platform this cluster runs, addressed to
this cluster -- and registry/clusters/<mgmt>/<name>-infrastructure.yaml, the
Application that brings it into being, addressed to the management cluster. Commit and push, and
the management cluster provisions it.

The name is what applications name to run here: the SpokePool composition
derives metadata.labels.cluster-name from it and the application ApplicationSets
select on it, so choose something meaningful -- it names the cluster, its secret
path and its backup prefix for their whole lives.`,
		RunE: runAddWorkloadCluster,
	}
	f := cmd.Flags()
	f.StringVar(&workloadCluster.GitopsDir, "gitops-dir", ".", "the tenant repository")
	f.StringVar(&workloadCluster.MgmtCluster, "mgmt-cluster", "", "the management cluster that creates this one (required)")
	f.StringVar(&workloadCluster.Name, "name", "", "the workload cluster's name, and what applications target (required)")
	f.StringVar(&workloadCluster.Provider, "provider", "hetzner", "cloud provider")
	f.StringVar(&workloadCluster.Region, "region", "hel1", "cloud region")
	// This cluster's, not the hub's (ADR-111 Revision 7). The management cluster
	// declares no environment; a workload cluster declares the stage it serves,
	// and it is this value the supported-topology check is asked about.
	f.StringVar(&workloadCluster.Environment, "environment", "dev",
		"the stage THIS workload cluster serves: dev (default), stg or prod. Not a "+
			"destination -- applications name the cluster they run on")
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
			"registry/clusters/%s/infrastructure/spokepool.yaml, which you fill in with "+
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
		fmt.Printf("  Edit registry/clusters/%s/infrastructure/spokepool.yaml, then run\n",
			workloadCluster.Name)
		fmt.Println("  scripts/hybrid/provision-flatcar-worker.sh for each node.")
	}

	fmt.Println("\nCommit and push; the management cluster provisions it.")
	return nil
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

// addApp is the application `kakt tenant add-app` declares.
var addApp tenant.AppSpec

// addAppWorkloads is repeatable: an application is one or more deployables, and
// each is its own directory and its own chart (ADR-073, ADR-088).
var addAppWorkloads []string

// newAddAppCmd declares an application in an environment.
//
// The counterpart of add-cluster, and it was missing: a cluster is a command and
// an application was `mkdir -p`, two `cp`s and a hand-edit of tokens that the two
// templates spelled five ways between them and that TOKENS.md did not list.
//
// What the command adds beyond the copying is the check. The cluster an
// application names is verified before a file is written -- it must exist, be a
// workload cluster rather than the management one, and serve the environment the
// directory names. Nothing used to read that name until the first
// `kakt app secrets` call, and a clusters generator that matches nothing
// generates zero Applications while reporting Healthy.
func newAddAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-app",
		Short: "Declare an application in an environment",
		Long: `Declare an application and the workloads it runs.

Writes environments/<env>/<app>/values.yaml -- what the application is, and the
cluster it runs on -- plus one directory and Chart.yaml per workload. Commit and
push, and the management cluster generates it.

The cluster is checked before anything is written: it must be a workload cluster
this repository declares, serving the environment named here. Several clusters
may serve one environment, so the application names which.`,
		SilenceUsage: true,
		RunE:         runAddApp,
	}
	f := cmd.Flags()
	f.StringVar(&addApp.GitopsDir, "gitops-dir", ".", "the tenant repository")
	f.StringVar(&addApp.Environment, "env", "", "the environment this application is declared in (required)")
	f.StringVar(&addApp.AppID, "app", "", "the application: the product, and its isolation unit (required)")
	// No --cluster. An application is deployed to every workload cluster serving
	// its environment, so there is nothing to choose here; `kakt tenant
	// add-cluster --environment dev` is what puts a cluster in that set.
	f.StringVar(&addApp.OwnerEmail, "owner-email", "", "who is paged for it (required)")
	f.StringSliceVar(&addAppWorkloads, "workload", nil,
		"a deployable inside this application; repeat for each")

	for _, required := range []string{"env", "app", "owner-email"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

func runAddApp(cmd *cobra.Command, _ []string) error {
	addApp.Workloads = addAppWorkloads

	written, err := tenant.AddApp(addApp)
	if err != nil {
		return err
	}
	for _, p := range written {
		rel, relErr := filepath.Rel(addApp.GitopsDir, p)
		if relErr != nil {
			rel = p
		}
		fmt.Printf("[add-app] ✓ %s\n", rel)
	}

	// The aggregate is the shared Gateway's listener list and has one producer
	// (ADR-096). A new application declares no hostnames yet, so this changes
	// nothing today -- it is run because a preflight check asserts the file
	// matches the declarations exactly, and leaving it to be remembered later is
	// how it stops matching.
	clusters, err := tenant.AggregatePublicHosts(addApp.GitopsDir)
	if err != nil {
		return fmt.Errorf("regenerate the public hostname aggregate: %w", err)
	}
	fmt.Printf("[add-app] %d cluster aggregate(s) checked\n", len(clusters))

	fmt.Printf(`
Next:
  1. Fill in environments/%s/%s/values.yaml -- its quota, public hostnames and
     the secrets it declares.
  2. git add environments && git commit && git push
  3. kakt app secrets status %s %s   -- what it declares, and what is present
`, addApp.Environment, addApp.AppID, addApp.Environment, addApp.AppID)
	return nil
}
