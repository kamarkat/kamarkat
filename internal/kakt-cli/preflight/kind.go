package preflight

import (
	"context"
	"os/exec"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/errors"
)

// KindValidator checks if Kind is available
type KindValidator struct {
	SkipIfBootstrapContext bool
}

func (v *KindValidator) Validate(ctx context.Context) error {
	if v.SkipIfBootstrapContext {
		return nil
	}
	
	cmd := exec.CommandContext(ctx, "kind", "version")
	if err := cmd.Run(); err != nil {
		return errors.KindNotFound(err)
	}
	return nil
}
