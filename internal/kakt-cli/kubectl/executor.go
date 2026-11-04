package kubectl

import (
	"context"
	"fmt"
	"os/exec"
)

// Executor wraps kubectl commands with optional debug logging
type Executor struct {
	Debug bool
}

// Run executes a kubectl command with optional debug output
func (e *Executor) Run(ctx context.Context, args ...string) error {
	if e.Debug {
		fmt.Printf("[DEBUG] kubectl %s\n", args)
	}
	
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.CombinedOutput()
	
	if e.Debug && len(output) > 0 {
		fmt.Printf("[DEBUG] Output: %s\n", string(output))
	}
	
	if err != nil && e.Debug {
		fmt.Printf("[DEBUG] Error: %v\n", err)
	}
	
	return err
}

// Output executes a kubectl command and returns output
func (e *Executor) Output(ctx context.Context, args ...string) ([]byte, error) {
	if e.Debug {
		fmt.Printf("[DEBUG] kubectl %s\n", args)
	}
	
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.CombinedOutput()
	
	if e.Debug {
		if len(output) > 0 {
			fmt.Printf("[DEBUG] Output: %s\n", string(output))
		}
		if err != nil {
			fmt.Printf("[DEBUG] Error: %v\n", err)
		}
	}
	
	return output, err
}
