package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A second reader is how the placeholder fix missed the validator: it parsed
// artifacts.yaml itself, so expanding {env} in the shared reader left it
// validating a path with the placeholder still in it. One reader is the
// invariant; this fails if another appears.
func TestOneReaderOfTheArtifactRegistry(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	// Counts file opens, not mentions: the name appears in comments too, and a
	// test that counted those would fail on documentation.
	if n := strings.Count(string(src), `"artifacts.yaml"`); n > 1 {
		t.Errorf("artifacts.yaml is opened in %d places; every consumer must go "+
			"through readADR045Registry so the {env} placeholder is resolved once", n)
	}
}

// readADR045Registry resolves the registry relative to the working directory,
// which under `go test` is the package directory rather than the repository.
func fromRepoRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
}

// The registry declares its artifacts under an {env} placeholder, which is the
// HUB OVERLAY DIRECTORY (ADR-111 Revision 7) -- one shared file under base/ meant
// prod rendered dev's Infisical project (ADR-045). Declaring the placeholder and
// never expanding it made the validator look for a directory literally called
// "{env}" and report the artifact missing, naming a path no step had written.
//
// It took the bootstrap's environment slug and the Orchestrator had to be given
// one; there is now one hub overlay, so it resolves with nothing supplied. Every
// consumer reads the registry through this function, which is what keeps an
// unresolvable path from reaching one.
func TestADR045RegistryResolvesTheOverlayPlaceholder(t *testing.T) {
	fromRepoRoot(t)
	o := &Orchestrator{}

	reg, err := o.readADR045Registry()
	if err != nil {
		t.Fatalf("reading the registry: %v", err)
	}
	if len(reg.Artifacts) == 0 {
		t.Fatal("the registry declares no artifacts; this test would prove nothing")
	}

	resolved := 0
	for _, a := range reg.Artifacts {
		if strings.Contains(a.File, "{env}") {
			t.Errorf("unexpanded placeholder in %q", a.File)
		}
		if strings.Contains(a.File, filepath.Join("environments", HubOverlayDir)) {
			resolved++
		}
	}
	if resolved == 0 {
		t.Errorf("no artifact resolved into environments/%s: the placeholder expanded "+
			"to something, but not to the hub overlay these artifacts are written to",
			HubOverlayDir)
	}
}

// The scope and the directory are both the word "management" and are different
// things: one is an Infisical environment, the other a path in this repository.
// Deriving the path from the scope would mean activating a new scope silently
// moved the artifacts, so the resolution must not read it.
func TestADR045RegistryIgnoresTheManagementScope(t *testing.T) {
	fromRepoRoot(t)

	bare, err := (&Orchestrator{}).readADR045Registry()
	if err != nil {
		t.Fatalf("reading the registry: %v", err)
	}
	scoped, err := (&Orchestrator{ManagementScope: "somewhere-else"}).readADR045Registry()
	if err != nil {
		t.Fatalf("reading the registry with a scope set: %v", err)
	}
	if len(bare.Artifacts) != len(scoped.Artifacts) {
		t.Fatalf("artifact count changed with the scope set: %d then %d",
			len(bare.Artifacts), len(scoped.Artifacts))
	}
	for i := range bare.Artifacts {
		if bare.Artifacts[i].File != scoped.Artifacts[i].File {
			t.Errorf("the management scope moved an artifact path: %q became %q",
				bare.Artifacts[i].File, scoped.Artifacts[i].File)
		}
	}
}
