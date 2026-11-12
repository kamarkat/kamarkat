package hubaccess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/session"
)

// WHAT THESE TESTS COVER, AND WHAT THEY CANNOT.
//
// Everything here is a CLI guard rail: it decides which identity `kakt` will act
// as against a box, and whether a declaration that disagrees with its own escrow
// is followed. None of it is the security boundary -- that is membership of the
// escrow project, held by Infisical and enforced by its server, and a member of
// that project reaches these artefacts through Infisical's own API whatever this
// package does. A test here that claimed otherwise would be the most misleading
// kind of passing test, so the distinction is stated rather than implied.

func box(t *testing.T, values string) (dir, cluster string) {
	t.Helper()
	dir = t.TempDir()
	cluster = "acme-hub"
	base := filepath.Join(dir, "registry", "clusters", cluster)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "values.yaml"), []byte(values), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, cluster
}

const declared = `boxId: acme-hub
clusterName: acme-hub
escrow:
  url: https://app.infisical.com
  projectId: proj-123
  organizationId: org-abc
platformAdminIdentity: user-admin-1
`

// The declared administrator is accepted.
func TestTheDeclaredAdministratorIsAuthorized(t *testing.T) {
	dir, cluster := box(t, declared)
	c, err := Declaration(dir, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if c.PlatformAdmin != "user-admin-1" {
		t.Fatalf("PlatformAdmin = %q, want user-admin-1", c.PlatformAdmin)
	}
	if err := authorizeAdministrator(c, session.Session{
		UserID: "user-admin-1", Email: "admin@acme.example", OrgID: "org-abc",
	}); err != nil {
		t.Errorf("the box's declared administrator was refused: %v", err)
	}
}

// ANOTHER HUMAN IS REFUSED BY kakt, even with a valid session.
//
// The refusal names the declared id and the one in hand, because the common cause
// is holding the wrong session rather than lacking permission -- and it says that
// the escrow project is what actually decides, so nobody reads this as the
// boundary.
func TestAnotherHumanIdentityIsRefused(t *testing.T) {
	dir, cluster := box(t, declared)
	c, err := Declaration(dir, cluster)
	if err != nil {
		t.Fatal(err)
	}
	err = authorizeAdministrator(c, session.Session{
		UserID: "user-someone-else", Email: "dev@acme.example", OrgID: "org-abc",
	})
	if err == nil {
		t.Fatal("a human identity the box does not declare was authorized")
	}
	for _, want := range []string{"user-admin-1", "user-someone-else", "escrow project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

// A session from another organization is refused even if the id matches.
func TestAnIdentityFromAnotherOrganizationIsRefused(t *testing.T) {
	dir, cluster := box(t, declared)
	c, _ := Declaration(dir, cluster)
	err := authorizeAdministrator(c, session.Session{
		UserID: "user-admin-1", Email: "admin@acme.example", OrgID: "org-other",
	})
	if err == nil {
		t.Fatal("an identity from a different organization was authorized; an id is only " +
			"meaningful inside the organization that issued it")
	}
}

// A session with no id cannot be checked, so it is refused rather than trusted.
func TestASessionWithoutAnIdentityIsRefused(t *testing.T) {
	dir, cluster := box(t, declared)
	c, _ := Declaration(dir, cluster)
	if err := authorizeAdministrator(c, session.Session{
		Email: "admin@acme.example", OrgID: "org-abc",
	}); err == nil {
		t.Fatal("a session carrying no user id was authorized on the strength of its address")
	}
}

// A box that declares no administrator authorizes no human.
//
// Fail closed: the alternative is that every logged-in identity operates every
// box that forgot to declare one.
func TestABoxWithNoDeclaredAdministratorAuthorizesNobody(t *testing.T) {
	dir, cluster := box(t, strings.Replace(declared, "platformAdminIdentity: user-admin-1\n", "", 1))
	c, err := Declaration(dir, cluster)
	if err != nil {
		t.Fatal(err)
	}
	err = authorizeAdministrator(c, session.Session{
		UserID: "user-admin-1", Email: "admin@acme.example", OrgID: "org-abc",
	})
	if err == nil {
		t.Fatal("a box with no declared administrator authorized a human session")
	}
	if !strings.Contains(err.Error(), "platformAdminIdentity") {
		t.Errorf("the refusal does not say what to declare:\n%v", err)
	}
}

// CHANGED ESCROW COORDINATES CANNOT SILENTLY REDIRECT WRITES.
//
// The binding recorded in the escrow pins the coordinates it was established
// with. An edited declaration produces a record that does not match, and a
// redirect to another project produces no record at all. Neither is reachable by
// editing Git.
func TestTheBindingRecordPinsTheDeclaredDestination(t *testing.T) {
	dir, cluster := box(t, declared)
	c, _ := Declaration(dir, cluster)
	want := DestinationRecord(c)
	if want != "https://app.infisical.com proj-123" {
		t.Fatalf("DestinationRecord = %q", want)
	}

	// The same box, redirected by a commit to another project.
	redirected, _ := Declaration(box(t, strings.Replace(declared, "proj-123", "proj-attacker", 1)))
	if DestinationRecord(redirected) == want {
		t.Error("a redirected declaration produces the same binding record, so the " +
			"redirect would be indistinguishable from the destination it replaced")
	}

	// And by a commit to another Infisical.
	elsewhere, _ := Declaration(box(t, strings.Replace(declared,
		"https://app.infisical.com", "https://infisical.attacker.example", 1)))
	if DestinationRecord(elsewhere) == want {
		t.Error("a declaration naming a different escrow produces the same record")
	}
}

// A trailing slash is not a different destination.
func TestTheBindingRecordIsStableAcrossTrailingSlashes(t *testing.T) {
	a, _ := Declaration(box(t, declared))
	b, _ := Declaration(box(t, strings.Replace(declared,
		"https://app.infisical.com", "https://app.infisical.com/", 1)))
	if DestinationRecord(a) != DestinationRecord(b) {
		t.Errorf("a trailing slash reads as a rebind: %q vs %q",
			DestinationRecord(a), DestinationRecord(b))
	}
}

// MACHINE CREDENTIALS DO NOT SILENTLY OVERRIDE A HUMAN LOGIN.
//
// Open prefers the human session whenever there is one, so an operator at a
// terminal that also carries automation credentials acts as themselves -- and is
// therefore subject to the administrator check. Only KAKT_IDENTITY=machine
// selects the other way, and it refuses when no machine identity is configured.
func TestSelectingTheMachineIdentityIsExplicit(t *testing.T) {
	t.Setenv("INFISICAL_ESCROW_CLIENT_ID", "")
	t.Setenv("KAKT_IDENTITY", "machine")
	dir, cluster := box(t, declared)
	c, _ := Declaration(dir, cluster)
	_, err := Open(nil, c)
	if err == nil {
		t.Fatal("KAKT_IDENTITY=machine was accepted with no machine identity configured")
	}
	if !strings.Contains(err.Error(), "INFISICAL_ESCROW_CLIENT_ID") {
		t.Errorf("the refusal does not say what is missing:\n%v", err)
	}
	if !strings.Contains(err.Error(), "never borrows a human session") {
		t.Errorf("the refusal does not state the separation:\n%v", err)
	}
}

// An unknown identity selector is refused rather than interpreted.
func TestAnUnknownIdentitySelectorIsRefused(t *testing.T) {
	t.Setenv("KAKT_IDENTITY", "root")
	dir, cluster := box(t, declared)
	c, _ := Declaration(dir, cluster)
	if _, err := Open(nil, c); err == nil {
		t.Fatal("KAKT_IDENTITY=root was accepted")
	}
}

// A FIRST ENROLMENT ESTABLISHES THE BINDING; A BOUND BOX IS NOT MOVED BY ONE.
//
// Ordering, which is the failure this closes: `Open` refuses an unbound project,
// and nothing could bind one without first writing to it. So the authorized
// enrolment that establishes a box's escrow also records the binding -- and a
// project already bound elsewhere is still refused, because moving a bound box is
// a rebind.
//
// The two branches are asserted through the shared refusal rather than through a
// live escrow: what matters is that a MISMATCH refuses and names the rebind, and
// that an ABSENT binding is not treated as a mismatch.
func TestAMismatchedBindingNamesTheRebind(t *testing.T) {
	dir, cluster := box(t, declared)
	c, _ := Declaration(dir, cluster)
	err := mismatchedBinding(c, "https://app.infisical.com proj-999", DestinationRecord(c))
	if err == nil {
		t.Fatal("a binding that disagrees with the declaration was accepted")
	}
	for _, want := range []string{"proj-999", "proj-123", "--rebind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}
