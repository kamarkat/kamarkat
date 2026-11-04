package tenant

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A cluster's declaration must tell the chart which version it is, not only which
// version to pull.
//
// targetRevision selects the chart; bundleVersion tells that chart what it is. The
// chart cannot infer it -- its own Chart.version is rewritten at package time and
// its default is "development" -- so a declaration that omits it leaves a released
// cluster in development mode, sourcing every boundary from the platform's git
// repository instead of the distribution it was published in. The failure is not
// loud: the ApplicationSets render, their git generator cannot resolve, and the
// bootstrap stops at "0 of 3 Applications generated".
func TestClusterDeclarationTellsTheChartItsVersion(t *testing.T) {
	root := repoRootForTest(t)
	base := filepath.Join(root, "manifests", "tenants", "gitops-template", "templates")

	// The control plane's chart source is rendered by scaffolding rather than
	// written literally, so its declaration is asserted against the rendered
	// output by TestDevelopmentModeLeavesTheReleasedDeclarationIntact. This checks
	// the template a tenant hydrates by hand, which must stay literal.
	for _, kind := range []string{"workload-cluster"} {
		path := filepath.Join(base, kind, "bundle.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", kind, err)
			continue
		}
		var app struct {
			Spec struct {
				Sources []struct {
					Chart          string `yaml:"chart"`
					TargetRevision string `yaml:"targetRevision"`
					Helm           struct {
						Parameters []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"parameters"`
					} `yaml:"helm"`
				} `yaml:"sources"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(raw, &app); err != nil {
			t.Errorf("%s: parse: %v", kind, err)
			continue
		}

		var found bool
		for _, src := range app.Spec.Sources {
			if src.Chart != "environment-manager" {
				continue
			}
			for _, p := range src.Helm.Parameters {
				if p.Name != "bundleVersion" {
					continue
				}
				found = true
				// The two must be the same token, or a promotion that moves one
				// leaves the cluster claiming a version it is not running.
				if p.Value != src.TargetRevision {
					t.Errorf("%s: bundleVersion is %q but targetRevision is %q; "+
						"they must be the same value", kind, p.Value, src.TargetRevision)
				}
			}
		}
		if !found {
			t.Errorf("%s/bundle.yaml does not pass bundleVersion to the chart, so a "+
				"released cluster would render in development mode", kind)
		}
	}
}

// Renovate must move both in one match. Two managers, or one that sees only
// targetRevision, would promote the chart and leave the version it was told behind.
func TestRenovatePromotesBothVersionsTogether(t *testing.T) {
	root := repoRootForTest(t)
	raw, err := os.ReadFile(filepath.Join(root,
		"manifests", "tenants", "gitops-template", "renovate.json"))
	if err != nil {
		t.Fatalf("read renovate.json: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "bundleVersion") {
		t.Error("renovate.json does not mention bundleVersion, so a promotion would " +
			"move targetRevision and leave the chart believing it is the old version")
	}
}

// The escrow credential is all three or none.
//
// hub-operator reads all four escrow values from one Secret. Three of four produces an operator that attempts a backup on every
// reconcile and fails -- an escrow that appears to exist and does not work, which
// is worse than one that is plainly absent.
func TestEscrowCredentialIsCompleteOrAbsent(t *testing.T) {
	cases := []struct {
		name string
		s    Secrets
		want bool
	}{
		{"all four", Secrets{EscrowURL: "u", EscrowClientID: "i", EscrowClientSecret: "s", EscrowProjectID: "p"}, true},
		{"none", Secrets{}, false},
		{"missing project", Secrets{EscrowURL: "u", EscrowClientID: "i", EscrowClientSecret: "s"}, false},
		{"missing secret", Secrets{EscrowURL: "u", EscrowClientID: "i", EscrowProjectID: "p"}, false},
		{"whitespace only", Secrets{EscrowURL: " ", EscrowClientID: " ", EscrowClientSecret: " ", EscrowProjectID: " "}, false},
	}
	for _, tc := range cases {
		if got := tc.s.hasEscrow(); got != tc.want {
			t.Errorf("%s: hasEscrow() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A box without an escrow must be told what it gives up. It bootstraps and runs
// either way, so nothing else will ever mention it -- and the moment it matters is
// the moment it can no longer be added.
func TestHandoverNamesTheMissingEscrow(t *testing.T) {
	spec := Spec{TenantID: "acme", GitOrg: "acme-inc", Provider: "hetzner", BundleVersion: "0.1.16"}
	complete := Secrets{ProviderToken: "a", GitopsToken: "b"}

	var buf bytes.Buffer
	HandoverInstructions(spec, complete, bufio.NewWriter(&buf))
	if !strings.Contains(buf.String(), "INFISICAL_ESCROW_URL") {
		t.Errorf("a box with no escrow is not told so:\n%s", buf.String())
	}

	buf.Reset()
	withEscrow := complete
	withEscrow.EscrowURL, withEscrow.EscrowClientID = "u", "i"
	withEscrow.EscrowClientSecret, withEscrow.EscrowProjectID = "s", "p"
	HandoverInstructions(spec, withEscrow, bufio.NewWriter(&buf))
	if strings.Contains(buf.String(), "This box has no escrow") {
		t.Errorf("a box that has an escrow is told it does not:\n%s", buf.String())
	}
}

// A box is not dispatched without an escrow (ADR-076).
//
// Required rather than warned about: a box without one behaves identically for
// months, and the difference appears on the day the cluster is gone -- at which
// point the escrow can no longer be added and every secret the platform managed
// for that tenant is unrecoverable.
func TestScaffoldRefusesABoxWithNoEscrow(t *testing.T) {
	none := Secrets{ProviderToken: "a", GitopsToken: "b"}
	err := none.RequireEscrow()
	if err == nil {
		t.Fatal("a box with no escrow was accepted")
	}
	// The refusal has to be actionable, not just correct.
	for _, want := range []string{"app.infisical.com", "--escrow-url", "--escrow-project-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}

	complete := none
	complete.EscrowURL, complete.EscrowClientID = "https://app.infisical.com", "i"
	complete.EscrowClientSecret, complete.EscrowProjectID = "s", "p"
	if err := complete.RequireEscrow(); err != nil {
		t.Errorf("a box with a complete escrow was refused: %v", err)
	}
}

// A released box's declaration must not change because a development mode exists.
//
// The chart source became a rendered block so a development box could name the
// platform's repository instead of the registry (ADR-068). That is a new shape for
// a case tenants never run, and it must leave the case they do run byte-identical
// in every respect the workflow and Renovate depend on.
func TestDevelopmentModeLeavesTheReleasedDeclarationIntact(t *testing.T) {
	released := Spec{
		TenantID: "acme", GitOrg: "acme-inc", ClusterName: "acme-hub",
		BundleVersion: "0.1.16", BundleRegistry: "ghcr.io/soloz-io/charts",
		PlatformRepoURL: "https://github.com/kamarkat/kamarkat",
	}

	src := released.chartSource()
	for _, want := range []string{
		"repoURL: ghcr.io/soloz-io/charts", // the registry, scheme-less
		"chart: environment-manager",       // a chart source, not a path
		"targetRevision: 0.1.16",
		"- name: bundleVersion", // the chart is told what it is
		`value: "0.1.16"`,       // and told the same version it pulls
		"$values/registry/clusters/acme-hub/values.yaml",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the released chart source no longer contains %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "path:") {
		t.Errorf("the released source names a git path, which makes it a git source:\n%s", src)
	}

	// And development is the other shape entirely, not the same one with a
	// different version string.
	dev := released
	dev.BundleVersion = "development"
	dev.PlatformRevision = "some-branch"
	d := dev.chartSource()
	if !strings.Contains(d, "path: manifests/argocd/environment-manager") ||
		!strings.Contains(d, "targetRevision: some-branch") {
		t.Errorf("a development box does not read the platform repository:\n%s", d)
	}
	if strings.Contains(d, "chart: environment-manager") {
		t.Errorf("a development box names a published chart that does not exist:\n%s", d)
	}
	// It must not claim a version either: "development" is the chart's own
	// default, and passing it as a parameter would be asserting a published
	// version by that name.
	if strings.Contains(d, "bundleVersion") {
		t.Errorf("a development box passes a bundleVersion parameter:\n%s", d)
	}
}

// The branch a development box reads defaults to main rather than to empty, which
// would render a declaration ArgoCD cannot resolve.
func TestDevelopmentRevisionFallsBackToMain(t *testing.T) {
	if got := (Spec{}).developmentRevision(); got != "main" {
		t.Errorf("developmentRevision() = %q, want %q", got, "main")
	}
	if got := (Spec{PlatformRevision: "wip"}).developmentRevision(); got != "wip" {
		t.Errorf("developmentRevision() = %q, want %q", got, "wip")
	}
}
