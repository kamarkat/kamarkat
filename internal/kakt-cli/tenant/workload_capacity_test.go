package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What counts as capacity differs by provider, so the guard asks each the
// question it can actually answer.
//
// hetzner buys its workers and has no second source: 0 is a cluster that can
// never schedule. hybrid buys none at all (ADR-075) -- its capacity is the
// home-workers list in the claim, which the operator fills in with their own
// hardware -- so 0 here is correct and must NOT be refused.
//
// Conflating the two is how this was first fixed wrongly: a single
// `Workers <= 0` check refused a valid hybrid declaration while passing a
// hetzner one that declared 2 workers the hybrid composition would ignore.
func TestWorkloadClusterCapacityIsProviderSpecific(t *testing.T) {
	for _, c := range []struct {
		name      string
		provider  string
		workers   int
		wantRefus bool
		wantIn    []string
	}{
		{
			name: "hetzner with no workers can never schedule", provider: "hetzner",
			workers: 0, wantRefus: true,
			wantIn: []string{"no capacity", "no on-premises nodes", "--workers >= 1"},
		},
		{
			name: "hetzner with workers is fine", provider: "hetzner",
			workers: 2, wantRefus: false,
		},
		{
			// The case the first version of this guard got wrong.
			name:     "hybrid with zero cloud workers is CORRECT, not an error",
			provider: "hybrid", workers: 0, wantRefus: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// The box runs the provider under test, so the only thing this case
			// can be refused for is capacity.
			writeFixtureBox(t, dir, c.provider)
			w := WorkloadCluster{
				GitopsDir:   dir,
				MgmtCluster: "acme-hub",
				Name:        "acme-01",
				Provider:    c.provider,
				Environment: "dev",
				Workers:     c.workers,
			}
			_, err := w.Add()

			if !c.wantRefus {
				// No template in the temp dir, so Add fails regardless; what
				// matters is that it did not fail on CAPACITY.
				if err != nil && strings.Contains(err.Error(), "no capacity") {
					t.Fatalf("capacity guard fired on a valid %s declaration:\n%v", c.provider, err)
				}
				return
			}

			if err == nil {
				t.Fatal("a cluster that can never schedule was accepted")
			}
			for _, want := range c.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

// writeFixtureBox gives a temporary repository the box declaration every tenant
// repository has. add-cluster reads it for the tenant, the management cluster and
// the provider the box runs on -- the last of which decides whether a hybrid
// cluster may be declared at all -- so a fixture without one is not a repository
// the command would ever meet.
func writeFixtureBox(t *testing.T, dir, provider string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, RegistryDir), 0o755); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	body := "boxId: acme-1\ntenantId: acme\ngitOrg: acme-inc\ngitopsRepo: acme-gitops-1\n" +
		"managementClusterName: acme-hub\ndomain: acme.example\nprovider: " + provider + "\n" +
		"region: hel1\nbundleVersion: 0.0.0\n"
	if err := os.WriteFile(filepath.Join(dir, RegistryDir, BoxDeclFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write box declaration: %v", err)
	}
}
