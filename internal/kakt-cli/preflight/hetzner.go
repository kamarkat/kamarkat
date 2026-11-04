package preflight

import (
	"context"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/errors"
)

// HetznerTokenValidator validates Hetzner API token
type HetznerTokenValidator struct {
	Token string
}

func (v *HetznerTokenValidator) Validate(ctx context.Context) error {
	client := hcloud.NewClient(hcloud.WithToken(v.Token))
	
	// Use read-only operation to validate token
	_, _, err := client.Datacenter.List(ctx, hcloud.DatacenterListOpts{})
	if err != nil {
		return errors.InvalidHetznerToken(err)
	}
	
	return nil
}
