package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WHAT THESE COVER. A box declares the Infisical organization its escrow belongs
// to, because the administrator it names is an id issued by one organization and
// meaningless in another. The value had one reader and no writer, so every box
// rendered before this declared an empty one -- and an empty one used to disable
// the comparison rather than fail it.
//
// Two contracts follow, and both are asserted here: a box cannot be created or
// resumed without the organization, and correcting an existing box's declaration
// cannot change which escrow that box belongs to.

func completeSpec() Spec {
	return Spec{
		TenantID: "acme", GitOrg: "acme-inc", Domain: "acme.example",
		Provider: "hetzner", Region: "hel1", BundleVersion: "0.1.16-rc.1",
		PlatformRepoURL: "https://github.com/kamarkat/kamarkat",
		BundleRegistry:  "ghcr.io/kamarkat/charts",
		EscrowURL:       "https://app.infisical.com", EscrowProjectID: "proj-123",
	}
}

// A box with no organization to declare is refused before anything is created.
func TestABoxWithoutAnEscrowOrganizationIsRefused(t *testing.T) {
	s := completeSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("the fixture is not otherwise complete: %v", err)
	}
	err := s.RequireEscrowOrganization()
	if err == nil {
		t.Fatal("a box declaring no escrow organization was accepted")
	}
	if !strings.Contains(err.Error(), "kakt escrow init") {
		t.Errorf("the refusal does not say where the value comes from:\n%v", err)
	}
	s.EscrowOrgID = "org-abc"
	if err := s.RequireEscrowOrganization(); err != nil {
		t.Errorf("a box declaring an organization was refused: %v", err)
	}
}

func declarations(t *testing.T, projectID, orgID string) string {
	t.Helper()
	dir := t.TempDir()
	body := "boxId: acme-1700000000\n" +
		"# a comment that has to survive the correction\n" +
		"escrow:\n" +
		"  url: https://app.infisical.com\n" +
		"  projectId: " + projectID + "\n" +
		"  organizationId: " + orgID + "\n" +
		"platformAdminIdentity: user-admin-1\n"
	reg := filepath.Join(dir, RegistryDir)
	cluster := filepath.Join(reg, "clusters", "acme-mgmt-1700000000")
	if err := os.MkdirAll(cluster, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(reg, BoxDeclFile), filepath.Join(cluster, "values.yaml")} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// An incomplete declaration is filled in, in every file that holds it, and
// nothing else in those files changes.
func TestAnIncompleteDeclarationIsCorrected(t *testing.T) {
	dir := declarations(t, "proj-123", "")
	changed, err := RecordEscrowOrganization(dir, "proj-123", "org-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("corrected %d files, want both the box and its cluster: %v", len(changed), changed)
	}
	for _, p := range changed {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		got := string(raw)
		if !strings.Contains(got, "organizationId: org-abc") {
			t.Errorf("%s was not corrected:\n%s", p, got)
		}
		if !strings.Contains(got, "# a comment that has to survive the correction") {
			t.Errorf("%s lost its comments, so the file was rewritten rather than corrected", p)
		}
		if !strings.Contains(got, "projectId: proj-123") || !strings.Contains(got, "platformAdminIdentity: user-admin-1") {
			t.Errorf("%s changed something other than the organization:\n%s", p, got)
		}
	}
	// Running it again is not a second correction.
	again, err := RecordEscrowOrganization(dir, "proj-123", "org-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a re-run rewrote %v", again)
	}
}

// THE OWNERSHIP CONTRACT. The organization is written only into a declaration
// that already names the escrow project this run authenticated against.
//
// Without this an unbound declaration could be claimed by any organization that
// happened to be signed in, simply by running the command inside the checkout.
func TestADeclarationNamingAnotherProjectIsNotClaimed(t *testing.T) {
	dir := declarations(t, "proj-999", "")
	changed, err := RecordEscrowOrganization(dir, "proj-123", "org-abc")
	if err == nil {
		t.Fatal("a declaration naming another escrow project was claimed by this session")
	}
	if len(changed) != 0 {
		t.Errorf("files were changed before the refusal: %v", changed)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, RegistryDir, BoxDeclFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(raw), "organizationId: \n") && !strings.Contains(string(raw), "organizationId:\n") {
		t.Errorf("the declaration was modified despite the refusal:\n%s", raw)
	}
	if !strings.Contains(err.Error(), "proj-999") || !strings.Contains(err.Error(), "proj-123") {
		t.Errorf("the refusal does not name both projects:\n%v", err)
	}
}

// A box already bound to another organization is not repointed.
func TestAnotherOrganizationIsNotOverwritten(t *testing.T) {
	dir := declarations(t, "proj-123", "org-original")
	if _, err := RecordEscrowOrganization(dir, "proj-123", "org-abc"); err == nil {
		t.Fatal("an existing organization binding was replaced")
	}
	raw, err := os.ReadFile(filepath.Join(dir, RegistryDir, BoxDeclFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "organizationId: org-original") {
		t.Errorf("the original organization did not survive:\n%s", raw)
	}
}
