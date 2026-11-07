package app

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fleetFixture() App {
	return App{
		TenantID: "acme",
		AppID:    "storefront",
		Secrets: []Declaration{
			{Name: "ACME_INTERNAL_TOKEN", Capability: "the BFF/SDK channel", Workloads: []string{"bff", "sdk"}},
			{Name: "AI_GATEWAY_API_KEY", Capability: "the AI gateway", Workloads: []string{"sdk"}},
			{Name: "S3_ACCESS_KEY_ID", Capability: "object storage", Workloads: []string{"sdk"}},
		},
	}
}

// The path is derived from the application's own declaration. Written out per key, it
// was twenty-three chances to name the wrong cluster.
func TestSecretPathIsPerDeployment(t *testing.T) {
	// The segment is the APP. That is what it always held -- before ADR-088 the
	// field feeding it was called tenantId but carried a product -- so the
	// content was right and only the label was wrong. Correcting the label
	// means moving live key material, which is deferred to its own change.
	// One folder per cluster serving the environment: each deployment's store
	// reads only its own /spoke-pool/<cluster>/, so a single path would supply
	// the secret to exactly one of them.
	if got, want := fleetFixture().SecretPathOn("nutgraf-01"), "/spoke-pool/nutgraf-01/tenants/storefront"; got != want {
		t.Fatalf("SecretPathOn(nutgraf-01) = %q, want %q", got, want)
	}
	if got, want := fleetFixture().SecretPathOn("nutgraf-02"), "/spoke-pool/nutgraf-02/tenants/storefront"; got != want {
		t.Fatalf("SecretPathOn(nutgraf-02) = %q, want %q", got, want)
	}
}

func TestJoinSeparatesPresentAbsentAndOrphan(t *testing.T) {
	// db-credentials is platform-written and appears in no application's `secrets:`.
	s := Join(fleetFixture(), "nutgraf-01", []string{"ACME_INTERNAL_TOKEN", "db-credentials"})

	var present, absent, orphan []string
	for _, e := range s.Entries {
		switch {
		case e.Orphan:
			orphan = append(orphan, e.Name)
		case e.Present:
			present = append(present, e.Name)
		default:
			absent = append(absent, e.Name)
		}
	}

	if want := []string{"ACME_INTERNAL_TOKEN"}; !reflect.DeepEqual(present, want) {
		t.Errorf("present = %v, want %v", present, want)
	}
	if want := []string{"AI_GATEWAY_API_KEY", "S3_ACCESS_KEY_ID"}; !reflect.DeepEqual(absent, want) {
		t.Errorf("absent = %v, want %v", absent, want)
	}
	// Reported, not removed: the platform owns this key and the application cannot
	// know that from its own file.
	if want := []string{"db-credentials"}; !reflect.DeepEqual(orphan, want) {
		t.Errorf("orphan = %v, want %v", orphan, want)
	}
}

// The consequence a per-key list understates. One absent key fails the whole
// ExternalSecret, so the SDK loses all three of its keys -- not the one.
func TestWithheldWorkloadsReportsTheWholeObject(t *testing.T) {
	s := Join(fleetFixture(), "nutgraf-01", []string{"ACME_INTERNAL_TOKEN"})

	got := s.WithheldWorkloads()
	if got["sdk"] != 3 {
		t.Errorf("sdk withheld = %d, want 3 (every key in its ExternalSecret, not just the absent ones)", got["sdk"])
	}
	// The BFF's only key IS present, so nothing of the BFF's is withheld. This
	// is the property per-workload grouping buys: the SDK's missing credential
	// does not stop the BFF.
	if _, broken := got["bff"]; broken {
		t.Errorf("bff reported as withheld, but its only key is present — per-workload grouping is not isolating failures")
	}
}

func TestWithheldIsEmptyWhenEverythingIsSupplied(t *testing.T) {
	s := Join(fleetFixture(), "nutgraf-01", []string{"ACME_INTERNAL_TOKEN", "AI_GATEWAY_API_KEY", "S3_ACCESS_KEY_ID"})
	if n := len(s.Missing()); n != 0 {
		t.Fatalf("Missing() = %d entries, want 0", n)
	}
	if n := len(s.WithheldWorkloads()); n != 0 {
		t.Fatalf("WithheldWorkloads() = %v, want empty", s.WithheldWorkloads())
	}
}

// No type in this package has a field a secret value could occupy. Asserted
// structurally rather than by reviewing the formatter: a value that cannot be
// held cannot be printed, and this fails if a field is ever added.
func TestNoTypeCanHoldASecretValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  any
	}{
		{"Declaration", Declaration{}},
		{"Entry", Entry{}},
		{"Status", Status{}},
		{"App", App{}},
	} {
		v := reflect.TypeOf(tc.typ)
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			lower := strings.ToLower(f.Name)
			if strings.Contains(lower, "value") || strings.Contains(lower, "secretvalue") || lower == "password" {
				t.Errorf("%s has field %q: this package reports secrets BY NAME, and a field "+
					"that can hold a value is one formatting mistake from a credential in a "+
					"scrollback buffer", tc.name, f.Name)
			}
		}
	}
}

