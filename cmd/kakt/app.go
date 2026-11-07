package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/app"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/infisical"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

var (
	appKubeconfig string
	appRepoRoot   string
)

// newAppCmd groups the operations an application's owner performs on their own box.
//
// ADR-087 splits an application's runtime input by who owns the value: the platform
// generates what it owns, git carries what is not secret, and what remains is
// supplied here. An application DECLARES its secrets in its repository -- key,
// capability, consuming workloads -- and this is the only sanctioned way to put
// the values where that declaration says they will be.
//
// The alternative it replaces is a person with a shell, curl, and a workspace
// id they had to resolve first. That put values at the wrong path, under the
// wrong name, and in the wrong environment -- and because an ExternalSecret is
// atomic, one mistyped key withholds every other key from every workload
// reading it.
func newAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Manage an application's workload configuration and secrets",
		// A non-zero exit here reports the state of the box, not a
		// misinvocation. Printing usage after "9 secrets have not been
		// supplied" buries the finding under flag documentation.
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&appKubeconfig, "kubeconfig", "",
		"Path to this box's hub kubeconfig")
	cmd.PersistentFlags().StringVar(&appRepoRoot, "repo", ".",
		"Root of the tenant's GitOps repository (holds environments/<env>/values.yaml)")
	cmd.AddCommand(newAppSecretsCmd())
	cmd.AddCommand(newAppHostsCmd())
	return cmd
}

// newAppHostsCmd regenerates each spoke's public-hostname aggregate (ADR-096).
//
// The shared :443 Gateway has exactly one owner, and its listener list comes
// from this aggregate rather than from each app's own Application -- the
// arrangement that previously had two Applications overwriting one another's
// listener and flapping public DNS. Run it after changing any app's
// public.hosts; a preflight check asserts the file matches the declarations
// exactly, in both directions, so forgetting fails review rather than becoming
// a hostname that does not resolve.
func newAppHostsCmd() *cobra.Command {
	hosts := &cobra.Command{
		Use:          "hosts",
		Short:        "Public hostname aggregate for the shared TLS Gateway",
		SilenceUsage: true,
	}
	hosts.AddCommand(&cobra.Command{
		Use:          "aggregate",
		Short:        "Regenerate registry/clusters/<spoke>/generated/values/public-hosts.yaml",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			written, err := tenant.AggregatePublicHosts(appRepoRoot)
			if err != nil {
				return err
			}
			for _, spoke := range written {
				fmt.Printf("[app-hosts] ✓ %s/%s\n", spoke, tenant.PublicHostsFile)
			}
			fmt.Printf("[app-hosts] %d spoke(s) updated; commit the result\n", len(written))
			return nil
		},
	})
	return hosts
}

func newAppSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Supply and audit the secrets an application declares",
		Long: `An application declares its secrets in environments/<env>/values.yaml -- the key,
what capability it serves, and which workloads read it. The VALUES are never in
that repository in any form.

This is where they are supplied, and where an operator can see which declared
secrets are still absent without any value being disclosed.`,
	}
	cmd.AddCommand(newAppSecretsTemplateCmd(), newAppSecretsStatusCmd(),
		newAppSecretsSetCmd(), newAppSecretsImportCmd())
	return cmd
}

// withInfisical runs fn against this box's own Infisical.
//
// The port-forward is how the CLI reaches an instance that is not published,
// and INFISICAL_API_URL is set by EnsureAPIAccess rather than defaulted: a
// default pointed every box at the platform's own instance, which is a
// different tenant's secret store (ADR-065).
func withInfisical(ctx context.Context, fn func(*infisical.Client, *infisical.Config) error) error {
	if appKubeconfig == "" {
		return fmt.Errorf("--kubeconfig is required: the credential for this box's Infisical " +
			"is held in the box, and is the same one ESO uses")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", appKubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	return infisical.RunWithPortForward(ctx,
		"platform-security", "infisical-standalone-infisical", "8081", "8080",
		appKubeconfig, 1,
		func() error {
			client, err := infisical.NewClient(ctx, clientset)
			if err != nil {
				return fmt.Errorf("connect to this box's Infisical: %w", err)
			}
			icfg, err := infisical.GetInfisicalConfig(ctx, clientset)
			if err != nil {
				return fmt.Errorf("read this box's Infisical configuration: %w", err)
			}
			return fn(client, icfg)
		})
}

func newAppSecretsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status <environment> <app>",
		// Read on the command that returns the error, not on its parent.
		SilenceUsage: true,
		Short:        "Report which declared secrets are present, by name",
		Long: `Join what the application declares against what has been supplied.

Prints key NAMES and never values. A declared secret that is absent is reported
with the capability it serves, because a key name alone is not something an
operator can act on -- and with the workloads it takes down, because an
ExternalSecret is atomic and one absent key withholds every other key in it.

Exits non-zero when anything declared is absent, so a pipeline can gate on it.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := app.Load(appRepoRoot, args[0], args[1])
			if err != nil {
				return err
			}
			if len(f.Secrets) == 0 {
				fmt.Printf("%s / %s declares no secrets.\n", f.TenantID, args[0])
				return nil
			}

			var statuses []app.Status
			err = withInfisical(cmd.Context(), func(c *infisical.Client, icfg *infisical.Config) error {
				// args[0], not icfg.EnvironmentSlug: an application's secrets live in
				// the environment the application runs in, and icfg carries the
				// HUB's. A hub manages clusters whose environments differ from its
				// own, and the spoke reads with its own slug
				// (03-platform-services-appset.yaml patches its ClusterSecretStore),
				// so reading the hub's environment here reports a stg app's secrets
				// as absent while they are present where the app looks.
				// ONE REPORT PER DEPLOYMENT. The application runs on every cluster
				// serving this environment and each reads its own cluster's folder,
				// so "is this secret supplied" has an answer per cluster. Reporting
				// one would have reported whichever cluster happened to be first.
				clusters, cErr := app.ClustersServing(".", args[0])
				if cErr != nil {
					return cErr
				}
				if len(clusters) == 0 {
					return fmt.Errorf("no workload cluster serves environment %q, so this "+
						"application is deployed nowhere and has no secret folder yet.\n\n"+
						"Add one: kakt tenant add-cluster --environment %s", args[0], args[0])
				}
				for _, cluster := range clusters {
					present, lErr := c.ListSecretNames(cmd.Context(), icfg.ProjectSlug, args[0], f.SecretPathOn(cluster))
					if lErr != nil {
						return lErr
					}
					statuses = append(statuses, app.Join(*f, cluster, present))
				}
				return nil
			})
			if err != nil {
				return err
			}

			// Printed per deployment, and the failure counts them: a secret
			// supplied on one cluster and absent on another is the state this is
			// for, and a single summary would have hidden exactly that.
			missingBy := map[string][]string{}
			for _, status := range statuses {
				printStatus(status, args[0])
				for _, m := range status.Missing() {
					missingBy[status.Path] = append(missingBy[status.Path], m.Name)
				}
			}
			if len(missingBy) > 0 {
				var where []string
				total := 0
				for path, names := range missingBy {
					sort.Strings(names)
					where = append(where, fmt.Sprintf("%s: %s", path, strings.Join(names, ", ")))
					total += len(names)
				}
				sort.Strings(where)
				return fmt.Errorf("%d declared secret(s) have not been supplied across %d "+
					"deployment(s).\n\nSupply each with:\n  kakt app secrets set %s <KEY>\n\n%s",
					total, len(missingBy), args[0], strings.Join(where, "\n"))
			}
			return nil
		},
	}
}

func printStatus(s app.Status, environment string) {
	fmt.Printf("\n%s / %s\n%s\n\n", s.App.TenantID, environment, s.Path)

	for _, e := range s.Entries {
		switch {
		case e.Orphan:
			// NOT a fault, and not labelled as one. The platform writes
			// OAUTH_*, CACHE_PASSWORD, db-credentials and the owner's initial
			// password to this same folder, and none belongs in an application's
			// `secrets:` -- an application cannot supply what the issuer mints. The
			// line exists so a key left behind by a rename is visible, which
			// means it cannot distinguish that from the normal case; saying
			// "orphan" would call six expected keys a problem.
			fmt.Printf("  %-9s %-34s %s\n", "other", e.Name, "not declared here (platform-written keys live here too)")
		case e.Present:
			fmt.Printf("  %-9s %-34s %s\n", "present", e.Name, e.Capability)
		default:
			fmt.Printf("  %-9s %-34s %s\n", "ABSENT", e.Name, e.Capability)
		}
	}

	if withheld := s.WithheldWorkloads(); len(withheld) > 0 {
		names := make([]string, 0, len(withheld))
		for w := range withheld {
			names = append(names, w)
		}
		sort.Strings(names)

		fmt.Printf("\n  Consequence -- an ExternalSecret is atomic, so an absent key withholds\n")
		fmt.Printf("  every other key in the same object:\n\n")
		for _, w := range names {
			fmt.Printf("    %s-%s-secrets delivers nothing: %d key(s), so %s cannot start\n",
				s.App.TenantID, w, withheld[w], w)
		}
	}
	fmt.Println()
}

func newAppSecretsSetCmd() *cobra.Command {
	var fromStdin bool
	cmd := &cobra.Command{
		Use:          "set <environment> <app> <KEY>",
		SilenceUsage: true,
		Short:        "Supply one declared secret",
		Long: `Write one value to this application's secret path.

