package main

import (
	"strings"
	"testing"
)

// --on-prem without a tailnet must be refused before anything is provisioned.
//
// ADR-046 invariant 6: Cilium takes its VXLAN tunnel endpoint from a node's
// InternalIP. An on-prem node's only InternalIP is its tailnet address, so a
// control plane that is not on the tailnet cannot exchange pod traffic with it
// -- while BOTH NODES REPORT READY. That is why this is fatal rather than
// advisory; the cluster looks healthy and silently cannot route.
//
// It was a warning inside capi-init. A resumed bootstrap skips completed phases,
// so on the run that mattered it was never printed, and the failure surfaced as
// cert-manager's webhook timing out from the API server -- three layers away.
func TestOnPremWithoutATailnetIsRefused(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "")
	t.Setenv("TAILSCALE_AUTHKEY", "")
	t.Chdir(t.TempDir())

	err := requireTailnetCredentials("")
	if err == nil {
		t.Fatal("--on-prem with no tailnet name and no authkey was accepted")
	}
	for _, want := range []string{"--tailnet-name", "authkey", "invariant 6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// Both gaps are reported together, so a half-configured run is refused once
// rather than failing twice.
func TestBothTailnetGapsAreReportedTogether(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "")
	t.Setenv("TAILSCALE_AUTHKEY", "")
	t.Chdir(t.TempDir())

	err := requireTailnetCredentials("")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "--tailnet-name") || !strings.Contains(err.Error(), "authkey") {
		t.Errorf("only one gap was named: %v", err)
	}
}

// An authkey in the environment satisfies the credential half; the name is still
// required, because joining a tailnet and knowing what the box is called on it
// are different facts.
func TestAnAuthkeyAloneIsNotEnough(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-auth-example")
	t.Chdir(t.TempDir())

	if err := requireTailnetCredentials(""); err == nil {
		t.Error("an authkey with no tailnet name was accepted")
	}
	if err := requireTailnetCredentials("acme.ts.net"); err != nil {
		t.Errorf("a complete configuration was refused: %v", err)
	}
}
