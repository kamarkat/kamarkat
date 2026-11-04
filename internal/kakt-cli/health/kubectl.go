package health

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// KubectlChecker is a base helper for checkers that shell out to `kubectl`.
// It centralizes the kubectl invocation pattern so concrete checkers can
// focus on the JSONPath or resource name they care about.
//
// Checkers that need typed API access should use ClientsetChecker (see
// clientset.go) instead.
type KubectlChecker struct {
	// checkName is the human-readable identifier for this check. The
	// Name() method returns it; the field is unexported so the public
	// contract is always the method.
	checkName string
	// KubectlArgs is the slice of arguments to pass to `kubectl` AFTER
	// `--kubeconfig <kubeconfig>`. For example:
	//   []string{"get", "crd", "clusters.cluster.x-k8s.io"}
	// The kubeconfig is prepended automatically by Check.
	KubectlArgs []string
	// Expected is a substring that must appear in the command output for
	// the check to pass. If empty, any non-empty output passes.
	Expected string
	// FailIfError controls whether a non-zero kubectl exit code is a
	// hard failure (true) or just an empty-result failure (false).
	// Most checks should leave this at the default (true).
	FailIfError bool
	// TimeoutHint, if non-zero, is included in the error message.
	TimeoutHint time.Duration
}

// Name returns the checker's identifier.
func (k *KubectlChecker) Name() string { return k.checkName }

// NewKubectlChecker constructs a KubectlChecker with the given name and
// kubectl args. Prefer this constructor to direct struct literals so the
// unexported checkName field stays private.
func NewKubectlChecker(name string, args []string) *KubectlChecker {
	return &KubectlChecker{checkName: name, KubectlArgs: args}
}

// Check executes the kubectl command and evaluates the result.
func (k *KubectlChecker) Check(ctx context.Context, kubeconfig string) error {
	args := append([]string{"--kubeconfig", kubeconfig}, k.KubectlArgs...)
	out, err := runKubectl(ctx, args)
	if err != nil {
		if k.FailIfError {
			return fmt.Errorf("%s: kubectl error: %w", k.checkName, err)
		}
		// Treat kubectl error as "not ready" so the waiter retries.
		return fmt.Errorf("%s: not yet ready (%v)", k.checkName, err)
	}
	if len(out) == 0 {
		return fmt.Errorf("%s: no output", k.checkName)
	}
	if k.Expected != "" && !strings.Contains(string(out), k.Expected) {
		return fmt.Errorf("%s: %q not found in output", k.checkName, k.Expected)
	}
	return nil
}

// runKubectl shells out to `kubectl` with the given args and returns stdout.
// Errors include stderr in the message for debuggability.
func runKubectl(ctx context.Context, args []string) ([]byte, error) {
	// We import exec only at the package level to keep this file focused.
	return runKubectlExec(ctx, args)
}
