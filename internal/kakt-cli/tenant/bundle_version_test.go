package tenant

import (
	"bufio"
	"bytes"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/versions"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A managed cluster's template declares no platform chart, and must not start
// declaring one again.
//
// It used to: templates/workload-cluster/bundle.yaml was an Application named
// <cluster>-platform that pulled the environment-manager chart with the managed
// cluster as its destination. Two things were wrong with it, and the second is
// why this test asserts absence rather than correctness.
//
// Nothing ever applied it. It was hydrated into registry/clusters/<cluster>/, and
// no Application covers that path -- the management cluster's root applies its
// OWN directory with recurse: false -- so the object was in git on every box and
// on no cluster.
//
// And it could not have worked where it was aimed. environment-manager renders
// ApplicationSets; a managed cluster runs Argo CD core, which is repo-server and
// application-controller with no ApplicationSet controller, so the objects would
// have landed somewhere nothing expands them. What installs the platform on a
// managed cluster is the hub's own boundary ApplicationSets, selecting it by the
// `spoke-type: pool` label the SpokePool composition writes onto its Argo CD
// cluster Secret.
//
// The management cluster's own bundleVersion contract is asserted by
// TestDevelopmentModeLeavesTheReleasedDeclarationIntact below.
func TestWorkloadClusterTemplateDeclaresNoPlatformChart(t *testing.T) {
	root := repoRootForTest(t)
	dir := filepath.Join(root, "manifests", "tenants", "gitops-template", "templates", "workload-cluster")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var app struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Sources []struct {
					Chart string `yaml:"chart"`
				} `yaml:"sources"`
				Source struct {
					Chart string `yaml:"chart"`
				} `yaml:"source"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(raw, &app); err != nil {
			t.Fatalf("%s: parse: %v", e.Name(), err)
		}
		if app.Kind != "Application" {
			continue
		}
		charts := []string{app.Spec.Source.Chart}
		for _, src := range app.Spec.Sources {
			charts = append(charts, src.Chart)
		}
		for _, chart := range charts {
			if chart != "" {
				t.Errorf("%s declares chart %q: a managed cluster has no ApplicationSet "+
					"controller to render it, and nothing applies an Application left in "+
					"the cluster's own directory", e.Name(), chart)
			}
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
		BundleVersion: "0.1.16", BundleRegistry: "ghcr.io/kamarkat/charts",
		PlatformRepoURL: "https://github.com/kamarkat/kamarkat",
	}

	src := released.chartSource()
	for _, want := range []string{
		"repoURL: ghcr.io/kamarkat/charts", // the registry, scheme-less
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

// THE BRANCH A DEVELOPMENT BOX READS IS NEVER DEFAULTED.
//
// It defaulted to "main", so a box whose branch could not be determined pinned
// a branch that exists and resolves: the declaration was valid, ArgoCD was
// happy, and the cluster reconciled something other than the change under
// test. The detection it depended on read the working directory, which stopped
// being a checkout the day a box had to be initialized in an empty directory.
//
// Refused instead, by Validate, naming the flag that resolves it.
func TestDevelopmentBoxRefusesAnUnknownBranch(t *testing.T) {
	if got := (Spec{}).developmentRevision(); got != "" {
		t.Errorf("developmentRevision() = %q, want empty: nothing may be assumed", got)
	}
	if got := (Spec{PlatformRevision: "wip"}).developmentRevision(); got != "wip" {
		t.Errorf("developmentRevision() = %q, want %q", got, "wip")
	}

	s := testSpec()
	s.BundleVersion = versions.DevelopmentBundle
	s.PlatformRevision = ""
	err := s.Validate()
	if err == nil {
		t.Fatal("a development box with no branch was accepted; it would pin whatever " +
			"the renderer assumed")
	}
	if !strings.Contains(err.Error(), "--platform-revision") {
		t.Errorf("the refusal must name the flag that resolves it, got: %v", err)
	}

	s.PlatformRevision = "wip"
	if err := s.Validate(); err != nil {
		t.Errorf("a development box that names its branch is exactly what this permits: %v", err)
	}
}
