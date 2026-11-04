package proposal

import (
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The property the whole package exists for: a check that could not run must
// not read as a check that passed. ADR-067 raises such a proposal as unverified
// and says so, and the failure mode being guarded is a verdict that silently
// degrades to "fine" — indistinguishable, to the tenant merging it, from one
// that actually looked.
func TestUnverifiedNeverCollapsesIntoPass(t *testing.T) {
	cases := []struct {
		name string
		in   []Result
		want Outcome
	}{
		{"all pass", []Result{{Outcome: Pass}, {Outcome: Pass}}, Pass},
		{"one unverified", []Result{{Outcome: Pass}, {Outcome: Unverified}}, Unverified},
		{"one failure", []Result{{Outcome: Pass}, {Outcome: Fail}}, Fail},
		// What was seen was bad; what was not seen cannot make it better.
		{"failure dominates unverified", []Result{{Outcome: Unverified}, {Outcome: Fail}}, Fail},
		{"nothing ran", nil, Pass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := worst(c.in); got != c.want {
				t.Errorf("worst() = %s, want %s", got, c.want)
			}
		})
	}
}

// A cluster below the release's declared floor takes a step the release does
// not claim to support. The floor was disclosed and enforced by nothing.
func TestFloorIsEnforcedNumerically(t *testing.T) {
	cases := []struct {
		from, minimum string
		want          Outcome
	}{
		{"0.1.16", "0.1.14", Pass},
		{"0.1.14", "0.1.14", Pass},
		{"0.1.13", "0.1.14", Fail},
		// String comparison puts 0.1.9 after 0.1.10 and would pass a cluster
		// that is genuinely below the floor.
		{"0.1.9", "0.1.10", Fail},
		{"0.2.0", "0.1.30", Pass},
		{"1.0.0", "0.9.9", Pass},
		// A prerelease is compared on its release numbers, not ordered against
		// them: an -rc is never a floor a release declares.
		{"0.1.16-rc.3", "0.1.16", Pass},
		{"", "0.1.14", Unverified},
		{"0.1.1", "", Pass},
		{"0.1.1", "any", Pass},
	}
	for _, c := range cases {
		got := checkFloor(c.from, "9.9.9", c.minimum)
		if got.Outcome != c.want {
			t.Errorf("checkFloor(from=%q, minimum=%q) = %s (%s), want %s",
				c.from, c.minimum, got.Outcome, got.Detail, c.want)
		}
	}
}

// A failing floor check has to say what to do about it. "Fail" alone sends the
// tenant to the platform, which is the interaction ADR-065 is trying to avoid.
func TestAFailedFloorSaysHowToResolveIt(t *testing.T) {
	r := checkFloor("0.1.9", "0.1.20", "0.1.14")
	if r.Outcome != Fail {
		t.Fatalf("expected a failure, got %s", r.Outcome)
	}
	for _, want := range []string{"0.1.9", "0.1.14", "move to"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("the detail does not mention %q: %s", want, r.Detail)
		}
	}
}

// Reads the repository rather than the cluster, deliberately: ADR-068 makes the
// cluster's own declaration authoritative, and that declaration is bundle.yaml.
func TestDeclaredVersionReadsTheRepositoryDeclaration(t *testing.T) {
	dir := t.TempDir()
	cluster := filepath.Join(dir, tenant.RegistryDir, "clusters", "acme-hub")
	if err := os.MkdirAll(cluster, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shape scaffolding renders, including the second targetRevision that
	// names a branch: a matcher that took the first version-looking value would
	// return the branch.
	bundle := `
    - repoURL: ghcr.io/soloz-io/charts
      chart: environment-manager
      targetRevision: 0.1.16
      helm:
        parameters:
          - name: bundleVersion
            value: "0.1.16"
    - repoURL: https://github.com/acme/acme-gitops
      targetRevision: main
`
	if err := os.WriteFile(filepath.Join(cluster, "bundle.yaml"), []byte(bundle), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := declaredVersion(dir, "acme-hub"); got != "0.1.16" {
		t.Errorf("declaredVersion = %q, want 0.1.16", got)
	}
	// Absent is empty, not an error and not a guess: an unreadable declaration
	// is what makes the floor check unverified rather than passing.
	if got := declaredVersion(dir, "missing-hub"); got != "" {
		t.Errorf("a cluster with no bundle.yaml returned %q", got)
	}
}

// An unreachable registry is not a missing chart. Failing a proposal because
// the network was down would teach tenants to ignore the verdict.
func TestRegistryCheckDistinguishesAbsenceFromUnreachable(t *testing.T) {
	if got := checkCandidateResolves(t.Context(), "", "0.1.16"); got.Outcome != Unverified {
		t.Errorf("no registry should be unverified, got %s", got.Outcome)
	}
}

func TestMarkdownLeadsWithTheAnswer(t *testing.T) {
	v := Verdict{Cluster: "acme-hub", From: "0.1.15", Candidate: "0.1.16",
		Outcome: Fail, Results: []Result{{"floor", Fail, "too old"}}}
	md := v.Markdown()
	if !strings.HasPrefix(md, "## Pre-flight: **do not merge**") {
		t.Errorf("a failing verdict must lead with the answer, got:\n%s", md)
	}
	if !strings.Contains(md, "ADR-067") {
		t.Error("the verdict should say where it was produced and what the platform receives")
	}

	u := Verdict{Cluster: "acme-hub", Candidate: "0.1.16", Outcome: Unverified}
	if !strings.Contains(u.Markdown(), "absence of one") {
		t.Error("unverified must say it is not a judgement that the version is unsafe")
	}
	// From unknown must still render, or a first proposal against a cluster
	// whose declaration cannot be read produces a blank arrow.
	if !strings.Contains(u.Markdown(), "unknown") {
		t.Error("an unreadable current version should render as unknown")
	}
}
