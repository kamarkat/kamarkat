package tenant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// A FIXED CLOCK for the whole test package.
//
// A box's identity is minted from the second it was created in, so every
// rendered name would otherwise differ between runs -- and an assertion on a
// rendered path could only be written as a pattern. Pinned here rather than
// exposed as a setting: a suffix a caller can choose is a second naming scheme,
// and two boxes could then be given the same identity deliberately.
const testSuffix = "1700000000"

func init() {
	boxClock = func() time.Time { return time.Unix(1700000000, 0).UTC() }
}

// The three names testSpec's box owns, under that clock.
var (
	testBoxID = "acme-" + testSuffix
	testRepo  = "acme-gitops-" + testSuffix
	testHub   = "acme-hub-" + testSuffix
)

func testSpec() Spec {
	s := Spec{
		TenantID: "acme", GitOrg: "acme-inc", Domain: "acme.example",
		Provider: "hetzner", Region: "hel1",
		BundleVersion:   "v1.2.3",
		PlatformRepoURL: "https://github.com/kamarkat/kamarkat",
		BundleRegistry:  "ghcr.io/kamarkat/charts",
	}
	// Minted the way the command mints it, so the fixture cannot drift from the
	// derivation it is testing.
	s = DeriveBoxIdentity(s.TenantID).Apply(s)
	s.ClusterName = s.ManagementCluster
	return s
}

// render is the two steps onboarding performs: create the box, then declare its
// first management cluster. One helper rather than two, because every assertion
// below is about the state a tenant's repository reaches, and that state is
// reached by both commands.
func render(t *testing.T, s Spec) string {
	t.Helper()
	return renderInto(t, t.TempDir(), s)
}

func renderInto(t *testing.T, dst string, s Spec) string {
	t.Helper()
	if err := RenderBox(filepath.Join("..", "..", "..", "manifests", "tenants", "gitops-template"), dst, s); err != nil {
		t.Fatalf("RenderBox: %v", err)
	}
	if s.ManagementCluster != "" {
		if _, err := (ManagementCluster{GitopsDir: dst}).Add(); err != nil {
			t.Fatalf("declare the first management cluster: %v", err)
		}
	}
	return dst
}

