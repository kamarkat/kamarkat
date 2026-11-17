package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"github.com/kamarkat/kamarkat/internal/platform/embedded"
)

// The two scripts Day-0 runs. Named here so a release missing either is refused
// before the first of them starts, rather than part-way through a bootstrap.
const (
	dayZeroBootstrap = "scripts/hub-bootstrap.sh"
	dayZeroValidate  = "scripts/validate-all.sh"
)

// newBootstrapDayZeroCmd is the ONE entry point a tenant's CI calls.
//
// It replaces a reusable GitHub workflow that the tenant's repository pinned by
// ref. That pin was substituted when the box was rendered and never moved, so a
// box promoted to a later version still called the workflow of the version it
// was created on -- and a fix to Day-0 could not reach an existing box without
// a hand edit to its CI. The orchestration is the release's, so the release
// carries it: the launcher reads the version from git, fetches that binary, and
// runs this.
//
// IT DUPLICATES NOTHING. hub-bootstrap.sh keeps the phase state, the resume
// behaviour and the provisioning; validate-all.sh keeps the validation. This
// sequences them and reports where the run reached, which is what the workflow's
// steps did between the same two calls.
func newBootstrapDayZeroCmd() *cobra.Command {
	var gitopsDir, cluster string

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Run Day-0 for a cluster this repository declares (tenant CI)",
		Long: `Build the cluster a declaration names, from the release this binary is.

Noninteractive, and the whole of Day-0: the management cluster, its convergence,
the workload cluster the box declares, and the platform validation that decides
whether the box is usable. Re-running continues a run that stopped -- the phase
state lives in the workspace, not in this process.

The scripts come from this binary. Nothing is checked out, no path can be
supplied for them, and a file in the tenant's repository cannot stand in for one
of them: what runs is what the release contains.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if strings.TrimSpace(cluster) == "" {
				return fmt.Errorf("name the cluster to build: --cluster")
			}
			ws, err := filepath.Abs(gitopsDir)
			if err != nil {
				return err
			}
			decl, err := readClusterDeclaration(ws, cluster)
			if err != nil {
				return err
			}

			// THE DECLARATION AND THIS BINARY MUST AGREE.
			//
			// The launcher reads the pinned version from bundle.yaml and fetches
			// the matching release, so a disagreement here means the two reads
			// disagreed -- and the one that decides what runs is this binary.
			// Refused rather than reconciled: a box built by a version its
			// declaration does not name is a box nobody can reason about.
			if versions.IsReleaseBuild() && decl.Version != "" && decl.Version != versions.BundleVersion {
				return fmt.Errorf(
					"%s pins %s and this binary is %s.\n\n"+
						"The launcher fetches the version the declaration names, so these "+
						"differing means something supplied a different binary. Nothing was "+
						"started.", decl.Source, decl.Version, versions.BundleVersion)
			}

			platformDir, cleanup, err := dayZeroScripts()
			if err != nil {
				return err
			}
			defer cleanup()
			if err := embedded.RequireMaterialized(platformDir, dayZeroBootstrap, dayZeroValidate); err != nil {
				return err
			}

			// THIS BINARY IS THE ONE THE SCRIPT SHOULD USE.
			//
			// hub-bootstrap.sh takes the CLI from KAKT_BINARY when it is
			// executable, and otherwise builds one from cmd/kakt. Unset, a
			// released run found no source to build from and stopped with "no
			// usable kakt binary, and no source to build one" -- while the
			// binary it needed was the process running it.
			//
			// Resolved rather than assumed: a release is fetched to whatever
			// name the launcher gave it, so the path cannot be spelled here.
			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("cannot determine this binary's own path, which the "+
					"bootstrap needs as its CLI: %w", err)
			}
			env := append(os.Environ(),
				"ZERO_OPS_DIR="+ws,
				"CLUSTER_NAME="+cluster,
				"KAKT_BINARY="+self,
			)
			if versions.IsReleaseBuild() {
				// Platform content from the release and nowhere else
				// (hub-bootstrap.sh resolve_owned).
				env = append(env, embedded.ReleasedScriptsEnv+"=1")
			}

			// THE WORKLOAD CLUSTERS THIS REPOSITORY DECLARES, and how many there
			// are decides what the hub phase asserts.
			//
			// The hub's spoke gates assert about ONE named pool. With several
			// declared, naming one of them would report partial coverage as full,
			// so the hub phase takes none and every pool is checked below
			// instead. With none, the gates are skipped because there is nothing
			// to assert against -- that is read from the declarations, not from
			// an unset variable.
			pools, err := declaredSpokePools(ws)
			if err != nil {
				return err
			}

			args := []string{
				"--name", cluster,
				"--provider", decl.Provider,
				"--region", decl.Region,
				"--gitops-dir", ws,
				// Named, so the script's own check that the binary reports this
				// version has something to compare against. Unset it compares
				// nothing and a mismatched CLI passes.
				"--bundle-version", versions.BundleVersion,
			}
			switch len(pools) {
			case 0:
				// NOT A FAILURE. CI builds one declared cluster at a time, and a
				// management cluster is built on its own: a box's first bootstrap
				// legitimately has no workload cluster yet, because declaring one
				// is a later, separate commit (ADR-072).
				//
				// The hub's spoke gates have nothing to assert against, so they
				// are skipped -- and that is read from the declarations rather
				// than from an unset variable, which is the difference between
				// inapplicable and unproven.
				fmt.Println("[day0] no workload cluster declared; the hub's spoke gates are skipped")
			case 1:
				args = append(args, "--spoke", pools[0])
				fmt.Printf("[day0] workload cluster declared: %s\n", pools[0])
			default:
				fmt.Printf("[day0] workload clusters declared: %s\n", strings.Join(pools, " "))
				fmt.Println("[day0] more than one, so the hub's single-pool gates are skipped;")
				fmt.Println("[day0] each is brought up and checked after the hub converges")
			}
			// ON-PREM BELONGS TO A MANAGED CLUSTER SERVING DEV, not to this one.
			//
			// The published architecture puts on-premises workers inside the
			// managed-clusters block, dev only. A hub runs on its cloud workers.
			// Refused here, where the declaration that asked for it is open, so
			// the message names the file to edit rather than a flag nobody typed:
			// this wrapper is what turns onPrem.enabled into --on-prem.
			//
			// The hybrid cell still joins home-lab nodes to its own hub, because
			// ADR-046 left it no other worker capacity; it is retired by ADR-075's
			// exit condition and this exception goes with it.
			if decl.OnPrem && decl.Provider != "hybrid" {
				return fmt.Errorf("registry/clusters/%s/values.yaml declares onPrem.enabled on a "+
					"management cluster.\n\n"+
					"On-premises nodes are capacity for a MANAGED cluster serving dev -- your own\n"+
					"machines, joining that cluster over your tailnet. This cluster is the box's\n"+
					"management cluster and runs on its cloud workers.\n\n"+
					"Set onPrem.enabled to false here and declare the on-prem pool on the managed\n"+
					"cluster that serves dev.", cluster)
			}
			if decl.OnPrem {
				args = append(args, "--on-prem")
				if decl.Tailnet != "" {
					args = append(args, "--tailnet-name", decl.Tailnet)
				}
			}

			fmt.Printf("[day0] cluster %s, %s/%s, version %s\n",
				cluster, decl.Provider, decl.Region, versions.BundleVersion)
			fmt.Printf("[day0] workspace %s\n", ws)
			fmt.Printf("[day0] scripts   %s (from this release)\n", platformDir)

			if err := runDayZero(platformDir, dayZeroBootstrap, env, args...); err != nil {
				// WHERE IT REACHED, not just that it failed. The phase state and
				// the logs are in the workspace and survive this process, which
				// is what makes a re-run a continuation.
				return fmt.Errorf("%w\n\nThe run's state is in %s/.state and its logs beside it.\n"+
					"Re-running this command continues from the phase that failed; it does not "+
					"start again.", err, ws)
			}

			// CONVERGENCE, which is a different claim from validation.
			//
			// `kakt verify`'s checkers: nodes Ready, secret stores valid,
			// ExternalSecrets synced, Applications Synced and Healthy, pods
			// Running. Validation asks whether the platform's components are
			// correct; this asks whether the cluster has settled, and a box can
			// fail either while passing the other.
			//
			// Day-0's own working kubeconfig, not a break-glass read from the
			// escrow: the credential is already on this machine because the step
			// above put it there, and reaching into the escrow for a normal
			// verification would make recovery and routine operation the same act.
			kubeconfig, err := dayZeroKubeconfig(cluster)
			if err != nil {
				return err
			}
			fmt.Printf("\n[day0] waiting for %s to converge (up to %v)\n", cluster, convergeBudget)
			w := &health.HealthWaiter{
				Checkers: health.ConvergedCheckers(),
				Interval: 15 * time.Second,
				Timeout:  convergeBudget,
				OnCheckPass: func(c health.HealthChecker) {
					fmt.Printf("[day0] ✓ %s\n", c.Name())
				},
			}
			if err := w.Wait(c.Context(), kubeconfig); err != nil {
				return fmt.Errorf("%s did not converge within %v: %w\n\n"+
					"The cluster exists and what it is still waiting on is named above. "+
					"Its state is in %s/.state; re-running this command continues.",
					cluster, convergeBudget, err, ws)
			}
			fmt.Printf("[day0] ✓ %s converged\n", cluster)

			// EVERY DECLARED WORKLOAD CLUSTER, through the hub's own readiness
			// and home-worker checks rather than a second implementation of them.
			// Each is reported, and one failing does not hide another.
			var unready []string
			for _, pool := range pools {
				fmt.Printf("\n[day0] ── %s ──\n", pool)
				if err := spokeReady(platformDir, env, cluster, decl.Provider, pool); err != nil {
					fmt.Printf("[day0] %s did not come up: %v\n", pool, err)
					unready = append(unready, pool)
				}
			}
			if len(unready) > 0 {
				return fmt.Errorf("these workload clusters did not come up: %s",
					strings.Join(unready, " "))
			}

			// THE BOX AFTER CONVERGENCE AND AFTER EVERY DECLARED POOL.
			//
			// hub-bootstrap.sh runs this same validation as its own final fatal
			// step, and that run is NOT before the workload clusters exist: it
			// comes after step10_wait_spokepool, step10e_spoke_home_worker,
			// step10f_spoke_database and the tenant-ingress gate, for the pool it
			// was given.
			//
			// What this second run adds is what happened since: the convergence
			// wait above, and -- where several pools are declared, so the hub
			// phase asserted about none of them -- the readiness of every one.
			// That is a state the first run cannot have observed.
			//
			// IT REPLACES THE FIRST REPORT. Both write .state/validation.json, so
			// the authoritative record is this one, deliberately: it is the later
			// and wider observation. The first is not preserved alongside it.
			//
			// A named phase that cannot run fails rather than skipping, so this
			// cannot pass without having checked.
			if err := runDayZero(platformDir, dayZeroValidate, env, "--only=platform"); err != nil {
				return fmt.Errorf("the clusters were built and the box did not validate: %w\n\n"+
					"They exist. Fix what the validation named and re-run this command.", err)
			}

			fmt.Printf("\n[day0] %s is built and validated on %s\n", cluster, versions.BundleVersion)
			return nil
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "the cluster to build, as its declaration names it")
	cmd.Flags().StringVar(&gitopsDir, "gitops-dir", ".", "the box's repository; its declarations and state")
	return cmd
}

// dayZeroScripts is where the orchestration comes from, and the CLI decides.
//
// No flag and no environment variable selects it: an externally supplied path
// would let whoever sets it replace the released orchestration, which is the
// property this whole arrangement exists to establish. A development build uses
// the checkout it was built from (ADR-068); a release uses itself, with no
// fallback to a checkout if its own content is incomplete.
func dayZeroScripts() (string, func(), error) {
	if !versions.IsReleaseBuild() {
		wd, err := os.Getwd()
		if err != nil {
			return "", nil, err
		}
		if _, err := os.Stat(filepath.Join(wd, dayZeroBootstrap)); err != nil {
			return "", nil, fmt.Errorf(
				"this is a development build, which runs Day-0 from the checkout it was "+
					"built from, and %s has no %s.\n\nRun it from a kamarkat checkout, or use "+
					"a released binary.", wd, dayZeroBootstrap)
		}
		return wd, func() {}, nil
	}
	return embedded.MaterializeScripts()
}

// runDayZero runs one of the release's scripts with the workspace's environment.
func runDayZero(platformDir, rel string, env []string, args ...string) error {
	c := exec.Command("bash", append([]string{filepath.Join(platformDir, rel)}, args...)...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", rel, err)
	}
	return nil
}

// clusterDeclaration is what git says this cluster is.
type clusterDeclaration struct {
	Provider string
	Region   string
	OnPrem   bool
	Tailnet  string
	Version  string
	Source   string
}

// pinnedVersion matches the platform chart's targetRevision and only that.
//
// A cluster's bundle.yaml is a multi-source Application: one source is the
// platform chart, another is this repository, whose targetRevision names a
// BRANCH. Taking the first match would read "main" as a bundle version. So the
// match is anchored on the environment-manager chart line, the same anchoring
// `kakt promote` uses when it rewrites these files.
var pinnedVersion = regexp.MustCompile(`(?s)chart:\s*environment-manager.*?targetRevision:\s*(\S+)`)

func readClusterDeclaration(ws, cluster string) (clusterDeclaration, error) {
	var d clusterDeclaration
	dir := filepath.Join(ws, "registry", "clusters", cluster)
	valuesPath := filepath.Join(dir, "values.yaml")
	body, err := os.ReadFile(valuesPath)
	if err != nil {
		return d, fmt.Errorf("no declaration for cluster %q: %s is not readable.\n\n"+
			"A cluster is built from what this repository declares, so the declaration "+
			"has to be committed before Day-0 can run.", cluster, valuesPath)
	}
	var values struct {
		Provider string `yaml:"provider"`
		Region   string `yaml:"region"`
		OnPrem   struct {
			Enabled bool   `yaml:"enabled"`
			Tailnet string `yaml:"tailnet"`
		} `yaml:"onPrem"`
	}
	if err := yaml.Unmarshal(body, &values); err != nil {
		return d, fmt.Errorf("%s is not readable as a cluster declaration: %w", valuesPath, err)
	}
	d.Provider, d.Region = strings.TrimSpace(values.Provider), strings.TrimSpace(values.Region)
	d.OnPrem, d.Tailnet = values.OnPrem.Enabled, strings.TrimSpace(values.OnPrem.Tailnet)
	if d.Provider == "" || d.Region == "" {
		return d, fmt.Errorf("%s declares no provider or region, and a cluster cannot be "+
			"created without both", valuesPath)
	}

	bundlePath := filepath.Join(dir, "bundle.yaml")
	d.Source = bundlePath
	if b, err := os.ReadFile(bundlePath); err == nil {
		if m := pinnedVersion.FindSubmatch(b); m != nil {
			d.Version = strings.Trim(strings.TrimSpace(string(m[1])), `"'`)
		}
	}
	return d, nil
}

