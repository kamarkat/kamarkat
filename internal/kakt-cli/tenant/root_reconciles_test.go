package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every object a scaffolded cluster directory carries must have a reconciler.
//
// This is the assertion that was missing. The root Application filtered with
// `include: bundle.yaml` -- an allowlist -- so generated.yaml, which DEFINES the
// Application that owns generated/ and everything Day-0 writes there (ADR-045),
// was never applied and that Application was never created. Nothing reported it:
// the root was Synced and Healthy throughout, because it was correctly applying
// the one file it had been told about (see the ArgoCD false-green shapes: a
// component managing nothing looks exactly like a component with nothing to do).
//
// The cost was a box running on the platform's own Infisical project ids while
// its own sat in git, and hub-operator answering every reconcile with a 404.
//
// Asserted against the FILTER's effect on the files actually rendered, not
// against the filter's text. A rule that reads correctly and matches nothing is
// the defect this exists to catch.
func TestRender_RootAppliesEveryObjectInTheClusterDirectory(t *testing.T) {
	dst := render(t, testSpec())
	clusterDir := filepath.Join(dst, "registry", "clusters", testSpec().ClusterName)

	root := readYAML(t, filepath.Join(clusterDir, "root.yaml"))
	dir, _ := root["spec"].(map[string]any)["source"].(map[string]any)["directory"].(map[string]any)
	if dir == nil {
		t.Fatal("the root Application declares no directory filter")
	}

	// An allowlist is refused outright. It cannot be audited by this test in a
	// way that survives a new artifact: whatever it names today, tomorrow's
	// generated file is absent from it and silently unreconciled.
	if inc, ok := dir["include"]; ok && inc != "" {
		t.Fatalf("the root uses include: %q. Use exclude, so a new artifact is "+
			"reconciled by default and only a deliberate omission is silent", inc)
	}

	excluded := map[string]bool{}
	for _, f := range strings.Split(strings.Trim(toStr(dir["exclude"]), "{}"), ",") {
		excluded[strings.TrimSpace(f)] = true
	}

	entries, err := os.ReadDir(clusterDir)
	if err != nil {
		t.Fatalf("read %s: %v", clusterDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") {
			continue // recurse:false; subdirectories have their own Application
		}
		if excluded[name] {
			continue
		}
		doc := readYAML(t, filepath.Join(clusterDir, name))
		if doc["kind"] == nil {
			t.Errorf("%s is applied by the root but declares no kind; if it is a "+
				"values file it belongs in the exclude list", name)
		}
	}

	// The two exclusions are the ones the template justifies, and no others.
	// Adding a third silently drops whatever it names.
	for f := range excluded {
		if f != "values.yaml" && f != "root.yaml" {
			t.Errorf("%s is excluded from the root with no recorded reason; "+
				"an excluded object is reconciled by nothing", f)
		}
	}

	// The specific thing that was broken: the Application owning generated/.
	if excluded["generated.yaml"] {
		t.Error("generated.yaml is excluded, so nothing creates the Application that owns generated/")
	}
	if _, err := os.Stat(filepath.Join(clusterDir, "generated.yaml")); err != nil {
		t.Fatalf("the cluster directory carries no generated.yaml, so ADR-045 artifacts have no owner: %v", err)
	}
}

// The Application generated.yaml defines must point at the directory Day-0
// writes into. A correct filter reaching the wrong path reconciles nothing just
// as effectively.
func TestRender_GeneratedApplicationOwnsTheGeneratedDirectory(t *testing.T) {
	spec := testSpec()
	dst := render(t, spec)
	clusterDir := filepath.Join(dst, "registry", "clusters", spec.ClusterName)

	app := readYAML(t, filepath.Join(clusterDir, "generated.yaml"))
	if app["kind"] != "Application" {
		t.Fatalf("generated.yaml declares kind %v, want Application", app["kind"])
	}
	src, _ := app["spec"].(map[string]any)["source"].(map[string]any)
	want := "registry/clusters/" + spec.ClusterName + "/generated"
	if got := toStr(src["path"]); got != want {
		t.Errorf("generated Application path = %q, want %q", got, want)
	}
}

// generated/ holds objects; generated/values/ holds values files. The two are
// separated by directory because the Application that reconciles generated/
// applies whatever it finds as a Kubernetes object, and a Helm values file has
// no `kind` -- applying one fails with "Object 'Kind' is missing", which takes
// the whole Application to ComparisonError and stops the real objects beside it
// from reconciling at all.
//
// Asserted on the generated Application itself rather than on a list of
// filenames, because the filenames change and the property does not.
func TestRender_GeneratedApplicationDoesNotDescendIntoValues(t *testing.T) {
	spec := testSpec()
	dst := render(t, spec)

	app := readYAML(t, filepath.Join(dst, "registry", "clusters", spec.ClusterName, "generated.yaml"))
	src, _ := app["spec"].(map[string]any)["source"].(map[string]any)
	dir, _ := src["directory"].(map[string]any)
	if dir == nil {
		t.Fatal("the generated Application declares no directory filter")
	}
	if r, _ := dir["recurse"].(bool); r {
		t.Error("the generated Application recurses, so it will try to apply the " +
			"values files under generated/values/ as objects and fail on all of them")
	}
}

// The bundle must read the values files from where Day-0 writes them. A $values
// path pointing at the old location resolves to nothing, and ArgoCD reports a
// missing values file rather than the wrong one -- but only once the Application
// is otherwise correct, which is late.
func TestRender_BundleReadsValuesFromTheValuesDirectory(t *testing.T) {
	spec := testSpec()
	dst := render(t, spec)
	body := read(t, filepath.Join(dst, "registry", "clusters", spec.ClusterName, "bundle.yaml"))

	for _, f := range []string{"platform-pki-values.yaml", "gateway-dns-target.yaml"} {
		want := "/generated/values/" + f
		if !strings.Contains(body, want) {
			t.Errorf("bundle.yaml does not read %s from generated/values/", f)
		}
		// The old path must be gone, not merely joined by the new one.
		if strings.Contains(body, "/generated/"+f) {
			t.Errorf("bundle.yaml still reads %s from generated/, where nothing writes it", f)
		}
	}
}

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}
