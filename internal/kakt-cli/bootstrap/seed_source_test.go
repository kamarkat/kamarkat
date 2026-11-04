package bootstrap

import (
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
)

// The seed must read the chart from wherever the rest of the platform is read
// from (ADR-068). A released seed pointing at git asks for a chart that only
// renders when it carries the descriptors copied in at package time, so the
// render fails, no ApplicationSet is created, and the boundary reports that it
// was activated but never rendered.
func TestSeedSourceFollowsBuildMode(t *testing.T) {
	unreleased := renderSeedApplication("main", "dev", "hybrid", "", "1.2.3.4",
		"letsencrypt-prod", "https://auth.example", "https://auth.example/jwks",
		"proj", "client", versions.DevelopmentBundle, "https://github.com/example-org/example-gitops", "dev.example.test", nil)

	if !strings.Contains(unreleased, "path: manifests/argocd/environment-manager") {
		t.Error("an unreleased seed must read the chart from the working tree")
	}
	if strings.Contains(unreleased, "chart: environment-manager") {
		t.Error("an unreleased seed must not name a published chart")
	}

	released := renderSeedApplication("main", "dev", "hybrid", "", "1.2.3.4",
		"letsencrypt-prod", "https://auth.example", "https://auth.example/jwks",
		"proj", "client", "0.1.3", "https://github.com/example-org/example-gitops", "dev.example.test", nil)

	if !strings.Contains(released, "chart: environment-manager") {
		t.Error("a released seed must name the published chart")
	}
	if strings.Contains(released, "path: manifests/argocd/environment-manager") {
		t.Error("a released seed must not carry a git path: ArgoCD rejects a " +
			"source declaring both a chart and a path")
	}
	// ArgoCD pulls a scheme-less registry host as an OCI artefact and passes
	// anything else to `helm pull --repo`, which does not speak OCI. With the
	// scheme present every platform-owned Application failed to load its source.
	if strings.Contains(released, "oci://") {
		t.Error("a released seed must name the registry without an oci:// scheme")
	}
	// Supplied, not defaulted: a published chart carries the registry it was
	// built with, so a cluster running a chart with a defective default could
	// not be repaired by a reseed if the seed did not override it.
	if !strings.Contains(released, "name: bundleRegistry") {
		t.Error("a released seed must supply the registry to the chart")
	}
	if !strings.Contains(released, "targetRevision: \"0.1.3\"") {
		t.Error("a released seed must request the bundle version it was built for")
	}
	if strings.Contains(released, "github.com/kamarkat/kamarkat") {
		t.Error("a released seed must not resolve the platform's repository: " +
			"ADR-063 requires a released cluster to reconcile from the bundle alone")
	}
}
