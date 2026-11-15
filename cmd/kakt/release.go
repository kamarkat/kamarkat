package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// newReleaseCmd groups the operations that PUBLISH the platform. Three, and only
// three: rehearse a version, publish it, write its documentation.
//
// Nothing here touches a box. Publishing used to promote a tenant and sync its
// cluster in the same command, which made the platform's release reach into a
// tenant's repository and push -- the opposite of what this platform promises,
// where a version reaches a box as a proposal its owner accepts. Moving a box
// onto a version is `kakt promote`, run by whoever owns that box.
//
// Each command runs the script that implements it -- scripts/release.sh,
// scripts/release/release-data.py -- so there is one release path and these
// commands only name its modes. They run from the root of a kamarkat checkout,
// where those scripts and the packaging live.
func newReleaseCmd() *cobra.Command {
	var owner, version string
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Publish a bundle version to the registry (platform maintainers)",
	}

	env := func(extra ...string) []string {
		return append([]string{"OWNER=" + owner}, extra...)
	}
	withVersion := func(args ...string) []string {
		if version != "" {
			args = append(args, "--version", version)
		}
		return args
	}

	check := &cobra.Command{
		Use:   "check",
		Short: "Rehearse the next version and check every component behind latest has a reason; pushes nothing",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runScript(env(), "scripts/release.sh", withVersion("--check")...)
		},
	}

	var docsDir string
	docs := &cobra.Command{
		Use:   "docs [version...]",
		Short: "Add published versions to the docs' release pages, and render every release page",
		Long: `Add published versions to the docs' release pages, and render every release page.

For each version named, writes its component inventory into the docs checkout:
each component against the newest release its project had published on the
version's release date, whether the version shipped it, and the catalog's reason
when it did not. A version behind with no reason fails, and nothing is written for
it. Then renders the release and line pages from every version the docs hold.
With no version, only renders.`,
		RunE: func(_ *cobra.Command, versions []string) error {
			if _, err := os.Stat(docsDir + "/docs.json"); err != nil {
				return fmt.Errorf("no docs checkout at %s: pass --docs-dir", docsDir)
			}
			for _, v := range versions {
				out := docsDir + "/data/releases/" + v + ".json"
				if err := runScript(nil, "scripts/release/release-data.py", "inventory", v, "--owner", owner, "--out", out); err != nil {
					return err
				}
			}
			return runScript(nil, "scripts/release/release-pages.py", docsDir)
		},
	}
	docs.Flags().StringVar(&docsDir, "docs-dir", "../kamarkat-docs", "The docs checkout to write the release pages into")

	var dryRun, stable bool
	publish := &cobra.Command{
		Use:   "publish",
		Short: "Publish the next version to the registry",
		Long: `Publish a bundle version to the registry, and nothing else.

No box is touched. A box moves onto a published version when its owner promotes
it (kakt promote), or by accepting the upgrade proposal the version raises.

Without --stable this publishes the next release CANDIDATE, packaging and
pushing it from this checkout.

With --stable it publishes a stable version the only way one is ever published:
by creating its GitHub release, which is what the publish workflow runs on. The
version must be named, because whether the next one is a patch, a minor or a
major says what the release means. The command follows the run and fails if it
does, so nothing is promoted to a version that has no charts.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			args := withVersion()
			// Ordered, not ranged over a map: the script takes these in any
			// order, but a stable ordering keeps `--dry-run` output comparable
			// between runs.
			for _, f := range []struct {
				flag string
				on   bool
			}{
				{"--stable", stable},
				{"--dry-run", dryRun},
			} {
				if f.on {
					args = append(args, f.flag)
				}
			}
			return runScript(env(), "scripts/release.sh", args...)
		},
	}
	publish.Flags().BoolVar(&stable, "stable", false,
		"Publish a stable version by creating its GitHub release; requires --version")
	publish.Flags().BoolVar(&dryRun, "dry-run", false, "Say what each step would do; change nothing")

	// Each flag goes on the subcommands that READ it, never on the group. A flag
	// that parses and has no effect is indistinguishable from one that is
	// broken, and it reads as a capability the command has.
	for _, c := range []*cobra.Command{check, docs, publish} {
		c.Flags().StringVar(&owner, "owner", "kamarkat",
			"GitHub organisation whose registry the bundle is published to")
	}
	// Not docs: it takes the versions it writes as arguments.
	for _, c := range []*cobra.Command{check, publish} {
		c.Flags().StringVar(&version, "version", "",
			"The version; default the newest published rc + 1")
	}

	// A failed release prints the state it left; usage text would bury it.
	for _, c := range []*cobra.Command{check, publish, docs} {
		c.SilenceUsage = true
		cmd.AddCommand(c)
	}
	return cmd
}

// runScript runs a kamarkat script from the checkout root, with its output and
// exit status as the command's own.
func runScript(env []string, script string, args ...string) error {
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("run this from the root of a kamarkat checkout: no %s here", script)
	}
	c := exec.Command(script, args...)
	c.Env = append(os.Environ(), env...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", script, err)
	}
	return nil
}
