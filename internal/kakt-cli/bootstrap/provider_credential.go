package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// enrollProviderCredential puts the credential this bootstrap was given into the
// box's escrow, BEFORE any cloud infrastructure exists.
//
// PART OF THE RECOVERY CONTRACT, NOT TELEMETRY. Destroying or repairing a box's
// infrastructure is a provider API call, and the credential for it lived only in
// HCLOUD_TOKEN or k8-secrets/hetzner/token on the machine that ran the
// bootstrap. So the one workstation able to delete the servers was the one that
// created them, and a teardown from anywhere else deleted nothing while
// reporting success. A box whose credential is not escrowed is a box that cannot
// be recovered from another machine, which is the thing ADR-076 exists to
// prevent -- so a failure here fails the bootstrap rather than being printed and
// stepped over.
//
// Ordering is the point: enrolment happens before the servers are created, since
// afterwards a failed bootstrap has left infrastructure whose credential is
// nowhere, and the in-cluster copy dies with the cluster it is in.
func enrollProviderCredential(ctx context.Context, store escrow.EscrowClient, boxID, clusterName, provider, token string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	token = strings.TrimSpace(token)

	switch {
	case store == nil:
		return fmt.Errorf("no escrow to enrol the %s credential into, and a box is not built "+
			"without one (ADR-076): the credential that destroys this box's infrastructure "+
			"would exist only on this machine", provider)
	case boxID == "":
		return fmt.Errorf("the box id is required before anything is escrowed: artefacts are " +
			"keyed by the logical hub (ADR-111 §1)")
	case provider == "":
		return fmt.Errorf("the provider is required: one artefact per provider, so a box that " +
			"buys from two does not have the second overwrite the first")
	case token == "":
		return fmt.Errorf("no %s credential to enrol, and this box is built with one: without "+
			"it in the escrow, only this machine could ever destroy the infrastructure it is "+
			"about to create", provider)
	}

	// BOX-SCOPED, not cluster-scoped, and the difference is hub succession.
	//
	// The credential buys and destroys the box's infrastructure; it belongs to the
	// box, which outlives any cluster in it (ADR-111 §1). Keyed per cluster, a
	// successor hub would write a SECOND copy under its own name instead of
	// finding the one already there -- and `kakt teardown`, resolving the box from
	// the declaration, would read whichever of them its cluster name happened to
	// match. The object-store credential is escrowed the same way for the same
	// reason (enroll-store-credential uses the box on both halves).
	//
	// Cluster-scoped artefacts stay per cluster: the admin kubeconfig is one
	// cluster's and a successor's is its own.
	id := escrow.ForCluster(boxID, boxID)
	artifact := escrow.ProviderCredentialArtifact(provider)

	existing, err := store.RestoreArtifact(ctx, id, artifact)
	if err != nil {
		// Unreadable is not absent, and guessing either way is wrong: writing over
		// a credential that may be there replaces one a teardown depends on, and
		// continuing without writing leaves the box unrecoverable.
		return fmt.Errorf("could not read the escrowed %s credential for box %s: %w",
			provider, boxID, err)
	}

	switch {
	case existing == token:
		// Idempotent: re-running a bootstrap against the same box writes nothing.
		fmt.Printf("[cluster-provision] the %s credential for box %s is already escrowed\n",
			provider, boxID)
		return nil
	case existing != "":
		// Refused, never replaced implicitly. The escrowed copy may be the only one
		// that can destroy infrastructure this box already has, and a bootstrap run
		// with a different token in the environment is not evidence that replacing
		// it is intended.
		return fmt.Errorf("box %s already has a DIFFERENT %s credential escrowed.\n\n"+
			"Refusing to replace it: the escrowed copy may be the only one that can reach "+
			"the infrastructure this box already owns.\n\n"+
			"If replacing it is what you intend, do it deliberately:\n\n"+
			"  kakt escrow enroll-provider-credential --gitops-dir . --cluster %s --rotate < token\n\n"+
			"then revoke the old credential at the provider", boxID, provider, clusterName)
	}

	if err := store.BackupArtifact(ctx, id, artifact, token); err != nil {
		return fmt.Errorf("could not escrow the %s credential for box %s: %w\n\n"+
			"Nothing has been provisioned. A box whose provider credential is not escrowed "+
			"can only be destroyed from this machine", provider, boxID, err)
	}

	// Proved readable through the identity a later command will use, rather than
	// assumed from a write that returned no error. The canonical identity is the
	// box's, which is what `kakt teardown` and `kakt kubeconfig` resolve from the
	// declaration -- so this is the same lookup, performed now.
	readBack, err := store.RestoreArtifact(ctx, id, artifact)
	if err != nil {
		return fmt.Errorf("the %s credential was written but cannot be read back for box %s: %w",
			provider, boxID, err)
	}
	if readBack != token {
		return fmt.Errorf("the escrow returned a different %s credential than was written for "+
			"box %s, so nothing escrowed here can be relied on", provider, boxID)
	}

	fmt.Printf("[cluster-provision] %s credential escrowed for box %s\n", provider, boxID)
	return nil
}
