package hubaccess

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/openbao"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/session"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// OpenCustody opens this box's custody project (ADR-117 §3, §6) as the signed-in
// box administrator, and the box escrow project beside it.
//
// ONLY a human session, only the administrator the box declares. A machine
// identity is refused outright, whatever KAKT_IDENTITY says: no automation
// identity is a member of the custody project, and a command that could reach it
// with one would be the CI custody identity the architecture rejects.
func OpenCustody(ctx context.Context, c Coordinates) (custody, box escrow.EscrowClient, who string, err error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("KAKT_IDENTITY")), "machine") {
		return nil, nil, "", fmt.Errorf("custody is reached only by the box's administrator, " +
			"never by a machine identity (ADR-117 §6). Unset KAKT_IDENTITY and sign in: kakt login")
	}
	s, err := session.Current(ctx, c.EscrowURL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%w\n\nCustody operations run as the box's administrator. Sign in first:\n\n  kakt login", err)
	}
	if err := authorizeAdministrator(c, s); err != nil {
		return nil, nil, "", err
	}
	box, err = escrow.NewEscrowClientForToken(ctx, c.EscrowURL, c.EscrowProjectID, s.Token)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open box %s's escrow as %s: %w", c.BoxID, s.Email, err)
	}
	id, err := box.RestoreProjectArtifact(ctx, openbao.ArtifactCustodyProjectID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read box %s's custody project record: %w", c.BoxID, err)
	}
	if strings.TrimSpace(id) == "" {
		return nil, nil, "", fmt.Errorf("box %s's escrow records no custody project. It is created by "+
			"`kakt escrow init`, run as the administrator", c.BoxID)
	}
	custody, err = escrow.NewEscrowClientForToken(ctx, c.EscrowURL, strings.TrimSpace(id), s.Token)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open box %s's custody project as %s: %w\n\nOnly the box's "+
			"administrator is a member of it", c.BoxID, s.Email, err)
	}
	return custody, box, s.Email, nil
}
