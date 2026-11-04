package health

import (
	"context"
	"fmt"
	"os/exec"
)

// runKubectlExec is the actual exec.CommandContext wrapper, kept in its
// own file so the kubectl.go file can stay focused on the checker contract.
func runKubectlExec(ctx context.Context, args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("runKubectl: no args")
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, string(out))
	}
	return out, nil
}
