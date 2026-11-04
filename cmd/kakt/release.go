package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// newReleaseCmd groups the platform team's release operations. Each runs the
// script that implements it -- scripts/release.sh, scripts/argocd-hard-sync.sh,
// scripts/release/release-data.py -- so there is one release path, and these
// commands only name its modes. The docs' release pages are written by
// `docs`, so the docs and the release check read one implementation.
//
// They run from the root of a kamarkat checkout, where those scripts and the
// packaging live. A box owner never runs them; their upgrade is a proposal to
// their own repository (kakt proposal).
func newReleaseCmd() *cobra.Command {
	var gitopsDir, owner, version string
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Publish a bundle version and promote a tenant to it (platform team)",
	}
	cmd.PersistentFlags().StringVar(&gitopsDir, "gitops-dir", "",
		"A tenant's GitOps checkout to promote and sync; without it a release only publishes")
	cmd.PersistentFlags().StringVar(&owner, "owner", "kamarkat", "GitHub organisation whose registry the bundle is published to")
	cmd.PersistentFlags().StringVar(&version, "version", "", "The version; default the newest published rc + 1")

	env := func(extra ...string) []string {
		return append([]string{"GITOPS_DIR=" + gitopsDir, "OWNER=" + owner}, extra...)
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

	var dryRun, noPromote, noSync bool
	publish := &cobra.Command{
		Use:   "publish",
		Short: "Publish the next version; with --gitops-dir, promote that tenant and verify its box reached it",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			args := withVersion()
			for flag, on := range map[string]bool{"--dry-run": dryRun, "--no-promote": noPromote, "--no-sync": noSync} {
				if on {
					args = append(args, flag)
				}
			}
			return runScript(env(), "scripts/release.sh", args...)
		},
	}
	publish.Flags().BoolVar(&dryRun, "dry-run", false, "Say what each step would do; change nothing")
	publish.Flags().BoolVar(&noPromote, "no-promote", false, "Publish only")
	publish.Flags().BoolVar(&noSync, "no-sync", false, "Publish and promote; do not sync the box")

	promote := &cobra.Command{
		Use:   "promote",
		Short: "Promote the tenant to a version already published (a stable release CI published)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if version == "" {
				return fmt.Errorf("name the published version to promote: --version")
			}
			if gitopsDir == "" {
				return fmt.Errorf("name the tenant checkout to promote: --gitops-dir")
			}
			args := withVersion("--promote-only")
			if dryRun {
				args = append(args, "--dry-run")
			}
			if noSync {
				args = append(args, "--no-sync")
			}
			return runScript(env(), "scripts/release.sh", args...)
		},
	}
	promote.Flags().BoolVar(&dryRun, "dry-run", false, "Say what each step would do; change nothing")
	promote.Flags().BoolVar(&noSync, "no-sync", false, "Promote; do not sync the box")

	sync := &cobra.Command{
		Use:   "sync",
		Short: "Sync the box to a promoted version and verify every Application reached it",
		Long: `Sync the box to a promoted version and verify every Application reached it.

For a release whose box did not converge: fix the Applications it named, then run
this again. Never re-publish. KUBECONFIG names the hub to drive.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if version == "" {
				return fmt.Errorf("name the version the box should reach: --version")
			}
			if os.Getenv("KUBECONFIG") == "" {
				return fmt.Errorf("set KUBECONFIG to the hub kubeconfig to drive")
			}
			return runScript(nil, "scripts/argocd-hard-sync.sh", "--promote", version)
		},
	}

	// A failed release prints the state it left; usage text would bury it.
	for _, c := range []*cobra.Command{check, publish, promote, sync, docs} {
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
