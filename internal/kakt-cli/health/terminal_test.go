package health

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsTerminalOnlyMatchesATerminalError(t *testing.T) {
	if IsTerminal(errors.New("not yet ready")) {
		t.Error("an ordinary not-ready error must keep the waiter waiting")
	}
	if !IsTerminal(&TerminalError{Reason: "manages nothing"}) {
		t.Error("a terminal error must stop the wait")
	}
	// Checkers wrap errors on the way out, so matching must survive wrapping --
	// otherwise the guard silently stops working the first time a caller adds
	// context to the error.
	if !IsTerminal(fmt.Errorf("CNPG cluster infisical-db: %w",
		&TerminalError{Reason: "manages nothing"})) {
		t.Error("a wrapped terminal error must still stop the wait")
	}
}

// The guard exists to end a wait that cannot succeed. It must not end one that
// can: an Application still syncing, or one that has not been created yet, are
// both states where the resource is coming and waiting is correct. A guard that
// guessed would fail runs that were about to succeed, which is worse than the
// wait it replaces.
func TestApplicationOwnsNothingStaysSilentWhenUncertain(t *testing.T) {
	// No cluster to read: kubectl fails, and the answer is not knowable.
	if err := ApplicationOwnsNothing(t.Context(), "/nonexistent-kubeconfig",
		"platform-ops", "platform-infisical-db"); err != nil {
		t.Errorf("an unreadable cluster must not be reported as terminal: %v", err)
	}
}
