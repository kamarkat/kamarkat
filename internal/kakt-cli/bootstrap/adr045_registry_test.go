package bootstrap

import (
	"os"
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

// The registry declares per-environment artifacts with an {env} placeholder,
// because these live per environment (ADR-045): one shared file under base/
// meant prod rendered dev's Infisical project. Declaring the placeholder and
// never expanding it made the validator look for a directory literally called
// "{env}" and report the artifact missing, naming a path no step had written.
func TestADR045RegistryResolvesTheEnvironmentPlaceholder(t *testing.T) {
	fromRepoRoot(t)
	o := &Orchestrator{EnvironmentSlug: "dev"}

	reg, err := o.readADR045Registry()
	if err != nil {
		t.Fatalf("reading the registry: %v", err)
	}
	if len(reg.Artifacts) == 0 {
		t.Fatal("the registry declares no artifacts; this test would prove nothing")
	}

	for _, a := range reg.Artifacts {
		if strings.Contains(a.File, "{env}") {
			t.Errorf("unexpanded placeholder in %q", a.File)
		}
	}
}

// Every consumer of the registry reads it through this function, so refusing
// here is what stops an unresolvable path reaching one of them. Returning the
// raw paths instead would restore the failure this prevents.
func TestADR045RegistryRefusesWithoutAnEnvironment(t *testing.T) {
	fromRepoRoot(t)
	o := &Orchestrator{}

	if _, err := o.readADR045Registry(); err == nil {
		t.Error("a registry with per-environment paths and no environment must " +
			"fail rather than hand back paths that cannot resolve")
	}
}
