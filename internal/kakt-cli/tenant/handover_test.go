package tenant

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestProviderCredentialIsTheProvidersOwnName(t *testing.T) {
	// A tenant reading its repository secrets should see a name it recognises
	// from the provider's documentation rather than one invented here.
	for _, p := range []string{"hetzner", "hybrid"} {
		got, err := ProviderCredential(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if got != "HCLOUD_TOKEN" {
			t.Errorf("%s: got %q, want HCLOUD_TOKEN", p, got)
		}
	}

	// An unknown provider is named rather than guessed at: a wrong secret name
	// produces a workflow that fails on a missing credential the tenant did set.
	if _, err := ProviderCredential("gcp"); err == nil {
		t.Error("an unknown provider must be reported, not given a default secret name")
	}
}

// Both or neither. A repository with one secret fails at the workflow step that
// needs the other, after the tenant was told setup succeeded.
func TestSecretsAreCompleteOnlyWithBoth(t *testing.T) {
	cases := []struct {
		name string
		s    Secrets
		want bool
	}{
		{"neither", Secrets{}, false},
		{"provider only", Secrets{ProviderToken: "x"}, false},
		{"gitops only", Secrets{GitopsToken: "y"}, false},
		{"both", Secrets{ProviderToken: "x", GitopsToken: "y"}, true},
		{"whitespace is not a value", Secrets{ProviderToken: "  ", GitopsToken: "y"}, false},
	}
	for _, c := range cases {
		if got := c.s.Complete(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Scaffolding has already created a repository by the time secrets are wanted,
// so a missing one cannot simply fail. What is owed is where it stopped and what
// completes it.
func TestHandoverSaysWhatIsMissingAndHowToFinish(t *testing.T) {
	spec := Spec{
		TenantID: "acme", GitOrg: "acme-inc", Provider: "hetzner",
		ClusterName: "acme-hub", BundleVersion: "0.1.9", GitopsRepo: "acme-gitops-1",
	}

	var buf bytes.Buffer
	HandoverInstructions(spec, Secrets{}, bufio.NewWriter(&buf))
	out := buf.String()

	for _, want := range []string{
		"acme-inc/acme-gitops-1",            // the repository that now exists
		"0.1.9",                             // what it is pinned to
		"HCLOUD_TOKEN",                      // the provider's own secret name
		"GITOPS_REPO_WRITE_TOKEN",           // and the other one
		"push is what builds it",            // what completes it
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the handover does not mention %q:\n%s", want, out)
		}
	}

	// With one secret present it must say which is missing rather than repeat the
	// generic case: a tenant that set one and is told "neither is set" will look
	// in the wrong place.
	buf.Reset()
	HandoverInstructions(spec, Secrets{ProviderToken: "set"}, bufio.NewWriter(&buf))
	if !strings.Contains(buf.String(), "GITOPS_REPO_WRITE_TOKEN is not set") {
		t.Errorf("the handover must name the one that is missing:\n%s", buf.String())
	}
}

// A hybrid box needs a third secret, and the handover has to say so. Hetzner must
// not be told about it: naming a credential nothing on that box reads sends the
// tenant to obtain something they will never use.
func TestHandoverNamesTheTailnetKeyOnlyWhereItIsRead(t *testing.T) {
	hybrid := Spec{TenantID: "acme", GitOrg: "acme-inc", Provider: "hybrid", BundleVersion: "0.1.9"}
	hetzner := Spec{TenantID: "acme", GitOrg: "acme-inc", Provider: "hetzner", BundleVersion: "0.1.9"}
	both := Secrets{ProviderToken: "set", GitopsToken: "set"}

	var buf bytes.Buffer
	HandoverInstructions(hybrid, both, bufio.NewWriter(&buf))
	if !strings.Contains(buf.String(), "TS_AUTHKEY") {
		t.Errorf("a hybrid box with no tailnet key must be told which one is missing:\n%s", buf.String())
	}

	buf.Reset()
	HandoverInstructions(hetzner, both, bufio.NewWriter(&buf))
	if strings.Contains(buf.String(), "TS_AUTHKEY") {
		t.Errorf("hetzner reads no tailnet key and must not be asked for one:\n%s", buf.String())
	}

	// And with everything a hybrid box needs, it is dispatchable.
	if !(Secrets{ProviderToken: "a", GitopsToken: "b", TailscaleAuthkey: "c"}).CompleteFor("hybrid") {
		t.Error("a hybrid box with all three secrets must be dispatchable")
	}
	if (Secrets{ProviderToken: "a", GitopsToken: "b"}).CompleteFor("hybrid") {
		t.Error("a hybrid box without a tailnet key must not be dispatchable")
	}
	if !(Secrets{ProviderToken: "a", GitopsToken: "b"}).CompleteFor("hetzner") {
		t.Error("hetzner needs only two secrets")
	}
}

// The credential an application build needs is set once, at the ORGANISATION,
// and never per application repository.
//
// This is the defect the arrangement exists to prevent, and it is not
// hypothetical: waypoint's bff published a chart on every commit for months and
// pinned a version not once, because its deploy job read a
// GITOPS_REPO_WRITE_TOKEN that nothing had set. `publish` was green every time,
// so the workflow looked healthy while the thing it existed to do never
// happened.
//
// A repository-level secret cannot fix that, because `kakt box init`
// knows exactly one repository -- <tenant>-gitops -- and applications are
// created, renamed and retired by the tenant long afterwards. The platform must
// not track that set (it would fall behind, silently); it publishes one
// credential the whole organisation inherits instead.
func TestGitopsTokenIsPublishedToTheOrganisationNotPerRepository(t *testing.T) {
	src, err := os.ReadFile("handover.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)

	if !strings.Contains(s, `"--org", org, "--visibility", "all"`) {
		t.Error("GITOPS_REPO_WRITE_TOKEN must be set at the organisation with visibility all, " +
			"so an application repository created after scaffolding inherits it")
	}

	// Not fatal. The box being scaffolded is complete without it; aborting here
	// would destroy a working repository over a credential no cluster reads.
	i := strings.Index(s, "func setOrgGitopsToken")
	if i < 0 {
		t.Fatal("setOrgGitopsToken is gone: the organisation-level publish is the mechanism")
	}
	body := s[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	if strings.Contains(body, "return fmt.Errorf") {
		t.Error("a failure to set the organisation secret must be reported, not fatal: " +
			"the cluster does not read this credential, a future application build does")
	}
	if !strings.Contains(body, "gh secret set GITOPS_REPO_WRITE_TOKEN --org") {
		t.Error("the failure path must print the one command that completes it by hand")
	}
}
