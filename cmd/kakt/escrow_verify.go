package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/openbao"
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
		Short: "Report what the escrow actually holds for a cluster, and whether this box is bound to it",
		Long: `Check what the escrow holds, rather than what it should hold.

ADR-076 lists what a box cannot be rebuilt without. This reports which of those
are actually present, by fingerprint, so a missing one is visible before a
recovery rather than during one.

It also reports the box's BINDING -- the record, inside the escrow, that this
escrow is the one that box belongs to. Nothing can be escrowed or recovered
without it: the first privileged read of a bootstrap refuses an escrow that was
never bound. It is reported here because this is the command that answers what an
escrow holds, and because an unbound box has no other way to find out before
Day-0 tells it.

Values are never printed. A fingerprint is enough to compare one store against
another and useless to anyone who obtains it. The binding is the exception, and
is printed in full: it is a URL and a project id, which are names rather than
secrets, and seeing WHERE a box is bound is the point of reporting it.`,
		RunE: runEscrowVerify,
	}
	f := cmd.Flags()
	f.StringVar(&verifyCluster, "cluster", "", "cluster name the escrow is scoped to (required)")
	f.StringVar(&verifyBox, "box", "", "the box the escrow is keyed by; box-scoped artefacts live under it (required)")
	f.BoolVar(&isHub, "management", false, "this is the management cluster; all artefacts apply")
	f.BoolVar(&isWorkload, "workload", false, "this is a workload cluster; management-only artefacts do not apply")
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
		{escrow.ArtifactZitadelMasterkey, "the identity provider's key-encryption key", true},
		// OpenBao's seal record (every retained key generation and the delivery
		// history), keyed by the escrow project rather than the box id: written
		// before the box id exists (ADR-117 §3). Required, because without it the
		// store cannot be unsealed and no backup of it can be decrypted.
		// Checked below with the other project-keyed artefacts.
		// ADR-111 §10: the tenant's half of the management-state backups'
		// encryption. One per box, so the management cluster's row.
		{escrow.ArtifactBackupSSECKey, "the management-state backups' encryption key", true},
		// The OTHER half, and it was not listed here at all: this command
		// reported "all required entries present" for a box whose databases
		// could not archive a single WAL segment, because the credential that
		// reaches the bucket was absent and uncounted. Encryption and
		// authentication are different authorities (ADR-111 §10) and a successor
		// hub needs both, so a box missing either cannot restore -- which is the
		// definition this list is supposed to encode.
		//
		// Required, not advisory. Every management cluster runs CloudNativePG
		// clusters that archive, and a box that cannot archive has no recovery
		// point at all; saying so here is cheaper than finding out from the
		// acceptance run two days later.
		{escrow.ArtifactBackupStoreCredential, "the object-store credential the backups are written and read with", true},
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
		return fmt.Errorf("say which this cluster is: --management for the management cluster, " +
			"--workload for a workload cluster. The recovery contract differs (ADR-076), and " +
			"guessing it reports artefacts a cluster never had as missing")
	}

	missing := 0
	checked := 0

	// THE BOX'S BINDING, FIRST AND BOX-SCOPED.
	//
	// Not one of ADR-076's artefacts -- it is not recovery material -- but the
	// escrow cannot be used without it, so a report that omitted it said "all
	// artefacts present" about a box whose Day-0 would refuse on its first
	// privileged read:
	//
	//   escrow project … holds no binding for box … so it is not established as
	//   this box's escrow
	//
	// A box built from the quickstart reached that refusal having never been told
	// the step existed. Reported here because this is the command an operator is
	// pointed at to find out what their escrow holds, and because the answer is a
	// single read.
	//
	// Keyed by the box on both halves of the identity: the binding belongs to the
	// box, not to any one of its clusters, so the same line is correct whether
	// --management or --workload was passed.
	checked++
	bound, err := store.RestoreArtifact(ctx, escrow.ForCluster(verifyBox, verifyBox),
		escrow.ArtifactEscrowDestination)
	switch {
	case err != nil:
		fmt.Printf("  ERROR    %-24s %v\n", escrow.ArtifactEscrowDestination, err)
		missing++
	case bound == "":
		fmt.Printf("  MISSING  %-24s this escrow is not established as box %s's — nothing can be escrowed or recovered until it is\n",
			escrow.ArtifactEscrowDestination, verifyBox)
		fmt.Printf("           %-24s bind it as the platform administrator: kakt escrow bind-destination --gitops-dir . --cluster %s\n",
			"", verifyCluster)
		missing++
	default:
		// The recorded destination, printed in full rather than fingerprinted: it
		// is a URL and a project id, which are names and not secrets, and the
		// whole point is to see WHERE this box is bound.
		fmt.Printf("  present  %-24s bound to %s\n", escrow.ArtifactEscrowDestination, bound)
	}

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

	// OPENBAO'S PROJECT-KEYED ARTEFACTS (ADR-117 §3, §6). Written before the box id
	// exists, so keyed by the escrow project. The box's management cluster only.
	if isHub {
		for _, r := range []requirement{
			{openbao.ArtifactSeal, "OpenBao's seal-key generations and delivery history", true},
			{openbao.ArtifactRecoveryPGPPublic, "the public key OpenBao's recovery shares are encrypted to", true},
			{openbao.ArtifactRecoveryKeyEncrypted, "OpenBao's PGP-encrypted recovery key (break-glass)", true},
			{openbao.ArtifactRootCertificate, "the box's root CA certificate (trust anchor)", true},
			{openbao.ArtifactCustodyProjectID, "which project holds custody (root key, recovery PGP key)", true},
		} {
			checked++
			v, err := store.RestoreProjectArtifact(ctx, r.artifact)
			switch {
			case err != nil:
				fmt.Printf("  ERROR    %-24s %v\n", r.artifact, err)
				missing++
			case v == "":
				fmt.Printf("  MISSING  %-24s %s\n", r.artifact, r.what)
				missing++
			default:
				fmt.Printf("  present  %-24s %s  (%d bytes)\n", r.artifact, keyFingerprint([]byte(v)), len(v))
			}
		}
	}

	if missing > 0 {
		// Non-zero, so this is usable as a gate. A box missing a root secret from
		// the escrow is one incident from unrecoverable, and ADR-076 is explicit
		// that the gap cannot be closed after the cluster is gone.
		return fmt.Errorf("%d of %d required escrow entries absent for %q",
			missing, checked, verifyCluster)
	}
	fmt.Printf("\nAll %d required escrow entries for %q are present, this box's binding included.\n",
		checked, verifyCluster)
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
