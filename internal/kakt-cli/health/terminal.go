package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// TerminalError is a failure that waiting cannot fix.
//
// The waiter polls until its deadline because most failures are a resource that
// has not appeared yet. Some are not: an Application that ArgoCD considers
// settled and that manages no resources will never produce what is being waited
// for, and thirty minutes of polling ends in the same answer the first poll had.
// That happened twice here, an hour spent on a condition detectable in seconds.
type TerminalError struct {
	Reason string
}

func (e *TerminalError) Error() string { return e.Reason }

// IsTerminal reports whether waiting longer is pointless.
func IsTerminal(err error) bool {
	var t *TerminalError
	return errors.As(err, &t)
}

// ApplicationOwnsNothing reports why an Application will never create what is
// being waited for, or nil if it might still.
//
// The signature is ArgoCD's most misleading: Synced and Healthy with no tracked
// resources reads as success everywhere it is displayed, and means the
// Application rendered nothing. A chart whose values enable no component
// produces exactly that -- valid, empty, and reported as good.
//
// Returns nil whenever the answer is not certain: an Application that does not
// exist yet, one still syncing, or a cluster that cannot be read. Waiting is
// the right behaviour for all of those, and a guard that guesses would fail
// runs that were about to succeed.
func ApplicationOwnsNothing(ctx context.Context, kubeconfig, namespace, name string) error {
	out, err := runKubectl(ctx, []string{
		"--kubeconfig", kubeconfig,
		"get", "application", name, "-n", namespace,
		"-o", "json",
	})
	if err != nil {
		return nil
	}

	var app struct {
		Status struct {
			Sync struct {
				Status string `json:"status"`
			} `json:"sync"`
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
			Resources []struct{} `json:"resources"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &app) != nil {
		return nil
	}

	settled := app.Status.Sync.Status == "Synced" && app.Status.Health.Status == "Healthy"
	if settled && len(app.Status.Resources) == 0 {
		return &TerminalError{Reason: fmt.Sprintf(
			"Application %s/%s is Synced and Healthy but manages no resources, so "+
				"nothing will create what this check waits for. Its source renders "+
				"nothing -- for a distribution chart that usually means the "+
				"Application enables no component (ADR-063)",
			namespace, name)}
	}
	return nil
}
