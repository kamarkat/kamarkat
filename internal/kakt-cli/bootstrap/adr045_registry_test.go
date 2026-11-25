package bootstrap

import (
	"os"
	"path/filepath"
	"regexp"
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
		t.Skip("the registry declares no artifacts, so there is no placeholder to resolve")
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

// Every registered artifact must have code that writes it. adr045-commit fails
// on a registered file that is absent, so an entry whose writer was removed (as
// self-hosted Infisical's was under ADR-117) stops every Day-0 run there.
func TestEveryADR045ArtifactHasAWriter(t *testing.T) {
	fromRepoRoot(t)
	reg, err := (&Orchestrator{}).readADR045Registry()
	if err != nil {
		t.Fatalf("reading the registry: %v", err)
	}
	if len(reg.Artifacts) == 0 {
		return
	}

	var sources []string
	for _, root := range []string{"internal", "operators", "cmd"} {
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && (d.Name() == "embedded" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				if b, err := os.ReadFile(path); err == nil {
					sources = append(sources, string(b))
				}
			}
			return nil
		})
	}

	for _, a := range reg.Artifacts {
		name := filepath.Base(a.File)
		written := false
		for _, src := range sources {
			if strings.Contains(src, `"`+name+`"`) {
				written = true
				break
			}
		}
		if !written {
			t.Errorf("%s is registered in manifests/generated/artifacts.yaml, but no "+
				"non-test Go source names %q, so nothing writes it and adr045-commit "+
				"will fail on it", a.File, name)
		}
	}
}

// Every generated values file the box's bundle reads must be registered, because
// registration is what gets it committed and pushed; ArgoCD reads only git.
// gateway-dns-target.yaml was committed only as a side effect of another
// registered file in the same directory, and when that entry was removed every
// hub hostname went NXDOMAIN.
func TestEveryGeneratedValuesFileTheBundleReadsIsRegistered(t *testing.T) {
	fromRepoRoot(t)
	src, err := os.ReadFile(filepath.Join("internal", "kakt-cli", "tenant", "render_box.go"))
	if err != nil {
		t.Fatal(err)
	}
	read := regexp.MustCompile(`/generated/values/([A-Za-z0-9._-]+\.yaml)`).FindAllStringSubmatch(string(src), -1)
	if len(read) == 0 {
		t.Fatal("the bundle reads no generated values file; this test would prove nothing")
	}
	reg, err := (&Orchestrator{}).readADR045Registry()
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, a := range reg.Artifacts {
		registered[filepath.Base(a.File)] = true
	}
	for _, m := range read {
		if !registered[m[1]] {
			t.Errorf("the box bundle reads generated/values/%s, but it is not in "+
				"manifests/generated/artifacts.yaml, so adr045-commit never commits it", m[1])
		}
	}
}
