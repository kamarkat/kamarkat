package tenant

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The CLI carries its own copy of the supported matrix so it can refuse a
// combination before a repository exists. A copy is only safe while something
// fails when the two disagree -- otherwise the CLI accepts what the chart will
// refuse, which is the failure this gate was added to prevent, restored by drift.
func TestScaffoldMatrixMatchesTheChart(t *testing.T) {
	root := repoRootForTest(t)
	raw, err := os.ReadFile(filepath.Join(root,
		"manifests", "argocd", "environment-manager", "values.yaml"))
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	var values struct {
		SupportedProviders []string `yaml:"supportedProviders"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse chart values: %v", err)
	}
	if len(values.SupportedProviders) == 0 {
		t.Fatal("the chart declares no supportedProviders; this test would pass vacuously")
	}

	for _, p := range values.SupportedProviders {
		if !contains(supportedProviders, p) {
			t.Errorf("the chart supports provider %q and the CLI does not; add-cluster would "+
				"refuse a cluster the platform can build", p)
		}
	}
	for _, p := range supportedProviders {
		if !contains(values.SupportedProviders, p) {
			t.Errorf("the CLI supports provider %q and the chart does not; add-cluster would "+
				"accept a cluster built from sources nobody maintains", p)
		}
	}
}

func TestEverySupportedProviderHasAClaimTemplate(t *testing.T) {
	root := repoRootForTest(t)
	for _, p := range supportedProviders {
		claim := filepath.Join(root, "manifests", "tenants", "gitops-template",
			"templates", "workload-cluster", "infrastructure", "spokepool-"+p+".yaml")
		if _, err := os.Stat(claim); err != nil {
			t.Errorf("%s is supported and has no workload-cluster claim template at %s, so "+
				"a cluster declared on it cannot be created: %v", p, claim, err)
		}
	}
}

// Any environment name is accepted, and the provider is what is judged. The
// table that keyed support on dev/stg/prod refused a tenant's qa cluster for
// its name while every reason it carried was about hybrid's topology.
func TestSupportedCombinationJudgesTheProviderAndNotTheName(t *testing.T) {
	for _, env := range []string{"dev", "stg", "prod", "qa", "uat", "sandbox", "e2e-1"} {
		if err := supportedCombination(env, "hetzner", "hetzner"); err != nil {
			t.Errorf("environment %q on hetzner was refused: %v", env, err)
		}
	}

	for _, env := range []string{"", "Dev", "dev_1", "-dev", "dev-", "dev.one"} {
		if err := supportedCombination(env, "hetzner", "hetzner"); err == nil {
			t.Errorf("environment %q is not a usable directory, label and DNS name, and was accepted", env)
		}
	}

	if err := supportedCombination("dev", "aws", "hetzner"); err == nil {
		t.Error("a provider the platform ships no topology for was accepted")
	}

	// hybrid belongs to the box that has the premises.
	if err := supportedCombination("dev", "hybrid", "hybrid"); err != nil {
		t.Errorf("hybrid on a hybrid box was refused: %v", err)
	}
	if err := supportedCombination("dev", "hybrid", "hetzner"); err == nil {
		t.Error("a hybrid cluster was accepted on a cloud box, which has no premises to run it on")
	}
	if err := supportedCombination("prod", "hetzner", "hybrid"); err != nil {
		t.Errorf("a cloud cluster on a hybrid box was refused, and cloud burst is the point: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root")
	return ""
}
