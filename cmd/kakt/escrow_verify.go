package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	"github.com/spf13/cobra"
)

var (
	verifyCluster string
	verifyBox     string
	isHub         bool
	isWorkload    bool
)

// newEscrowVerifyCmd reports what the escrow ACTUALLY holds for a cluster.
//
// # WHY THIS IS NOT PREFLIGHT 91
//
// Preflight 91 checks that ADR-076's list and the escrow implementation name the
// same artefacts. That is a static check and it passed while the escrow held
// nothing for two of them, because names agreeing says nothing about presence.
//
// A recovery runs from what is in the escrow, not from what the ADR says should be.
// So presence is checked against the escrow itself, which needs credentials and
// therefore cannot live in a build-time gate.
//
// It prints FINGERPRINTS, never values. The point is to answer "is it there, and is
// it the same thing the box is using" without putting a root secret in a terminal
// or a CI log.
func newEscrowVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Report which escrow artefacts actually exist for a cluster",
		Long: `Check what the escrow holds, rather than what it should hold.

ADR-076 lists what a box cannot be rebuilt without. This reports which of those
are actually present, by fingerprint, so a missing one is visible before a
recovery rather than during one.

Values are never printed. A fingerprint is enough to compare one store against
another and useless to anyone who obtains it.`,
		RunE: runEscrowVerify,
	}
	f := cmd.Flags()
	f.StringVar(&verifyCluster, "cluster", "", "cluster name the escrow is scoped to (required)")
	f.StringVar(&verifyBox, "box", "", "the logical hub the escrow is keyed by; box-scoped artefacts live under it (required)")
	f.BoolVar(&isHub, "hub", false, "this is the management cluster; all artefacts apply")
	f.BoolVar(&isWorkload, "workload", false, "this is a workload cluster; hub-only artefacts do not apply")
	_ = cmd.MarkFlagRequired("cluster")
	_ = cmd.MarkFlagRequired("box")
	return cmd
}

func runEscrowVerify(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return fmt.Errorf("no escrow to verify: %w", err)
	}

	// ADR-076's recovery contract, INCLUDING SCOPE.
	//
	// Scope is not decoration. The first version of this command checked all four
	// artefacts against every cluster and reported `infisical-master-keys` and
	// `zitadel-masterkey` as MISSING for a workload cluster -- which is correct by
	// the contract: there is one secret store and one identity provider, both on the
	// management cluster, so a workload cluster is not meant to hold them. The tool
	// manufactured two findings and buried the one real gap among them.
	//
	// A verification that reports artefacts a cluster was never supposed to have is
	// worse than no verification: it trains the reader to skim past MISSING.
	type requirement struct {
		artifact string
		what     string
		hubOnly  bool
	}
	required := []requirement{
		{escrow.ArtifactKubeconfig, "the cluster's administrative recovery credential", false},
		{escrow.ArtifactSecretEncryptionKey, "the API server's at-rest encryption key", false},
		{"infisical-master-keys", "the secret store's encryption key and auth secret", true},
		{escrow.ArtifactZitadelMasterkey, "the identity provider's key-encryption key", true},
		// ADR-111 §10: the tenant's half of the management-state backups'
		// encryption. One per box, so the management cluster's row.
		{escrow.ArtifactBackupSSECKey, "the management-state backups' encryption key", true},
	}

	// ADR-111 §2: the trust roots a replacement hub cannot do without and cannot
	// re-create. Required of EVERY cluster, the management cluster included -- it is
	// CAPI-managed like any other, and a box that can no longer issue a node
	// certificate for its own control plane is in the same position as one that
	// cannot for a workload cluster.
	for _, purpose := range escrow.SpokePKIPurposes {
		required = append(required, requirement{
			escrow.SpokePKIArtifact(purpose),
			"the cluster's " + purpose + " certificate authority",
			false,
		})
	}

	// Whether this cluster is the management cluster decides which rows apply. Taken
	// from the caller rather than guessed from the name, because a name convention is
	// not a contract and a wrong guess here reports a real gap as not applicable.
	if !isHub && !isWorkload {
		return fmt.Errorf("say which this cluster is: --hub for the management cluster, " +
			"--workload for a workload cluster. The recovery contract differs (ADR-076), and " +
			"guessing it reports artefacts a cluster never had as missing")
	}

	missing := 0
	checked := 0
	for _, r := range required {
		if r.hubOnly && !isHub {
			fmt.Printf("  n/a      %-24s %s — held by the management cluster only (ADR-076)\n", r.artifact, r.what)
			continue
		}
		checked++
		v, err := store.RestoreArtifact(ctx, escrow.ForCluster(verifyBox, verifyCluster), r.artifact)
		switch {
		case err != nil:
			fmt.Printf("  ERROR    %-24s %v\n", r.artifact, err)
			missing++
		case v == "":
			fmt.Printf("  MISSING  %-24s %s — this cluster cannot be rebuilt without it\n", r.artifact, r.what)
			missing++
		default:
			// keyFingerprint reduces base64, hex and raw to the same digest, so this
			// is comparable with what `kakt encryption rotate` prints for the live
			// configuration. It was sha256 of the stored string, which for an
			// encryption key is the HEX encoding -- and the configuration holds BASE64,
			// so the two never matched and the comparison this command tells you to make
			// could not be made.
			fmt.Printf("  present  %-24s %s  (%d bytes)\n", r.artifact, keyFingerprint([]byte(v)), len(v))
		}
	}

	if missing > 0 {
		// Non-zero, so this is usable as a gate. A box missing a root secret from
		// the escrow is one incident from unrecoverable, and ADR-076 is explicit
		// that the gap cannot be closed after the cluster is gone.
		return fmt.Errorf("%d of %d required escrow artefact(s) absent for %q",
			missing, checked, verifyCluster)
	}
	fmt.Printf("\nAll %d artefacts required for %q are present.\n", checked, verifyCluster)
	fmt.Println("Presence is not provenance: compare a fingerprint against the value the box")
	fmt.Println("is actually using before relying on it (ADR-076).")
	return nil
}

// escrowFingerprintOf is used by `kakt encryption` to compare a live value against
// the escrow without either being printed.
func escrowFingerprintOf(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(absent)"
	}
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

var _ = os.Getenv