func TestLoadRejectsAFleetWithNoPath(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		// tenantId is still required. clusterName is NOT a field any more: an
		// application is deployed to every cluster serving its environment, so
		// there is no placement in this file to be missing.
		{"no tenantId", "appId: acme\n", "tenantId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			envDir := filepath.Join(dir, "environments", "dev", "acme")
			if err := os.MkdirAll(envDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(envDir, "values.yaml"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}

			// Half a path is worse than none: it produces a valid string naming
			// a folder that will never hold anything.
			_, err := Load(dir, "dev", "acme")
			if err == nil {
				t.Fatalf("Load() accepted an application with no %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name the missing field %q", err, tc.wantErr)
			}
		})
	}
}

// writeWorkloadCluster declares a workload cluster the way a tenant repository
// does: values.yaml naming the environment it serves, beside the spoke-pool
// claim that tells a workload cluster from the management one.
func writeWorkloadCluster(t *testing.T, repoRoot, name, environment string) {
	t.Helper()
	dir := filepath.Join(repoRoot, "registry", "clusters", name)
	if err := os.MkdirAll(filepath.Join(dir, "infrastructure"), 0o755); err != nil {
		t.Fatal(err)
	}
	values := fmt.Sprintf("clusterName: %s\nenvironmentSlug: %s\n", name, environment)
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(values), 0o644); err != nil {
		t.Fatal(err)
	}
	// spokepool.yaml -- what adjustDriver leaves behind, not the template's
	// per-provider spelling. The fixture named the template and so agreed with
	// a glob that matched nothing real.
	claim := filepath.Join(dir, "infrastructure", "spokepool.yaml")
	if err := os.WriteFile(claim, []byte("kind: SpokePool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Several clusters may serve one environment, and during a replacement two
// deliberately do (ADR-082, ADR-111). What is refused is an application naming a
// cluster that is not a workload cluster serving its own environment.
// Placement is read from the CLUSTERS, not from the application.
//
// This test replaces one that checked an application's clusterName against the
// cluster's environmentSlug. That pair no longer exists: the application names
// no cluster, so the only question is which clusters serve an environment --
// every one of them deploys it, and the management cluster never does.
func TestClustersServingIsEveryWorkloadClusterInTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeWorkloadCluster(t, dir, "nutgraf-01", "dev")
	writeWorkloadCluster(t, dir, "nutgraf-02", "stg")
	writeWorkloadCluster(t, dir, "nutgraf-03", "stg")

	// The management cluster: a declaration with no spoke-pool claim. It must
	// never appear, which is the same exclusion the ApplicationSets make with
	// `spoke-type: pool`.
	mgmt := filepath.Join(dir, "registry", "clusters", "nutgraf-hub")
	if err := os.MkdirAll(mgmt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mgmt, "values.yaml"),
		[]byte("clusterName: nutgraf-hub\nenvironmentSlug: dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for env, want := range map[string][]string{
		"dev":  {"nutgraf-01"},
		"stg":  {"nutgraf-02", "nutgraf-03"},
		"prod": nil,
	} {
		got, err := ClustersServing(dir, env)
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s is served by %v, want %v", env, got, want)
		}
	}

	// An application in an environment with no cluster is still a legal
	// declaration: the definition is independent of any cluster's lifecycle.
	envDir := filepath.Join(dir, "environments", "prod", "acme")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "values.yaml"),
		[]byte("tenantId: acme\nappId: acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "prod", "acme"); err != nil {
		t.Errorf("an application in an environment with no cluster was refused: %v", err)
	}
}

func TestLoadReadsDeclarations(t *testing.T) {
	dir := t.TempDir()
	writeWorkloadCluster(t, dir, "nutgraf-01", "dev")
	envDir := filepath.Join(dir, "environments", "dev", "acme")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
tenantId: acme
clusterName: nutgraf-01
secrets:
  - name: AI_GATEWAY_API_KEY
    capability: "the AI gateway the SDK calls"
    workloads: [sdk]
`
	if err := os.WriteFile(filepath.Join(envDir, "values.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := Load(dir, "dev", "acme")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := f.Declared("AI_GATEWAY_API_KEY")
	if !ok {
		t.Fatal("declared key not found")
	}
	if d.Capability == "" {
		t.Error("capability was not read: it is what a report names, so losing it makes the report unactionable")
	}
	if _, ok := f.Declared("NOT_DECLARED"); ok {
		t.Error("Declared() returned true for a key the application does not declare")
	}
}
