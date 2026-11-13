package tenant

import (
	"os"
	"path/filepath"
	"strings"
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
		SupportedMatrix map[string][]string `yaml:"supportedMatrix"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse chart values: %v", err)
	}
	if len(values.SupportedMatrix) == 0 {
		t.Fatal("the chart declares no supportedMatrix; this test would pass vacuously")
	}

	for env, chartProviders := range values.SupportedMatrix {
		cliProviders, ok := supportedMatrix[env]
		if !ok {
			t.Errorf("the chart supports environment %q and the CLI does not; "+
				"scaffolding would refuse a combination the platform ships", env)
			continue
		}
		for _, p := range chartProviders {
			if !contains(cliProviders, p) {
				t.Errorf("the chart supports %s+%s and the CLI does not", env, p)
			}
		}
		for _, p := range cliProviders {
			if !contains(chartProviders, p) {
				t.Errorf("the CLI accepts %s+%s and the chart refuses it: scaffolding "+
					"would create a repository whose bundle cannot render", env, p)
			}
		}
	}
	for env := range supportedMatrix {
		if _, ok := values.SupportedMatrix[env]; !ok {
			t.Errorf("the CLI accepts environment %q and the chart does not", env)
		}
	}
}

// Every supported provider must have a workload-cluster claim template.
//
// This checked manifests/spoke/spoke-pools/<env>/<provider> -- a SpokePool claim
// in the PLATFORM tree, carrying a literal name, packaged into the published
// bundle. That is what made every box provision its workload cluster under the
// same name and every identity derived from it collide, the CNPG archive prefix
// most damagingly.
//
// The claim is now the tenant's, at clusters/<name>/infrastructure/, hydrated
// from this template by `kakt tenant add-cluster`. So the question the test
// asks is unchanged -- can a supported combination actually be created -- but
// the thing that answers it is the template, and it varies by provider rather
// than by environment: an environment adds no claim of its own.
func TestEverySupportedCombinationHasAClaimTemplate(t *testing.T) {
	root := repoRootForTest(t)
	seen := map[string]bool{}
	for env, providers := range supportedMatrix {
		for _, p := range providers {
			if seen[p] {
				continue
			}
			seen[p] = true
			claim := filepath.Join(root, "manifests", "tenants", "gitops-template",
				"templates", "workload-cluster", "infrastructure", "spokepool-"+p+".yaml")
			if _, err := os.Stat(claim); err != nil {
				t.Errorf("%s+%s is supported but has no workload-cluster claim template "+
					"at %s: `kakt tenant add-cluster --provider %s` would refuse, so the "+
					"combination cannot be created: %v", env, p, claim, p, err)
			}
		}
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

// THE SUPPORT POLICY IS ENFORCED WHERE A WORKLOAD CLUSTER IS DECLARED.
//
// ADR-111 Revision 7 moved this question off the management chart: the hub is one
// shared, environment-independent cluster and declares no workload environment, so
// a render of its bundle cannot answer which environment/provider pairs are
// supported. `supportedCombination` is the only thing that does, and it is asked
// once, when the cluster is declared, before anything is built.
//
// Both halves are asserted. A policy that only accepts is not a policy: the
// release gate used to claim it checked unsupported combinations while checking
// none, and the parity test above cannot notice that -- it compares two copies of
// the same matrix and says nothing about what either refuses.
func TestSupportedCombinationAcceptsTheMatrixAndRefusesTheRest(t *testing.T) {
	accepted := 0
	for env, providers := range supportedMatrix {
		for _, p := range providers {
			if err := supportedCombination(env, p); err != nil {
				t.Errorf("%s+%s is declared supported and was refused: %v", env, p, err)
				continue
			}
			accepted++
		}
	}
	if accepted != 4 {
		t.Errorf("accepted %d supported combination(s), want the four the matrix "+
			"declares (dev+hetzner, dev+hybrid, stg+hetzner, prod+hetzner)", accepted)
	}

	// Named rather than derived. A case list generated from the same matrix
	// would go empty the day the matrix grew, and an empty loop passes.
	for _, tc := range []struct{ env, provider, because string }{
		{"prod", "hybrid", "hybrid is development-only; production runs on hetzner"},
		{"stg", "hybrid", "staging rehearses production, and production is hetzner"},
		{"management", "hetzner", "the hub's own overlay is not a workload environment (ADR-111 Revision 7)"},
		{"ephemeral", "hetzner", "preview environments are not a workload cluster's environment"},
		{"dev", "aws", "there is no such provider"},
		{"", "hetzner", "an absent environment is not a supported one"},
	} {
		err := supportedCombination(tc.env, tc.provider)
		if err == nil {
			t.Errorf("%s+%s was accepted, and %s. A combination with no topology behind it "+
				"builds from sources nobody maintains", tc.env, tc.provider, tc.because)
			continue
		}
		// The refusal has to name the combination it refused: the caller is a
		// person choosing flags, not a log nobody reads.
		if !strings.Contains(err.Error(), tc.env) && tc.env != "" {
			t.Errorf("the refusal of %s+%s does not name the environment: %v", tc.env, tc.provider, err)
		}
	}
}
