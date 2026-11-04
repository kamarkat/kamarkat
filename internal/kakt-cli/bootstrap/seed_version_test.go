package bootstrap

import (
	"strings"
	"testing"
)

// The version a cluster records is what it runs, and a reseed must not move it
// by accident: an operator's binary carrying an older bundle would otherwise
// return a promoted cluster to it with no diff, no error, and nothing recording
// what moved it (ADR-064). Moving it is allowed, but only when asked for.
func TestBundleVersionOverrideIsExplicit(t *testing.T) {
	render := func(version string) string {
		return renderSeedApplication("main", "dev", "hybrid", "", "1.2.3.4",
			"letsencrypt-prod", "https://id.dev", "https://id.dev/jwks",
			"proj", "client", version, "https://github.com/example-org/example-gitops", "dev.example.test", nil)
	}

	// What the cluster records reaches the chart unchanged.
	kept := render("0.1.3")
	if !strings.Contains(kept, `value: "0.1.3"`) {
		t.Error("the version the cluster records must reach the chart")
	}
	if !strings.Contains(kept, `targetRevision: "0.1.3"`) {
		t.Error("the seed must resolve the same version it asks the chart for; " +
			"a seed reading one bundle while the chart renders another would " +
			"install a distribution assembled from two versions")
	}

	// A different version produces a different seed, so the override has an
	// effect the caller can see rather than one it has to trust.
	moved := render("0.1.4")
	if kept == moved {
		t.Fatal("a different bundle version must produce a different seed")
	}
	if strings.Contains(moved, "0.1.3") {
		t.Error("no trace of the replaced version may remain in the seed")
	}
}
