package components

import (
	"context"
	"strings"
	"testing"
)

// A box must not be built without an escrow.
//
// This is the regression that mattered: InstallEscrowCredentials used to print a
// notice and return nil when all four values were absent, deferring to a decision
// "made before this point". That decision is RequireEscrow, which runs in another
// process during scaffolding and inspects values passed as flags — so it proves
// the values were typed, not that they reached the box.
//
// On the local path they reached nothing: scaffolding accepted them, stored them
// nowhere, and Day-0 read an empty environment. The box ran with its Infisical
// master keys existing only inside it, which is the single outcome ADR-076 makes
// the escrow mandatory to prevent, and the only trace was one line of output.
func TestBootstrapRefusesABoxWithNoEscrow(t *testing.T) {
	for _, name := range escrowEnv {
		t.Setenv(name, "")
	}
	i := &Installer{Kubeconfig: "/nonexistent-on-purpose"}

	err := i.InstallEscrowCredentials(context.Background())
	if err == nil {
		t.Fatal("a box with no escrow was accepted; its master keys would be lost with the cluster")
	}
	// Must fail on the escrow, not incidentally on the unreadable kubeconfig --
	// otherwise the refusal disappears the moment a caller has a valid one.
	if !strings.Contains(err.Error(), "no escrow is configured") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// A partial set is refused too, and separately: it produces an operator that
// believes it has an escrow and fails every backup against it, which is worse
// than having none because nothing reports the difference.
func TestBootstrapRefusesAPartiallyConfiguredEscrow(t *testing.T) {
	for _, name := range escrowEnv {
		t.Setenv(name, "")
	}
	t.Setenv(escrowEnv[0], "https://app.infisical.com")
	i := &Installer{Kubeconfig: "/nonexistent-on-purpose"}

	err := i.InstallEscrowCredentials(context.Background())
	if err == nil {
		t.Fatal("a partially configured escrow was accepted")
	}
	if !strings.Contains(err.Error(), "partially configured") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	// The message must name what is missing. Three of four is the case where an
	// operator most needs to be told which one.
	for _, name := range escrowEnv[1:] {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name the missing %s", name)
		}
	}
}
