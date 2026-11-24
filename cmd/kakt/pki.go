package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/openbao"
)

func newPKICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pki",
		Short: "Administer this box's certificate authority (ADR-117)",
	}
	cmd.AddCommand(newPKISignIntermediateCmd())
	return cmd
}

func newPKISignIntermediateCmd() *cobra.Command {
	var gitopsDir, cluster string
	var renew bool
	var validity time.Duration
	cmd := &cobra.Command{
		Use:   "sign-intermediate",
		Short: "Sign the box's issuing intermediate CA with its custody-held root",
		Long: `Sign this box's issuing intermediate certificate authority.

The intermediate's private key is generated inside the box's OpenBao and never
leaves it. This command asks OpenBao for a certificate signing request over that
key, validates it -- the box's identity, the request's subject and extensions, and
that the existing trust hierarchy is this box's -- signs it with the root CA held
in the box's administrator-only custody project, and returns only the signed
certificate chain to OpenBao.

Run it as the box's administrator, after pushing the management cluster's
declaration: it waits for OpenBao to be ready, so it can be started right away.
Day-0 waits for it, bounded, and fails with "administrator signing pending" rather
than report a platform whose certificates cannot issue.

Idempotent: once the intermediate is signed by this box's root and not near
expiry, it changes nothing. --renew issues a new intermediate over a new key for a
scheduled renewal (ADR-035 dual-trust rollover).

The root is read only under your own session; it never enters CI or the cluster.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			coords, err := hubaccess.Declaration(gitopsDir, cluster)
			if err != nil {
				return err
			}
			custody, _, who, err := hubaccess.OpenCustody(ctx, coords)
			if err != nil {
				return err
			}
			raw, err := custody.RestoreProjectArtifact(ctx, openbao.ArtifactRootCA)
			if err != nil {
				return fmt.Errorf("read the root from custody: %w", err)
			}
			if strings.TrimSpace(raw) == "" {
				return fmt.Errorf("box %s's custody holds no root CA; run `kakt escrow init` as the administrator", coords.BoxID)
			}
			var root openbao.RootCA
			if err := json.Unmarshal([]byte(raw), &root); err != nil {
				return fmt.Errorf("the custody root is malformed: %w", err)
			}

			access, err := hubaccess.Open(ctx, coords)
			if err != nil {
				return err
			}
			cfg, kubeconfig, cleanup, err := access.Kubernetes(ctx,
				hubaccess.Identity{BoxID: coords.BoxID, ClusterName: coords.ClusterName})
			if err != nil {
				return err
			}
			defer cleanup()
			cs, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return err
			}
			fmt.Printf("Signing box %s's issuing intermediate as %s.\n", coords.BoxID, who)
			return openbao.SignIntermediateOnBox(ctx, cs, openbao.SignOptions{
				// The root is bound to the box's escrow project (kakt escrow init).
				BoxID:      coords.EscrowProjectID,
				Kubeconfig: kubeconfig,
				Root:       root,
				Validity:   validity,
				Renew:      renew,
				Now:        time.Now(),
				Out:        os.Stdout,
			})
		},
	}
	cmd.Flags().StringVar(&gitopsDir, "gitops-dir", ".", "the box's repository")
	cmd.Flags().StringVar(&cluster, "cluster", "", "the management cluster, when the repository declares more than one")
	cmd.Flags().BoolVar(&renew, "renew", false, "issue a new intermediate over a new key (scheduled renewal)")
	cmd.Flags().DurationVar(&validity, "validity", openbao.DefaultIntermediateValidity,
		"the intermediate's validity; always bounded by the root's remaining validity")
	return cmd
}
