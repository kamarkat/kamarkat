package preflight

import (
	"context"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/errors"
)

// HetznerTokenValidator answers one question before a box is built: can this
// box's credential read this box's provider project?
type HetznerTokenValidator struct {
	Token string
}

// Validate reads something cheap and project-scoped.
//
// NOT Datacenter.List, which is what this used. Hetzner REMOVED
// GET /v1/datacenters after 1 October 2026 -- it answers 410 with
// `deprecated_api_endpoint` -- so from that date every bootstrap failed its
// preflight, and said "Invalid or expired Hetzner API token" about a token that
// was perfectly good. The credential was hunted for two runs; nothing had
// changed about it.
//
// SSH keys instead: project-scoped, so a token that can read the project passes
// and one scoped to another project does not, and on an endpoint with no
// announced deprecation. A global resource would have answered "this token is
// valid somewhere", which is not the question.
func (v *HetznerTokenValidator) Validate(ctx context.Context) error {
	client := hcloud.NewClient(hcloud.WithToken(v.Token))

	if _, _, err := client.SSHKey.List(ctx, hcloud.SSHKeyListOpts{}); err != nil {
		// WHAT THE API SAID, not what the caller assumed. Every failure here was
		// reported as a bad token -- a removed endpoint, a rate limit, an outage,
		// a DNS failure -- which is how a platform defect was diagnosed as the
		// tenant's credential. Only the two codes that actually mean "this token
		// is not accepted" produce that verdict now.
		if hcloud.IsError(err, hcloud.ErrorCodeUnauthorized) ||
			hcloud.IsError(err, hcloud.ErrorCodeForbidden) {
			return errors.InvalidHetznerToken(err)
		}
		return errors.ProviderAPIRefused("hetzner", err)
	}

	return nil
}
