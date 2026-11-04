package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/proposal"
	"github.com/spf13/cobra"
)

// kakt proposal preflight — the verdict ADR-067 requires a proposal to carry.
//
// Run by the tenant's own CI on the pull request the platform's version bump
// opened, inside the tenant's box, against the cluster the bump targets. The
// platform cannot run it: it holds no access to the cluster and under ADR-065
// never will. So the judgement is made where the cluster is and only the
// verdict travels.
func newProposalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proposal",
		Short: "Judge a bundle proposal against the cluster it targets",
	}
	cmd.AddCommand(newProposalPreflightCmd())
	return cmd
}

func newProposalPreflightCmd() *cobra.Command {
	var (
		o          proposal.Options
		outPath    string
		jsonPath   string
		failOnUnv  bool
		clusterArg string
	)

	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Produce the pre-flight verdict for a proposed bundle version",
		Long: "Checks the proposed version against this cluster: that the release's\n" +
			"declared minimum permits the step, that the version is actually published\n" +
			"where this box pulls from, and that the cluster is reconciling now.\n\n" +
			"A check that cannot run returns unverified rather than passing. A verdict\n" +
			"that degrades to \"fine\" when it cannot see anything is worse than none,\n" +
			"because it is indistinguishable from one that looked.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.Candidate == "" {
				return fmt.Errorf("--candidate is required: there is no proposal to judge")
			}
			o.Cluster = clusterArg
			if o.Kubeconfig == "" && o.Cluster != "" {
				o.Kubeconfig = filepath.Join("k8-secrets", "kubeconfig", o.Cluster+".kubeconfig")
			}

			v := proposal.Run(cmd.Context(), o)
			md := v.Markdown()

			if outPath != "" {
				if err := os.WriteFile(outPath, []byte(md), 0o644); err != nil {
					return err
				}
			} else {
				fmt.Print(md)
			}
			if jsonPath != "" {
				b, err := json.MarshalIndent(v, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(jsonPath, append(b, '\n'), 0o644); err != nil {
					return err
				}
			}
			fmt.Fprintln(os.Stderr, "[preflight] "+v.Summary())

			// A failing verdict exits non-zero so the tenant's own branch
			// protection can act on it. Unverified does not, by default: it is
			// the absence of a judgement, and blocking every merge on a check
			// that could not reach the cluster would make the tenant delete the
			// check rather than fix it. --fail-on-unverified is for a tenant who
			// wants the stricter rule, which is theirs to choose (ADR-065).
			switch v.Outcome {
			case proposal.Fail:
				return fmt.Errorf("pre-flight failed: this cluster should not take %s", o.Candidate)
			case proposal.Unverified:
				if failOnUnv {
					return fmt.Errorf("pre-flight could not verify %s against this cluster", o.Candidate)
				}
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.Candidate, "candidate", "", "the bundle version being proposed (required)")
	f.StringVar(&clusterArg, "cluster", "", "the cluster the proposal targets")
	f.StringVar(&o.Kubeconfig, "kubeconfig", "", "kubeconfig for that cluster (default: k8-secrets/kubeconfig/<cluster>.kubeconfig)")
	f.StringVar(&o.GitopsDir, "gitops-dir", ".", "the tenant repository the proposal is against")
	f.StringVar(&o.Registry, "registry", "", "registry the candidate would be pulled from, no scheme. Empty skips that check rather than verifying a source this box does not use")
	f.StringVar(&o.MinimumFrom, "minimum-from", "", "the release's declared minimum version this may be taken from")
	f.StringVar(&outPath, "out", "", "write the markdown verdict here instead of stdout")
	f.StringVar(&jsonPath, "json", "", "also write the verdict as JSON here")
	f.BoolVar(&failOnUnv, "fail-on-unverified", false, "exit non-zero when a check could not run")
	return cmd
}