The value is read from the terminal with echo disabled, or from stdin with
--stdin so a password manager can pipe into it. There is deliberately no
--value flag: a secret passed as an argument is in the shell's history and in
the process table for anyone on the machine.

One key per invocation, and no batch form. A batch means a file, and a file
means secrets at rest on a laptop.

Re-running is how a secret is rotated. The value is replaced, ESO delivers it
within its refresh interval, and the workload's reloader restarts on the change.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment, appID, key := args[0], args[1], args[2]
			f, err := app.Load(appRepoRoot, environment, appID)
			if err != nil {
				return err
			}

			// Refused, not written. The declaration is the contract: a value at
			// a path nothing declares is delivered to no workload, and would sit
			// there looking supplied.
			decl, ok := f.Declared(key)
			if !ok {
				return fmt.Errorf("%s/%s declares no secret named %q.\n\n"+
					"A value written for an undeclared key is delivered to nothing, because the\n"+
					"ExternalSecrets are rendered from the declaration. Add it to\n"+
					"environments/%s/values.yaml under `secrets:` -- with the capability it\n"+
					"serves and the workloads that read it -- and run this again.",
					f.TenantID, environment, key, environment)
			}

			value, err := readSecretValue(cmd, fromStdin, key, decl.Capability)
			if err != nil {
				return err
			}
			if value == "" {
				return fmt.Errorf("no value given for %s; nothing was written", key)
			}

			// WRITTEN ONCE PER CLUSTER, because each deployment reads its own
			// cluster's folder and the store on a managed cluster is scoped to
			// /spoke-pool/<that cluster>/. A single write would have supplied the
			// secret to exactly one of the deployments and left the others
			// reporting it absent.
			clusters, err := app.ClustersServing(".", environment)
			if err != nil {
				return err
			}
			if len(clusters) == 0 {
				return fmt.Errorf("no workload cluster serves environment %q, so there is "+
					"nowhere for this value to be read from.\n\n"+
					"Add one: kakt tenant add-cluster --environment %s", environment, environment)
			}
			if err := withInfisical(cmd.Context(), func(c *infisical.Client, icfg *infisical.Config) error {
				for _, cluster := range clusters {
					// The environment named on the command line, not the hub's: see
					// the note in `app secrets status`.
					if wErr := c.CreateOrUpdateSecret(cmd.Context(),
						icfg.ProjectSlug, environment, f.SecretPathOn(cluster), key, value); wErr != nil {
						return fmt.Errorf("write %s for %s: %w", key, cluster, wErr)
					}
				}
				return nil
			}); err != nil {
				return err
			}

			// The NAME, and never the value.
			fmt.Printf("\n  wrote %s to %d deployment(s): %s\n", key, len(clusters), strings.Join(clusters, ", "))
			fmt.Printf("  read by: %s\n\n", strings.Join(decl.Workloads, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&fromStdin, "stdin", false,
		"Read the value from stdin instead of prompting, for piping from a password manager")
	return cmd
}

