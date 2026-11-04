package preflight

import (
	"context"
	"fmt"

	"github.com/hetznercloud/hcloud-go/hcloud"
)

// SSHKeyValidator validates SSH key exists in Hetzner
type SSHKeyValidator struct {
	Token  string
	KeyName string
}

func (v *SSHKeyValidator) Validate(ctx context.Context) error {
	if v.KeyName == "" {
		return nil // Optional
	}
	
	client := hcloud.NewClient(hcloud.WithToken(v.Token))
	
	_, _, err := client.SSHKey.GetByName(ctx, v.KeyName)
	if err != nil {
		return fmt.Errorf("SSH key '%s' not found in HCloud. Please upload it first", v.KeyName)
	}
	
	return nil
}
