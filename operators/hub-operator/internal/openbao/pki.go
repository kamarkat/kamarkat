package openbao

import (
	"context"
	"fmt"

	"github.com/kamarkat/kamarkat/internal/pki"
)

// EnsurePKIRoles converges one pki_int role per platform certificate profile
// (internal/pki.RequiredProfiles). Each role's max_ttl is the profile's server-side
// cap: a request for longer is clamped by OpenBao, whatever the Certificate asks.
// Roles sign with the default issuer, which `kakt pki sign-intermediate` sets.
func (c *Client) EnsurePKIRoles(ctx context.Context) error {
	for _, p := range pki.RequiredProfiles {
		ttl := fmt.Sprintf("%dh", p.TTLDays*24)
		if err := c.PutPKIRole(ctx, p.Slug, map[string]any{
			"issuer_ref":        "default",
			"max_ttl":           ttl,
			"ttl":               ttl,
			"allow_any_name":    true,
			"enforce_hostnames": false,
			"allow_ip_sans":     true,
			"server_flag":       true,
			"client_flag":       true,
			"key_type":          "any",
			// Short-lived leaves are not stored: expiry, not revocation, ends them.
			"no_store": p.TTLDays < 30,
		}); err != nil {
			return fmt.Errorf("pki_int role %s: %w", p.Slug, err)
		}
	}
	return nil
}
