package hubaccess

import (
	"context"
	"fmt"
	"strings"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// EnrollBoxCredential escrows one of the credentials a box is built with.
//
// The same five rules EnrollProviderCredential established, which is why this is
// beside it rather than inside each caller:
//
//   - The box on both halves of the identity. These belong to the box, not to any
//     one management cluster, so a successor finds what is already escrowed
//     instead of writing a second copy under its own name (ADR-111 §1).
//   - It ESTABLISHES the binding, through OpenEstablishing. Enrolment is a box's
//     first privileged write, performed by an operator who knows which project is
//     this box's, so an unbound project is bound in that same authorized act --
//     and a project already bound elsewhere is still refused. Nothing instructs
//     an operator to bind separately.
//   - Unreadable is not absent. A read that fails refuses, because enrolling over
//     a credential that may be there is how the one a teardown depends on is lost.
//   - The same bytes are UNCHANGED, not a rotation. A re-run of `box init` must
//     not report every credential rotated.
//   - Different bytes are refused without --rotate, so an ordinary re-run can
//     never replace a credential by accident.
//
// Returns what it did, so a caller can report "escrowed" and "already escrowed"
// differently -- an operator re-running init should see which of the two happened.
func EnrollBoxCredential(ctx context.Context, c Coordinates, artifact, payload string, rotate bool) (Enrolment, error) {
	artifact = strings.TrimSpace(artifact)
	payload = strings.TrimSpace(payload)
	switch {
	case artifact == "":
		return EnrolledFirst, fmt.Errorf("the artefact name is required: it is what the escrow " +
			"keys this credential by, and a box cannot read back what was written under no name")
	case payload == "":
		// Not an error. A box that declines a capability has nothing to enrol for
		// it, and the capability's own absence is reported where it matters -- at
		// Day-0, by the thing that needed it.
		return EnrolledUnchanged, nil
	case strings.TrimSpace(c.BoxID) == "":
		return EnrolledFirst, fmt.Errorf("the box id is required before anything is escrowed: " +
			"artefacts are keyed by the logical box (ADR-111 §1)")
	}

	if _, declared := escrow.DeclaredScope(artifact); !declared {
		// A scope that is merely DEFAULTED places a security-critical artefact by
		// accident, which is the hazard escrow.DeclaredScope exists to expose.
		return EnrolledFirst, fmt.Errorf("%s has no declared escrow scope, so where it would be "+
			"written is an accident of the default; declare it in escrow.go beside the others",
			artifact)
	}

	access, err := OpenEstablishing(ctx, c)
	if err != nil {
		return EnrolledFirst, err
	}
	id := Identity{BoxID: c.BoxID, ClusterName: c.BoxID}

	existing, err := access.Artifact(ctx, id, artifact)
	if err != nil {
		return EnrolledFirst, fmt.Errorf("reading the escrowed %s for box %s: %w",
			artifact, c.BoxID, err)
	}
	switch {
	case strings.TrimSpace(existing) == payload:
		return EnrolledUnchanged, nil
	case strings.TrimSpace(existing) != "" && !rotate:
		return EnrolledUnchanged, fmt.Errorf("box %s already has an escrowed %s, and this is a "+
			"different one.\n\nRefusing to replace it: pass --rotate if that is what you intend, "+
			"and revoke the old credential at its source afterwards", c.BoxID, artifact)
	}

	if err := access.PutArtifact(ctx, id, artifact, payload); err != nil {
		return EnrolledFirst, fmt.Errorf("escrow %s for box %s: %w", artifact, c.BoxID, err)
	}
	if strings.TrimSpace(existing) != "" {
		return EnrolledRotated, nil
	}
	return EnrolledFirst, nil
}

// BoxCredential reads one of those credentials back.
//
// Day-0 reads these, and reads them from here rather than from its environment:
// the values are the box's, they are needed before the box has a secret store of
// its own, and the escrow is the only store that answers then.
//
// An absent artefact is "" and no error. The capability it serves reports its own
// absence, in its own terms, where it is needed -- which is a better message than
// this function could write.
func BoxCredential(ctx context.Context, access Access, boxID, artifact string) (string, error) {
	if strings.TrimSpace(boxID) == "" {
		return "", fmt.Errorf("the box id is required to read %s: artefacts are keyed by the "+
			"logical box (ADR-111 §1)", artifact)
	}
	id := Identity{BoxID: boxID, ClusterName: boxID}
	payload, err := access.Artifact(ctx, id, artifact)
	if err != nil {
		return "", fmt.Errorf("reading the escrowed %s for box %s: %w", artifact, boxID, err)
	}
	return strings.TrimSpace(payload), nil
}
