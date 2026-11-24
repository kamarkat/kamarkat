package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
)

// Promoting and syncing a box are the BOX OWNER'S acts, which is why they are
// top-level commands and not modes of `kakt release`.
//
// `kakt release` publishes the platform: it is for whoever maintains Kamarkat,
// and it ends when a version exists in the registry. Moving a box onto a version
// is a different act by a different party -- a rewrite of that box's own
// declarations, committed and pushed with that box owner's credentials -- and it
// needs no checkout of the platform at all (ADR-065).
//
// They were `kakt release promote` and `kakt release sync`, which put a tenant's
// operation inside the platform's release surface and read as something a
// release does to a box. Nothing about the implementation changed.
func newPromoteCmd() *cobra.Command {
	var gitopsDir, owner, version string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Move this box onto a published platform version",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if version == "" {
				return fmt.Errorf("name the published version to promote: --version")
			}
			// THE BOX YOU ARE STANDING IN, when you did not name one. Promotion
			// rewrites a box's own references, so the common case is being in it;
			// requiring the flag there made an operator name the directory they
			// were already inside. A directory holding registry/box.yaml is a
			// box and nothing else is, so there is nothing to guess.
			dir := gitopsDir
			if dir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				if _, err := os.Stat(filepath.Join(cwd, tenant.RegistryDir, tenant.BoxDeclFile)); err != nil {
					return fmt.Errorf(`name the box to promote: --gitops-dir

This directory is not a box: it has no %s. Run this from a box's checkout, or
name one.`, filepath.Join(tenant.RegistryDir, tenant.BoxDeclFile))
				}
				dir = cwd
			}
			// No platform checkout. Promotion is a rewrite of this box's own
			// references and a commit, so it runs wherever the box is -- which is
			// what lets a tenant promote without a copy of the platform (ADR-065).
			running, err := tenant.Promotion{
				GitopsDir: dir,
				Version:   version,
				Registry:  "ghcr.io/" + owner + "/charts",
				DryRun:    dryRun,
			}.Promote()
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Printf("dry run: %s would be promoted %s -> %s; nothing was written\n", dir, running, version)
				return nil
			}
			fmt.Printf("promoted %s -> %s and pushed. Your control plane reconciles the new version from git.\n", running, version)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "The published version to move this box to")
	cmd.Flags().StringVar(&gitopsDir, "gitops-dir", "",
		"The box to promote; defaults to the box you are standing in")
	cmd.Flags().StringVar(&owner, "owner", "kamarkat",
		"GitHub organisation whose registry the bundle is published to")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Say what would change; write nothing")
	cmd.SilenceUsage = true
	return cmd
}

// newSyncCmd OBSERVES a promotion. It does not drive one.
//
// It used to run `scripts/argocd-hard-sync.sh --promote`: a hard refresh of the
// root Application, then a submitted sync operation for each child, waited on in
// order. That made the CLI a second reconciliation engine beside ArgoCD, and put
// forced synchronisation in the documented release path -- step 5 of the release
// skill -- rather than in break-glass recovery where it belongs.
//
// Git declares the desired state, ArgoCD reconciles it, and this reports the
// result. The promotion commit is the whole trigger; a box that does not converge
// has a fault to read, not a sync to force.
//
// What made forcing look necessary was a liveness defect, now fixed at its cause:
// a sync wave after the workloads turned every workload's health into a gate on
// the operation finishing, ArgoCD's built-in workload health reports Progressing
// rather than Degraded while pods are not Ready, and the operation had no bound --
// so one resource that could not become Ready left an operation Running for ever,
// and ArgoCD will not start a new one while one is in flight. The late wave is
// gone from the spoke catalogue and controller.sync.timeout.seconds is set, so an
// operation now ends and the next revision reconciles by itself.
func newSyncCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Report whether the box has converged on a promoted version",
		Long: `Report whether every Application on this box reached a promoted version.

This OBSERVES; it syncs nothing. ArgoCD reconciles the promotion commit on its
own, and this waits for that to finish and names what has not converged.

For a box that does not converge: read the Applications it names and fix their
cause. Never re-publish, and do not force a sync -- a promotion that needs forcing
is a defect in the Application, not in the promotion. KUBECONFIG names the hub.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if version == "" {
				return fmt.Errorf("name the version the box should have reached: --version")
			}
			if os.Getenv("KUBECONFIG") == "" {
				return fmt.Errorf("set KUBECONFIG to the hub kubeconfig to read")
			}
			// --wait reports and waits. It issues no refresh and submits no
			// operation, which is the whole difference from what this ran before.
			return runScript(nil, "scripts/argocd-hard-sync.sh", "--wait-promote", version)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "The promoted version the box should have reached")
	cmd.SilenceUsage = true
	return cmd
}
