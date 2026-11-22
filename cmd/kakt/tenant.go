package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/kubernetes"

	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"github.com/kamarkat/kamarkat/internal/platform/encryption"
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
	cmd.AddCommand(newAddWorkloadCmd())
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
// It exists because render's attempt is non-fatal by design: a token without
// the organisation's "Secrets: write" permission leaves the box complete and the
// credential unset, and the only recovery was the `gh secret set` line render
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

Render does this already. This re-runs it for a box whose render-time token
lacked the organisation's "Secrets: write" permission -- the case where the
render succeeded, printed a warning, and left application builds unable to pin
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
	f.StringVar(&workloadCluster.MgmtCluster, "mgmt-cluster", "", "the management cluster that creates this one; read from registry/box.yaml when unset")
	f.StringVar(&workloadCluster.Name, "name", "", "the workload cluster's name, and what applications target (required)")
	f.StringVar(&workloadCluster.Provider, "provider", "hetzner", "cloud provider")
	f.StringVar(&workloadCluster.Region, "region", "hel1", "cloud region")
	// This cluster's, not the hub's (ADR-111 Revision 7). The management cluster
	// declares no environment; a workload cluster declares the stage it serves,
	// and it is this value the supported-topology check is asked about.
	f.StringVar(&workloadCluster.Environment, "environment", "dev",
		"the stage THIS workload cluster serves: any name a DNS label allows, dev by "+
			"default. Not a "+
			"destination -- applications name the cluster they run on")
	f.StringVar(&workloadCluster.BundleVersion, "bundle-version", "", "platform version this cluster starts on; read from registry/box.yaml when unset")
	f.StringVar(&workloadCluster.TenantID, "tenant", "", "the tenant that owns it; read from registry/box.yaml when unset")
	f.StringVar(&workloadCluster.GitopsRepoURL, "gitops-repo-url", "", "this repository's URL; composed from registry/box.yaml when unset")
	f.StringVar(&workloadCluster.PlatformRepoURL, "platform-repo-url", "", "the platform repository URL")
	f.StringVar(&workloadCluster.BundleRegistry, "bundle-registry", "ghcr.io/kamarkat/charts", "the chart registry")
	f.StringVar(&workloadCluster.ChartSource, "chart-source", "", "chart source, as the management cluster was rendered with")
	f.StringVar(&workloadCluster.PublicTLSIssuer, "public-tls-issuer", "letsencrypt-prod", "ACME issuer for public certificates")
	// -1 is "no opinion", so an explicit --workers is distinguishable from the
	// default. hybrid runs on the tenant's own hardware alone and has no cloud
	// workers at all; silently ignoring a number someone typed would let them
	// believe they had bought capacity they do not have.
	f.IntVar(&workloadClusterWorkers, "workers", -1,
		"cloud worker nodes this cluster starts with (default: 2 on hetzner). "+
			"Not applicable on hybrid, which runs on your own premises alone (ADR-075)")

	// ONLY THE NAME. Everything else the box declares is filled by fillFromBox
	// and checked there, with a message naming the field of registry/box.yaml it
	// would have come from. Marking them required here would refuse the command
	// before it had looked.
	for _, required := range []string{"name"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

// fillFromBox supplies every value the box already declares, so declaring a
// cluster asks only for what the box cannot know: its name and the stage it
// serves.
//
// Six flags were required that registry/box.yaml had already recorded -- the
// management cluster, the tenant, the repository URL, the platform version, the
// provider and the region. An operator had to open two files and copy them
// across, and a brief written to do that had eight flags on one command, any of
// which could disagree with the box and declare a cluster the box cannot build.
//
// Flags still win where given. This fills the unset ones, and reports what it
// filled, so a value that came from somewhere is never silent.
func fillFromBox(cmd *cobra.Command) error {
	box, err := tenant.ReadBoxDecl(workloadCluster.GitopsDir)
	if err != nil {
		return fmt.Errorf("this box's declaration could not be read, and it is where a "+
			"cluster's tenant, management cluster, repository and platform version come "+
			"from: %w", err)
	}

	filled := map[string]string{}
	// A flag the caller typed wins; one the box cannot answer is left as it is.
	// Keyed on Changed rather than on emptiness, so a flag whose default equals
	// the box's value is still reported as the box's -- and a default that
	// disagrees with the box, as `--provider hetzner` would on a hybrid box,
	// does not quietly outrank the declaration.
	set := func(flag string, target *string, from string) {
		if cmd.Flags().Changed(flag) || from == "" {
			return
		}
		*target = from
		filled[flag] = from
	}

	set("mgmt-cluster", &workloadCluster.MgmtCluster, box.ManagementCluster)
	set("tenant", &workloadCluster.TenantID, box.TenantID)
	set("provider", &workloadCluster.Provider, box.Provider)
	set("region", &workloadCluster.Region, box.Region)
	set("bundle-version", &workloadCluster.BundleVersion, box.BundleVersion)
	set("bundle-registry", &workloadCluster.BundleRegistry, box.BundleRegistry)
	set("platform-repo-url", &workloadCluster.PlatformRepoURL, box.PlatformRepoURL)
	if !cmd.Flags().Changed("gitops-repo-url") && box.GitOrg != "" && box.GitopsRepo != "" {
		workloadCluster.GitopsRepoURL = fmt.Sprintf("https://github.com/%s/%s", box.GitOrg, box.GitopsRepo)
		filled["gitops-repo-url"] = workloadCluster.GitopsRepoURL
	}

	for _, missing := range []struct {
		flag, value, why string
	}{
		{"name", workloadCluster.Name, "the cluster's name is yours to choose and permanent"},
		{"mgmt-cluster", workloadCluster.MgmtCluster, "registry/box.yaml declares no managementClusterName"},
		{"tenant", workloadCluster.TenantID, "registry/box.yaml declares no tenantId"},
		{"bundle-version", workloadCluster.BundleVersion, "registry/box.yaml declares no bundleVersion"},
		{"gitops-repo-url", workloadCluster.GitopsRepoURL, "registry/box.yaml declares no gitOrg and gitopsRepo"},
	} {
		if strings.TrimSpace(missing.value) == "" {
			return fmt.Errorf("--%s is required: %s", missing.flag, missing.why)
		}
	}

	if len(filled) > 0 {
		fmt.Println("Read from this box's declaration:")
		for _, flag := range []string{"mgmt-cluster", "tenant", "provider", "region",
			"bundle-version", "bundle-registry", "platform-repo-url", "gitops-repo-url"} {
			if v, ok := filled[flag]; ok {
				fmt.Printf("  --%-18s %s\n", flag, v)
			}
		}
		fmt.Println()
	}
	return nil
}

func runAddWorkloadCluster(cmd *cobra.Command, args []string) error {
	if err := fillFromBox(cmd); err != nil {
		return err
	}

	// Preflight 1: Check cluster name collision
	clusterDir := filepath.Join(workloadCluster.GitopsDir, "registry", "clusters", workloadCluster.Name)
	if _, err := os.Stat(clusterDir); err == nil {
		return fmt.Errorf("preflight check failed: cluster %q is already declared in %s",
			workloadCluster.Name, workloadCluster.GitopsDir)
	}

	// Preflight 2: Verify management cluster declaration exists
	coords, err := hubaccess.Declaration(workloadCluster.GitopsDir, workloadCluster.MgmtCluster)
	if err != nil {
		return fmt.Errorf("preflight check failed: reading management cluster declaration: %w", err)
	}

	// Preflight 3: Connect to escrow and management cluster
	access, err := hubaccess.Open(cmd.Context(), coords)
	if err != nil {
		return fmt.Errorf("preflight check failed: connecting to escrow and management cluster: %w", err)
	}

	id := hubaccess.Identity{BoxID: coords.BoxID, ClusterName: coords.ClusterName}

	// Preflight 4: Target provider configuration exists
	if workloadCluster.Provider != "hybrid" {
		if _, err := access.Provider(cmd.Context(), id, workloadCluster.Provider); err != nil {
			return fmt.Errorf("preflight check failed: provider credentials for %q absent in escrow: %w",
				workloadCluster.Provider, err)
		}
	}

	// Preflight 5: Management cluster API connectivity
	cfg, _, cleanup, err := access.Kubernetes(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("preflight check failed: connecting to management cluster API: %w", err)
	}
	defer cleanup()

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("preflight check failed: creating management cluster clientset: %w", err)
	}

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

	// Automatic Encryption Key & Configuration Provisioning:
	// Resolve/generate key, persist to escrow BEFORE writing cluster manifests,
	// and ensure Secret exists in platform-capi on management cluster.
	fmt.Printf("Ensuring encryption configuration for %q...\n", workloadCluster.Name)
	encOpts := encryption.EnsureOptions{
		BoxID:        coords.BoxID,
		ClusterName:  workloadCluster.Name,
		Namespace:    "platform-capi",
		EscrowClient: access.Escrow(),
		SecretClient: encryption.KubeSecretClient{Clientset: clientset},
	}
	encRes, err := encryption.EnsureClusterEncryption(cmd.Context(), encOpts)
	if err != nil {
		return fmt.Errorf("provisioning encryption configuration for %q: %w", workloadCluster.Name, err)
	}
	if encRes.KeyGenerated {
		fmt.Println("  ✓ encryption key generated and escrowed")
	} else {
		fmt.Println("  ✓ encryption key restored from escrow")
	}
	if encRes.SecretCreated {
		fmt.Printf("  ✓ secret platform-capi/%s created on management cluster\n", assets.EncryptionSecretName(workloadCluster.Name))
	} else if encRes.SecretAlreadyExisted {
		fmt.Printf("  ✓ secret platform-capi/%s verified on management cluster\n", assets.EncryptionSecretName(workloadCluster.Name))
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
// offer, and rendering a repository pinned to something that does not resolve
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

// addWorkload is what `kakt tenant add-workload` collects.
var addWorkload tenant.AppSpec

// newAddWorkloadCmd adds a deployable to an application that already exists.
//
// A separate command because add-app cannot do it: it refuses an application
// whose directory is present, which is correct -- an application is declared
// once, and its values.yaml carries secret declarations and hostnames a second
// declaration would overwrite. The documented way to add a workload later was to
// re-run add-app with more --workload flags, and that could only ever report
// "<app> already exists".
//
// It asks for nothing the repository already knows. The environment and the
// application name the directory; the cluster the workload runs on comes from
// which clusters serve that environment, the tenant from registry/box.yaml, and
// the chart version from the placeholder the template carries until the
// application's own build pins it.
func newAddWorkloadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-workload",
		Short: "Add a deployable to an application already declared",
		Long: `Add one or more workloads to an application that exists.

Writes environments/<env>/<app>/<workload>/Chart.yaml per workload, from the
template this repository keeps. Commit and push, and the management cluster
generates the workload.

A workload that already exists is left exactly as it is, including a chart
version a build has pinned: adding a second workload must not reset the first.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			written, err := tenant.AddWorkloads(addWorkload)
			if err != nil {
				return err
			}
			if len(written) == 0 {
				fmt.Println("Nothing to add.")
				return nil
			}
			fmt.Printf("✓ %d workload(s) added to %q in %s\n",
				len(written), addWorkload.AppID, addWorkload.Environment)
			for _, p := range written {
				fmt.Printf("  %s\n", p)
			}
			fmt.Println("\nThe chart version is a placeholder that cannot resolve, on purpose:")
			fmt.Println("an empty dependency list would render nothing and report healthy.")
			fmt.Println("The application's first build replaces it.")
			fmt.Println("\nCommit and push; the management cluster generates it.")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&addWorkload.GitopsDir, "gitops-dir", ".", "the tenant repository")
	f.StringVar(&addWorkload.Environment, "env", "", "the environment the application is declared in (required)")
	f.StringVar(&addWorkload.AppID, "app", "", "the application to add to (required)")
	f.StringSliceVar(&addWorkload.Workloads, "workload", nil, "a deployable to add; repeat for each")
	for _, required := range []string{"env", "app", "workload"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}
