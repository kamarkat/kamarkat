package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AN UNRELATED HCLOUD_TOKEN ON THE WORKSTATION HAS NO EFFECT.
//
// This is the correctness property, not an ergonomic one. A token exported in a
// shell authenticates PERFECTLY against whichever provider project issued it, and
// then answers about that project: an inventory that comes back empty, a second
// management cluster admitted because the first was invisible, a teardown that
// deletes nothing and reports success. Every one of those reads as a pass.
//
// So the test is not "the right token is used" -- it is that a wrong one cannot be
// reached at all. The seam is the only place a token enters: the resolver reads the
// box's escrow, and the inventory closure takes what it was given.
func TestAWorkstationTokenCannotReachTheInventory(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "another-projects-token")

	// The gate takes the credential it was handed and nothing else. Handed none,
	// it refuses -- it does not reach for the one in the environment, which is
	// what would let the environment answer on the box's behalf.
	if _, err := providerOwnedCounts("")(context.Background(), []string{"acme-hub"}); err == nil {
		t.Fatal("an empty credential was accepted while HCLOUD_TOKEN was set, so the " +
			"environment would have answered for the box")
	}

	// And it does not publish the credential it was given, which is how a value
	// resolved for one box leaks into whatever runs next in the same process.
	if got := os.Getenv("HCLOUD_TOKEN"); got != "another-projects-token" {
		t.Errorf("the inventory closure rewrote HCLOUD_TOKEN to %q", got)
	}
}

// bindDecision keeps a first binding and a replacement distinguishable.
//
// The authorization rule for moving a box's escrow is this table. A rule only
// reachable through a network call is a rule nothing checks.
func TestRebindAuthorizationIsExplicit(t *testing.T) {
	const want, other = "https://app.infisical.com proj-123", "https://app.infisical.com proj-999"

	if a, err := bindDecision("", want, false, "acme-hub", ".", "acme-hub"); err != nil || a != bindFirst {
		t.Errorf("a first binding was refused: %v %v", a, err)
	}
	if a, err := bindDecision(want, want, false, "acme-hub", ".", "acme-hub"); err != nil || a != bindAlreadyBound {
		t.Errorf("re-binding the same destination was not idempotent: %v %v", a, err)
	}

	// A move without --rebind is refused, and the refusal names both ends so the
	// operator can see what would have changed.
	_, err := bindDecision(want, other, false, "acme-hub", ".", "acme-hub")
	if err == nil {
		t.Fatal("a trusted binding was replaced without --rebind")
	}
	for _, s := range []string{want, other, "--rebind"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the refusal does not mention %q:\n%v", s, err)
		}
	}

	// With --rebind it is a rebind, reported as such rather than as a first
	// binding.
	if a, err := bindDecision(want, other, true, "acme-hub", ".", "acme-hub"); err != nil || a != bindRebind {
		t.Errorf("an authorized rebind was refused or mislabelled: %v %v", a, err)
	}

	// --rebind where nothing is bound is refused: performing a first binding
	// would make the two forms indistinguishable.
	if _, err := bindDecision("", want, true, "acme-hub", ".", "acme-hub"); err == nil {
		t.Fatal("--rebind performed a first binding, so the distinction the command " +
			"exists to keep would not hold")
	}
}

// The resolver is the only door, and it reads the box's declaration.
//
// With no box to read, it fails closed -- it does not reach for the environment
// or for k8-secrets, both of which were sources before.
func TestTheProviderResolverFailsClosedWithNoBox(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "another-projects-token")
	dir := t.TempDir()
	// A box directory with no declaration at all.
	if err := os.MkdirAll(filepath.Join(dir, "registry", "clusters", "acme-hub"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := resolveHCloudToken(context.Background(), dir, "acme-hub")
	if err == nil {
		t.Fatal("bootstrap resolved a provider credential for a box with no declaration; " +
			"the only candidate was the workstation's own token")
	}
	for _, unwanted := range []string{"another-projects-token"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the error carries the credential: %v", err)
		}
	}
}

// k8-secrets/hetzner/token is no longer a source.
//
// The file is still where a developer keeps the credential they ENROL, which is a
// different act. What is gone is the search: bootstrap used to read it when the
// environment was empty, so the same file could build one box and answer about
// another.
func TestTheLocalTokenFileIsNotSearched(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "k8-secrets", "hetzner")
	if err := os.MkdirAll(secrets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "token"), []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_OPS_DIR", dir)
	t.Setenv("HCLOUD_TOKEN", "")

	if _, err := resolveHCloudToken(context.Background(), dir, "acme-hub"); err == nil {
		t.Fatal("a token file beside the checkout was accepted as this box's credential")
	}
}