// readSecretValue obtains one value without it appearing anywhere it persists.
func readSecretValue(cmd *cobra.Command, fromStdin bool, key, capability string) (string, error) {
	if fromStdin {
		raw, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read value from stdin: %w", err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal, so the value cannot be read with echo " +
			"disabled. Pass --stdin to pipe it in deliberately")
	}

	fmt.Printf("\n%s\n  %s\n\nValue (not echoed): ", key, capability)
	raw, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read value: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func newAppSecretsImportCmd() *cobra.Command {
	var fromEnv string
	var confirm bool
	cmd := &cobra.Command{
		Use:          "import <environment> <app> --from-env <file>",
		SilenceUsage: true,
		Short:        "Migrate an application whose secrets predate ADR-087",
		Long: `Read a KEY=VALUE file once and write every DECLARED key it carries.

This exists to migrate an application that predates ADR-087, whose values were held in
a seed file. It is not how secrets are supplied afterwards: the file it reads is
an input to the migration, not a location where secrets live, and should be
deleted once this has run.

Without --confirm it lists the key names it would write and writes nothing.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment, appID := args[0], args[1]
			if fromEnv == "" {
				return fmt.Errorf("--from-env is required: import reads a file, and there is no default one")
			}
			f, err := app.Load(appRepoRoot, environment, appID)
			if err != nil {
				return err
			}
			values, err := readEnvFile(fromEnv)
			if err != nil {
				return err
			}

			// Only DECLARED keys. A seed file from the retired fleet-registry arrangement carries
			// configuration and platform-owned material alongside the secrets --
			// waypoint's carried twenty-seven keys of which ten were secrets --
			// and writing all of it would put configuration back in the secret
			// store this ADR moved it out of.
			//
			// And only keys that HAVE a value. `template` emits a skeleton of
			// empty entries, so a file straight from it -- or one half filled
			// in -- would otherwise write empty strings over secrets that are
			// already supplied and working. An empty entry means "not filled in
			// yet", which is the normal state of a skeleton and must never be
			// mistaken for "set this to nothing".
			var willWrite, skipped, empty []string
			for k, v := range values {
				_, declared := f.Declared(k)
				switch {
				case !declared:
					skipped = append(skipped, k)
				case strings.TrimSpace(v) == "":
					empty = append(empty, k)
				default:
					willWrite = append(willWrite, k)
				}
			}
			sort.Strings(willWrite)
			sort.Strings(skipped)
			sort.Strings(empty)

			// Resolved before anything is written, so the preview names the
			// deployments this will reach rather than one folder.
			importClusters, err := app.ClustersServing(".", environment)
			if err != nil {
				return err
			}
			if len(importClusters) == 0 {
				return fmt.Errorf("no workload cluster serves environment %q, so there is "+
					"nowhere for these values to be read from.\n\n"+
					"Add one: kakt tenant add-cluster --environment %s", environment, environment)
			}
			fmt.Printf("\n%s / %s\n%d deployment(s): %s\n\n",
				f.TenantID, environment, len(importClusters), strings.Join(importClusters, ", "))
			for _, k := range willWrite {
				fmt.Printf("  write  %s\n", k)
			}
			for _, k := range empty {
				fmt.Printf("  empty  %-34s declared, but no value in the file\n", k)
			}
			for _, k := range skipped {
				fmt.Printf("  skip   %-34s not declared by this app\n", k)
			}
			if len(willWrite) == 0 {
				if len(empty) > 0 {
					return fmt.Errorf("\nnothing to import: every declared key in %s is empty.\n\n"+
						"That is what `template` produces -- fill in the values and run this again.\n"+
						"An empty entry is never written: it would replace a secret that is already\n"+
						"supplied with nothing, and the workload reading it would start and fail.",
						fromEnv)
				}
				return fmt.Errorf("\nnothing to import: %s carries no key this application declares", fromEnv)
			}

			if !confirm {
				fmt.Printf("\nNothing was written. Re-run with --confirm to write the %d key(s) above.\n\n", len(willWrite))
				return nil
			}

			if err := withInfisical(cmd.Context(), func(c *infisical.Client, icfg *infisical.Config) error {
				for _, k := range willWrite {
					// Once per deployment: each cluster's store reads only its own
					// /spoke-pool/<cluster>/ folder.
					for _, cluster := range importClusters {
						// The named environment, not the hub's: see `app secrets status`.
						if err := c.CreateOrUpdateSecret(cmd.Context(),
							icfg.ProjectSlug, environment, f.SecretPathOn(cluster), k, values[k]); err != nil {
							return fmt.Errorf("write %s for %s: %w", k, cluster, err)
						}
					}
					fmt.Printf("  wrote  %s to %d deployment(s)\n", k, len(importClusters))
				}
				return nil
			}); err != nil {
				return err
			}

			fmt.Printf("\n  %d key(s) written. Delete %s: its values are now held where the\n"+
				"  application's declaration says they are, and a second copy on this machine is\n"+
				"  a copy nothing rotates.\n\n", len(willWrite), fromEnv)
			return nil
		},
	}
	cmd.Flags().StringVar(&fromEnv, "from-env", "", "KEY=VALUE file to migrate from")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Actually write; without it, list the key names and stop")
	return cmd
}

// readEnvFile parses KEY=VALUE lines. Values are not expanded or word-split: a
// token containing $ or spaces must survive verbatim.
func readEnvFile(path string) (map[string]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer fh.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(fh)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.TrimPrefix(v, `"`)
		v = strings.TrimSuffix(v, `"`)
		v = strings.TrimPrefix(v, `'`)
		v = strings.TrimSuffix(v, `'`)
		out[strings.TrimSpace(k)] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

// newAppSecretsTemplateCmd emits a .env skeleton containing exactly the keys
// the application declares.
//
// The alternative is a tenant hand-writing that file from the declaration,
// which is transcription: a typo produces a key the import skips silently,
// because a key the application does not declare is indistinguishable from a key the
// tenant chose not to supply. Generating it removes the transcription step
// entirely -- the skeleton cannot disagree with the declaration because it is
// derived from it.
//
// Emits NAMES and capabilities, never values. Writing a skeleton that already
// carried values would be writing secrets to a file on behalf of someone who
// did not ask for it.
func newAppSecretsTemplateCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "template <environment> <app>",
		Short:        "Print a .env skeleton holding exactly the keys this application declares",
		SilenceUsage: true,
		Long: `Generate the file that ` + "`import`" + ` reads, with one empty entry per declared
secret and the capability each serves as its comment.

It carries no values, so it is committed as environments/<env>/<app>/.env.example:
scripts/seed-secrets.sh copies it to .env for the values, and refuses an example
that no longer matches this output.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := app.Load(appRepoRoot, args[0], args[1])
			if err != nil {
				return err
			}
			if len(f.Secrets) == 0 {
				return fmt.Errorf("%s/%s declares no secrets, so there is nothing to supply",
					f.TenantID, args[0])
			}

			out := cmd.OutOrStdout()
			env, app := args[0], args[1]
			fmt.Fprintf(out, "# Secrets for %s / %s / %s -- generated from environments/%s/%s/values.yaml\n",
				f.TenantID, env, app, env, app)
			fmt.Fprintf(out, "# by `kakt app secrets template %s %s`. No values here, ever.\n", env, app)
			fmt.Fprintf(out, "#\n")
			fmt.Fprintf(out, "# To supply them, from the repository root:\n")
			fmt.Fprintf(out, "#\n")
			fmt.Fprintf(out, "#     ./scripts/seed-secrets.sh %s %s             copies this to .env beside it\n", app, env)
			fmt.Fprintf(out, "#     (fill in the values in .env)\n")
			fmt.Fprintf(out, "#     ./scripts/seed-secrets.sh %s %s             shows what would be written\n", app, env)
			fmt.Fprintf(out, "#     ./scripts/seed-secrets.sh %s %s --confirm   writes it\n", app, env)
			fmt.Fprintf(out, "#\n")
			fmt.Fprintf(out, "# Then delete .env: once imported, the values live where the declaration\n")
			fmt.Fprintf(out, "# says they do, and a copy there is one nothing rotates. An empty entry is\n")
			fmt.Fprintf(out, "# never written, and a key this app does not declare is ignored.\n")

			for _, d := range f.Secrets {
				fmt.Fprintf(out, "\n# %s\n", d.Capability)
				fmt.Fprintf(out, "#   read by: %s\n", strings.Join(d.Workloads, ", "))
				fmt.Fprintf(out, "%s=\n", d.Name)
			}
			return nil
		},
	}
}