// convergeBudget is what convergence has actually taken: images pull, the secret
// operator retries on its own interval, and the GitOps engine's reconciliation is
// not instant. The same budget `kakt verify` defaults to, because it is the same
// question.
const convergeBudget = 15 * time.Minute

// dayZeroKubeconfig is the working copy the bootstrap just wrote.
//
// Not the escrowed one. During a bootstrap this is the only copy that exists --
// the escrow copy is written once the cluster is up -- and it is a file this CLI
// put there rather than a credential discovered in a checkout. Reaching into the
// escrow for a routine verification would make recovery and normal operation the
// same act.
func dayZeroKubeconfig(cluster string) (string, error) {
	home := os.Getenv("KAMARKAT_HOME")
	if strings.TrimSpace(home) == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("no home directory to find Day-0's kubeconfig in: %w", err)
		}
		home = filepath.Join(h, ".kamarkat")
	}
	p := filepath.Join(home, "run", "kubeconfig", cluster+".kubeconfig")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("the bootstrap left no kubeconfig at %s, so the cluster "+
			"cannot be verified.\n\nIt is written when the control plane comes up; a run "+
			"that reached here without one did not get that far.", p)
	}
	return p, nil
}

// declaredSpokePools are the workload clusters THIS REPOSITORY declares.
//
// Read from git, not from the cluster: the repository is the authority for which
// clusters a box has, and asking the cluster would report what it had managed to
// create rather than what it was asked to.
func declaredSpokePools(ws string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(ws,
		"registry", "clusters", "*", "infrastructure", "spokepool.yaml"))
	if err != nil {
		return nil, err
	}
	name := regexp.MustCompile(`(?m)^\s{2}name:\s*(\S+)`)
	seen := map[string]bool{}
	var pools []string
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(b), "kind: SpokePool") {
			continue
		}
		if g := name.FindSubmatch(b); g != nil {
			if p := string(g[1]); !seen[p] {
				seen[p] = true
				pools = append(pools, p)
			}
		}
	}
	sort.Strings(pools)
	return pools, nil
}

// spokeReady runs the hub's OWN readiness and home-worker checks for one pool.
//
// Sourced rather than reimplemented. These are two functions inside
// hub-bootstrap.sh and they encode what "up" means for a spoke -- a second
// implementation in Go would be a second answer to that, free to disagree with
// the one the bootstrap itself used.
func spokeReady(platformDir string, env []string, cluster, provider, pool string) error {
	const script = `set -euo pipefail
source "$DAY0_SCRIPT"
read_kubeconfig_from_state
export KUBECONFIG="$KUBECONFIG_PATH"
step10_wait_spokepool || exit 1
step10e_spoke_home_worker "$SPOKEPOOL_NAME"`

	c := exec.Command("bash", "-c", script)
	c.Env = append(env,
		"DAY0_SCRIPT="+filepath.Join(platformDir, dayZeroBootstrap),
		"CLUSTER_NAME="+cluster,
		"PROVIDER="+provider,
		"SPOKEPOOL_NAME="+pool,
	)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
