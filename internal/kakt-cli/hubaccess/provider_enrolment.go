package hubaccess

import (
	"context"
	"fmt"
	"strings"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// Enrolment is what a call to EnrollProviderCredential turned out to be.
//
// Returned rather than printed, because the two callers report differently: box
// init folds it into the box's creation output, and `kakt escrow
// enroll-provider-credential` is a command whose only output this is.
type Enrolment int

const (
	// EnrolledFirst: the escrow held none and now holds this one.
	EnrolledFirst Enrolment = iota
	// EnrolledUnchanged: the escrow already held exactly this credential.
	EnrolledUnchanged
	// EnrolledRotated: a different credential was replaced, which the caller
	// had to ask for.
	EnrolledRotated
)

// EnrollProviderCredential records a box's provider credential in its escrow.
//
// THE ONE WAY A PROVIDER CREDENTIAL ENTERS A BOX. Every lifecycle command reads
// it back from here -- bootstrap, teardown, inventory -- because the credential
// that buys and destroys a box's infrastructure must be the one the box was
// built with, and a token that happened to be in an environment answers
// perfectly about somebody else's provider project.
//
// WHY THIS IS SHARED. It had two implementations. One was this logic inside
// `kakt escrow enroll-provider-credential`; the other was
// bootstrap/provider_credential.go, called from the cloud provisioner with
// os.Getenv("HCLOUD_TOKEN") just before the servers were created. That second
// one could not work: resolveHCloudToken runs earlier in the same command and
// overwrites HCLOUD_TOKEN with the value it read FROM the escrow, so the
// enrolment could only ever write back what it had just read -- and on a box
// whose escrow was empty, the read refused before the enrolment was reached. It
// was a write that could not write, and it is gone. This is the only one.
//
// It ESTABLISHES the binding, via OpenEstablishing: enrolment is a box's first
// privileged write, performed by an operator who knows which project is this
// box's, so an unbound project is bound in the same authorized act. A project
// already bound elsewhere is still refused.
func EnrollProviderCredential(ctx context.Context, c Coordinates, provider, token string, rotate bool) (Enrolment, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	token = strings.TrimSpace(token)
	switch {
	case provider == "":
		return EnrolledFirst, fmt.Errorf("the provider is required: one artefact per provider, " +
			"so a box that buys capacity from two does not have the second overwrite the first")
	case token == "":
		return EnrolledFirst, fmt.Errorf("no %s credential to enrol, and this box is built with "+
			"one: without it in the escrow, only this machine could ever destroy the "+
			"infrastructure it creates", provider)
	case strings.TrimSpace(c.BoxID) == "":
		return EnrolledFirst, fmt.Errorf("the box id is required before anything is escrowed: " +
			"artefacts are keyed by the logical box (ADR-111 §1)")
	}

	access, err := OpenEstablishing(ctx, c)
	if err != nil {
		return EnrolledFirst, err
	}

	// The box on both halves of the identity. The credential is the box's, not
	// any one cluster's, so a successor hub finds the one already escrowed
	// instead of writing a second under its own name (ADR-111 §1).
	id := Identity{BoxID: c.BoxID, ClusterName: c.BoxID}
	artifact := escrow.ProviderCredentialArtifact(provider)

	existing, err := access.Artifact(ctx, id, artifact)
	if err != nil {
		// Unreadable is not absent. Enrolling over a credential that may be there
		// is how the one a teardown depends on gets lost.
		return EnrolledFirst, fmt.Errorf("reading the escrowed %s credential for box %s: %w",
			provider, c.BoxID, err)
	}
	switch {
	case existing == token:
		return EnrolledUnchanged, nil
	case existing != "" && !rotate:
		return EnrolledUnchanged, fmt.Errorf("box %s already has an escrowed %s, and this is a "+
			"different one.\n\nRefusing to replace it: pass --rotate if that is what you intend, "+
			"and revoke the old credential at the provider afterwards", c.BoxID, artifact)
	}

	if err := access.PutArtifact(ctx, id, artifact, token); err != nil {
		return EnrolledFirst, fmt.Errorf("escrow the %s credential for box %s: %w",
			provider, c.BoxID, err)
	}
	if existing != "" {
		return EnrolledRotated, nil
	}
	return EnrolledFirst, nil
}