func read(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// The template a tenant keeps must still be a template. Substituting cluster
// facts everywhere leaves it hard-coded to whichever cluster was rendered first,
// so a tenant adding its second cluster silently gets a copy of its first --
// same name, same environment, same region -- and the collision appears as two
// clusters contending for one set of resources rather than as a bad render.
func TestRender_RetainedTemplateKeepsPerClusterTokens(t *testing.T) {
	dst := render(t, testSpec())
	// Asserted per file, because the two carry different cluster facts: the
	// Application names the cluster, and the values file describes where it runs.
	for f, tokens := range map[string][]string{
		// No Application files here any anywhere else in this template: the two a
		// management cluster holds for a workload cluster are GENERATED from the
		// claim below, by the ApplicationSets in templates/mgmt. What a tenant
		// keeps is the declaration itself.
		"infrastructure/spokepool-hetzner.yaml": {"<CLUSTER_NAME>", "<ENVIRONMENT>", "<CLOUD_PROVIDER>"},
		"infrastructure/namespace.yaml":         {"<CLUSTER_NAME>"},
		"values.yaml":                           {"<ENVIRONMENT>", "<CLOUD_PROVIDER>", "<CLOUD_REGION>", "<BUNDLE_VERSION>"},
	} {
		got := read(t, dst, "templates", "workload-cluster", f)
		for _, token := range tokens {
			if !strings.Contains(got, token) {
				t.Errorf("templates/workload-cluster/%s lost %s; it is no longer a template", f, token)
			}
		}
		if strings.Contains(got, testHub) {
			t.Errorf("templates/workload-cluster/%s was rendered against the control plane", f)
		}
	}
}

// Tenant facts are known at onboarding and are the same for every cluster, so a
// tenant should never have to supply its own repository URL to add one.
func TestRender_RetainedTemplateHasTenantFactsFilled(t *testing.T) {
	dst := render(t, testSpec())
	got := read(t, dst, "templates", "workload-cluster", "values.yaml")
	if !strings.Contains(got, "https://github.com/acme-inc/"+testRepo) {
		t.Error("retained template does not name the tenant's own repository")
	}
	if strings.Contains(got, "<GITOPS_REPO_URL>") || strings.Contains(got, "<PLATFORM_REPO_URL>") {
		t.Error("tenant-wide tokens survived into the retained template")
	}
}

func TestRender_HydratesControlPlaneAndKeepsItsTemplate(t *testing.T) {
	dst := render(t, testSpec())
	if _, err := os.Stat(filepath.Join(dst, "registry", "clusters", testHub, "bundle.yaml")); err != nil {
		t.Fatalf("control plane not hydrated: %v", err)
	}
	// KEPT, where it used to be consumed and deleted. The box is created before
	// any cluster, so this template is what `kakt tenant add-management` renders
	// the first management cluster from -- and a box whose clusters have all been
	// removed can declare another.
	if _, err := os.Stat(filepath.Join(dst, "templates", "mgmt")); err != nil {
		t.Errorf("the management template must survive the box that carries it: %v", err)
	}
	got := read(t, dst, "registry", "clusters", testHub, "bundle.yaml")
	if strings.Contains(got, "<") && strings.Contains(got, ">") {
		for _, line := range strings.Split(got, "\n") {
			if tok := unresolvedToken(line); tok != "" {
				t.Errorf("instance still carries %s", tok)
			}
		}
	}
}

// An unsubstituted token reaches the tenant's repository as a literal, and the
// Application carrying it fails to resolve a repository or a version with an
// error naming neither.
func TestRender_RefusesToLeaveATokenInTheInstance(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "tmpl")
	if err := os.MkdirAll(filepath.Join(tmpl, "templates", "mgmt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpl, RegistryDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// The minimum a box declaration must carry for the cluster render to have a
	// box to read.
	if err := os.WriteFile(filepath.Join(tmpl, RegistryDir, BoxDeclFile),
		[]byte("boxId: <BOX_ID>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "templates", "mgmt", "x.yaml"),
		[]byte("a: <CLUSTER_NAME>\nb: <NOT_A_REAL_TOKEN>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := RenderBox(tmpl, out, testSpec()); err != nil {
		t.Fatalf("RenderBox: %v", err)
	}
	// The refusal moved with the content: cluster tokens are substituted when the
	// cluster is declared, so that is where a token nothing resolves is caught.
	_, err := (ManagementCluster{GitopsDir: out, Name: testHub}).Add()
	if err == nil || !strings.Contains(err.Error(), "<NOT_A_REAL_TOKEN>") {
		t.Fatalf("expected the unsubstituted token to be named, got %v", err)
	}
}

// A token is an identifier or a location. A secret substituted into one would be
// committed to the tenant's repository and would survive in its history after
// any later correction.
func TestRender_RefusesSecretMaterialInATokenValue(t *testing.T) {
	s := testSpec()
	s.Domain = "-----BEGIN RSA PRIVATE KEY-----"
	if err := RenderBox(filepath.Join("..", "..", "..", "manifests", "tenants", "gitops-template"), t.TempDir(), s); err == nil {
		t.Fatal("expected a refusal for secret-looking token value")
	}
}

func TestSpec_RepoNameAndValidation(t *testing.T) {
	if got := testSpec().RepoName(); got != testRepo {
		t.Errorf("RepoName = %q, want %s", got, testRepo)
	}
	var empty Spec
	err := empty.Validate()
	if err == nil {
		t.Fatal("expected validation to fail on an empty spec")
	}
	for _, want := range []string{"tenant", "box", "domain"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error does not name %q: %v", want, err)
		}
	}
}

// A scaffolded cluster resolves the published distribution and its own values,
// and nothing else. The shape is checked rather than the text because every part
// of it has failed in production at least once.
func TestRender_BundleResolvesTheRegistryAndTheTenantsOwnValues(t *testing.T) {
	dir := renderInto(t, t.TempDir(), testSpec())

	raw, err := os.ReadFile(filepath.Join(dir, "registry", "clusters", testHub, "bundle.yaml"))
	if err != nil {
		t.Fatalf("reading the hydrated bundle: %v", err)
	}

	var app struct {
		Spec struct {
			Source  map[string]any `yaml:"source"`
			Sources []struct {
				RepoURL        string `yaml:"repoURL"`
				Chart          string `yaml:"chart"`
				TargetRevision string `yaml:"targetRevision"`
				Ref            string `yaml:"ref"`
			} `yaml:"sources"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &app); err != nil {
		t.Fatalf("parsing the hydrated bundle: %v", err)
	}

	// ArgoCD's GetSources returns spec.sources whenever it is non-empty and never
	// consults spec.source, so an Application holding both resolves the array and
	// silently ignores the chart. Released clusters carried the platform's own
	// git repository this way for three versions.
	if app.Spec.Source != nil {
		t.Error("spec.source must be absent when spec.sources is used; ArgoCD " +
			"ignores spec.source entirely and the conflict is silent")
	}
	if len(app.Spec.Sources) != 2 {
		t.Fatalf("expected the chart and the tenant's values, got %d source(s)", len(app.Spec.Sources))
	}

	chart := app.Spec.Sources[0]
	if chart.Chart == "" {
		t.Error("the first source must name a chart: the bundle is a published " +
			"distribution, not a path in a repository (ADR-063)")
	}
	if strings.Contains(chart.RepoURL, "://") {
		t.Errorf("the registry must carry no scheme, got %q; ArgoCD passes "+
			"anything else to `helm pull --repo`, which does not speak OCI", chart.RepoURL)
	}
	if chart.TargetRevision == "<BUNDLE_VERSION>" || chart.TargetRevision == "" {
		t.Errorf("the bundle version must be substituted, got %q", chart.TargetRevision)
	}

	// The second source is the tenant's own repository. If it were the
	// platform's, revoking the platform's access would stop the cluster
	// reconciling, which ADR-063 and ADR-065 both forbid.
	values := app.Spec.Sources[1]
	if values.Ref != "values" {
		t.Errorf("the second source must be the values ref, got %q", values.Ref)
	}
	if strings.Contains(values.RepoURL, "zero-ops") {
		t.Errorf("a scaffolded cluster must not resolve the platform's repository "+
			"at runtime, got %q", values.RepoURL)
	}
}

// The Renovate manager and the bundle it matches are two files that must agree,
// and nothing else notices when they stop agreeing: a manager whose regex misses
// produces no proposals at all, which looks exactly like a tenant already being
// up to date. This renders both and matches one against the other.
func TestRender_RenovateManagerMatchesTheBundleItProposesAgainst(t *testing.T) {
	dir := renderInto(t, t.TempDir(), testSpec())

	raw, err := os.ReadFile(filepath.Join(dir, "renovate.json"))
	if err != nil {
		t.Fatalf("a scaffolded repository must carry a Renovate config, or the "+
			"platform has no way to propose to it (ADR-064): %v", err)
	}

	var cfg struct {
		CustomManagers []struct {
			MatchStrings        []string `json:"matchStrings"`
			PackageNameTemplate string   `json:"packageNameTemplate"`
			DatasourceTemplate  string   `json:"datasourceTemplate"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("renovate.json is not valid JSON: %v", err)
	}
	if len(cfg.CustomManagers) != 1 {
		t.Fatalf("expected one custom manager, got %d", len(cfg.CustomManagers))
	}
	m := cfg.CustomManagers[0]

	if strings.Contains(m.PackageNameTemplate, "<") {
		t.Errorf("the registry token was not substituted: %q", m.PackageNameTemplate)
	}
	if m.DatasourceTemplate != "docker" {
		t.Errorf("an OCI chart is a docker datasource, got %q", m.DatasourceTemplate)
	}

	bundle, err := os.ReadFile(filepath.Join(dir, "registry", "clusters", testHub, "bundle.yaml"))
	if err != nil {
		t.Fatalf("reading the hydrated bundle: %v", err)
	}

	// Go's regexp is RE2 and Renovate's is JavaScript, but the construct used
	// here -- literals, \s+ and one named group -- means the same in both.
	re, err := regexp.Compile(m.MatchStrings[0])
	if err != nil {
		t.Fatalf("the manager's regex does not compile: %v", err)
	}
	got := re.FindSubmatch(bundle)
	if got == nil {
		t.Fatalf("the Renovate manager matches nothing in the bundle it exists to "+
			"propose against; no proposal would ever open, and a tenant would look "+
			"up to date while falling behind.\nregex: %s", m.MatchStrings[0])
	}

	// It must find the chart's version, not the branch the tenant's own values
	// are read from. Rewriting that one would repoint this repository at a chart
	// version and the cluster would stop resolving its values.
	if v := string(got[1]); v != testSpec().BundleVersion {
		t.Errorf("the manager matched %q; expected the bundle version %q",
			v, testSpec().BundleVersion)
	}
}

// The scaffolded repository must be able to bootstrap itself (ADR-072). The
// failure this guards is one GitHub reports as a missing workflow rather than as
// a scaffolding fault: a pin left as @v<BUNDLE_VERSION> is a reference to a tag
// by that literal name.
func TestRender_BootstrapWorkflowIsPinnedAndSubstituted(t *testing.T) {
	dir := renderInto(t, t.TempDir(), testSpec())

	path := filepath.Join(dir, ".github", "workflows", "bootstrap-cluster.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a scaffolded repository must carry the workflow that bootstraps "+
			"it, or Day-0 is a manual step run from somewhere else (ADR-072): %v", err)
	}
	body := string(raw)

	if strings.Contains(body, "<") && regexp.MustCompile(`<[A-Z_]{3,}>`).MatchString(body) {
		t.Errorf("the workflow still carries placeholders:\n%s",
			strings.Join(regexp.MustCompile(`<[A-Z_]{3,}>`).FindAllString(body, -1), " "))
	}

	// Pinned to a released tag, not a branch. The execution logic is then fixed
	// for this repository until someone here moves the pin, so a change to the
	// platform's workflow cannot alter how this repository bootstraps.
	pin := regexp.MustCompile(`uses:\s+\S+/\.github/workflows/\S+@(\S+)`).FindStringSubmatch(body)
	if pin == nil {
		t.Fatal("the workflow does not call the platform's reusable workflow")
	}
	if pin[1] == "main" || pin[1] == "master" || strings.HasPrefix(pin[1], "refs/heads/") {
		t.Errorf("the platform workflow is pinned to a branch (%s); it must be a "+
			"released tag, or this repository's bootstrap changes without anyone "+
			"here deciding it should", pin[1])
	}
	if want := "v" + testSpec().BundleVersion; pin[1] != want {
		t.Errorf("pinned to %s, expected %s -- the workflow and the bundle a "+
			"cluster runs are the same release", pin[1], want)
	}
}

// The generated values file does not exist until Day-0 writes it, and helm
// EXITS on a values file it cannot open. Without ignoreMissingValueFiles the
// seed Application cannot render at all, and the bootstrap dies at boundary 01
// with "no ApplicationSet targets project boundary-01" -- naming the boundary
// rather than the values file, which is why this is asserted rather than left
// to be rediscovered.
//
// Ordering cannot fix it: the file is written by bootstrap-infisical-api, which
// runs after the boundaries it would unblock.
func TestBundleToleratesTheGeneratedValuesFileNotExistingYet(t *testing.T) {
	for _, version := range []string{"0.1.16-rc.8", "development"} {
		t.Run(version, func(t *testing.T) {
			spec := Spec{
				TenantID: "acme", GitOrg: "acme-io", Domain: "nutgraf.in",
				ClusterName: "acme-hub", Provider: "hetzner",
				BundleVersion: version, BundleRegistry: "ghcr.io/kamarkat/charts",
				PlatformRepoURL: "https://github.com/kamarkat/kamarkat",
			}
			src := spec.chartSource()
			// generated/values/, where Day-0 writes it. Values files are kept
			// out of generated/ because the Application reconciling that
			// directory applies what it finds there as objects, and a values
			// file has no kind.
			if !strings.Contains(src, "generated/values/platform-pki-values.yaml") {
				t.Fatal("the bundle does not read the generated PKI values at all")
			}
			if !strings.Contains(src, "ignoreMissingValueFiles: true") {
				t.Error("the bundle reads a file Day-0 has not written yet without " +
					"ignoreMissingValueFiles; the seed cannot render on a first bootstrap")
			}
		})
	}
}
