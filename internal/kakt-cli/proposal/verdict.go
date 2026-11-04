// Package proposal produces the verdict ADR-067 requires a bundle proposal to
// carry.
//
// Not in internal/kakt-cli/preflight: that package validates the machine a
// Day-0 run is launched from -- docker present, token valid, image available.
// This one judges a version against a cluster that already exists. Sharing a
// name would invite one to grow into the other.
//
// ADR-064 proposes a version to a tenant; ADR-065 leaves the decision with the
// tenant; ADR-067 closes the gap between them: "An upgrade proposal is
// accompanied by a pre-flight verdict produced inside the box, against the
// cluster the proposal targets." Nothing did that. A proposal arrived as a
// dependency bump with release notes, and whether the version was safe for THIS
// cluster was a judgement the tenant had no way to make and the platform had no
// standing to make for them -- it holds no access to the cluster and never will
// (ADR-065).
//
// So the judgement is made where the cluster is, and only the verdict crosses
// the boundary. That is the whole shape of this package.
//
// The third outcome matters as much as the other two. A check that cannot run
// returns UNVERIFIED rather than passing: a verdict that silently degrades to
// "fine" when it cannot see anything is worse than no verdict, because it is
// indistinguishable from one that looked.
package proposal

import (
	"context"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
)

// Outcome is the verdict for one check and for the run as a whole.
type Outcome string

const (
	Pass       Outcome = "pass"
	Fail       Outcome = "fail"
	Unverified Outcome = "unverified"
)

// Result is one check's finding. Detail is shown to whoever reads the proposal,
// so it names what was observed rather than restating the check.
type Result struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Detail  string  `json:"detail"`
}

// Verdict is what accompanies the proposal.
type Verdict struct {
	Cluster   string   `json:"cluster"`
	From      string   `json:"from"`
	Candidate string   `json:"candidate"`
	Outcome   Outcome  `json:"outcome"`
	Results   []Result `json:"results"`
}

// Options are what the box knows about the proposal it is judging.
type Options struct {
	Kubeconfig string
	Cluster    string
	GitopsDir  string
	// Candidate is the version the proposal moves to.
	Candidate string
	// Registry is where the candidate would be pulled from. Empty skips the
	// resolution check rather than guessing a registry the box does not use.
	Registry string
	// MinimumFrom is the release's declared floor -- the minimum version the
	// candidate may be taken from. Empty means the release declared none.
	MinimumFrom string
}

// worst collapses the checks into the run's outcome.
//
// Fail dominates Unverified, which dominates Pass. A run with one failure and
// one unreachable check is a failure: the thing that was seen was bad, and what
// was not seen cannot make it better.
func worst(results []Result) Outcome {
	out := Pass
	for _, r := range results {
		switch r.Outcome {
		case Fail:
			return Fail
		case Unverified:
			out = Unverified
		}
	}
	return out
}

// Run produces the verdict. It never returns an error for a check that could not
// run -- that is an Unverified result, which is the point.
func Run(ctx context.Context, o Options) Verdict {
	v := Verdict{Cluster: o.Cluster, Candidate: o.Candidate}
	v.From = declaredVersion(o.GitopsDir, o.Cluster)

	v.Results = append(v.Results,
		checkFloor(v.From, o.Candidate, o.MinimumFrom),
		checkCandidateResolves(ctx, o.Registry, o.Candidate),
	)
	v.Results = append(v.Results, checkClusterConverged(ctx, o.Kubeconfig)...)

	v.Outcome = worst(v.Results)
	return v
}

// declaredVersion reads what the cluster pins today, from the repository the
// proposal is against.
//
// The repository rather than the cluster: ADR-068 makes the cluster's own
// declaration authoritative, and the declaration lives in bundle.yaml. Reading
// it from the running cluster would report what reconciled, which is the same
// value except in exactly the situation a pre-flight exists to notice.
func declaredVersion(gitopsDir, cluster string) string {
	if gitopsDir == "" || cluster == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(gitopsDir, tenant.RegistryDir, "clusters", cluster, "bundle.yaml"))
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`(?m)^\s*-?\s*name:\s+bundleVersion\s*\n\s*value:\s*"?([^"\s]+)"?`).
		FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// checkFloor enforces the release's declared minimum.
//
// A release states "the minimum version this may be taken from" and nothing
// enforced it. A cluster below that floor takes a step the release does not
// claim to support, and finds out by reconciling it.
func checkFloor(from, candidate, minimum string) Result {
	const name = "candidate may be taken from this cluster's version"
	switch {
	case minimum == "" || minimum == "any":
		return Result{name, Pass, "the release declares no minimum"}
	case from == "":
		return Result{name, Unverified,
			"this cluster's bundle.yaml declares no bundleVersion, so the step " +
				"being taken is unknown"}
	case semverLess(from, minimum):
		return Result{name, Fail, fmt.Sprintf(
			"this cluster is on %s and %s may only be taken from %s or later; "+
				"move to %s first", from, candidate, minimum, minimum)}
	default:
		return Result{name, Pass, fmt.Sprintf("%s → %s, floor %s", from, candidate, minimum)}
	}
}

// checkCandidateResolves asks whether the version being proposed is actually
// published where this box would pull it from.
func checkCandidateResolves(ctx context.Context, registry, candidate string) Result {
	const name = "candidate resolves in the registry this box pulls from"
	if registry == "" {
		return Result{name, Unverified,
			"no registry was supplied, and guessing one would verify a source " +
				"this box does not use"}
	}
	if _, err := exec.LookPath("helm"); err != nil {
		return Result{name, Unverified, "helm is not on PATH here"}
	}
	ref := "oci://" + strings.TrimPrefix(registry, "oci://") + "/environment-manager"
	out, err := exec.CommandContext(ctx, "helm", "show", "chart", ref,
		"--version", candidate).CombinedOutput()
	if err == nil {
		return Result{name, Pass, ref + " " + candidate}
	}
	// A clean absence is a real answer; anything else is a question that could
	// not be asked. Treating an auth or network failure as "not published"
	// would fail a proposal for a version that is perfectly fine.
	msg := strings.TrimSpace(string(out))
	if regexp.MustCompile(`(?i): not found|manifest unknown|NAME_UNKNOWN`).MatchString(msg) {
		return Result{name, Fail, candidate + " is not published at " + ref}
	}
	return Result{name, Unverified, "could not reach " + ref + ": " + firstLine(msg)}
}

// checkClusterConverged asks whether the cluster is healthy enough for the
// answer to mean anything.
//
// Sampled once, not waited on. A pre-flight that blocked for fifteen minutes
// would be a hung pull request, and the question here is "is this box in a
// state where taking a version is reasonable", which is answered now or not at
// all.
//
// A degraded cluster does not fail the proposal. It returns Unverified: the
// candidate may be entirely good, and what cannot be said is whether moving is
// safe while the box is already not reconciling.
func checkClusterConverged(ctx context.Context, kubeconfig string) []Result {
	const prefix = "cluster is reconciling: "
	if kubeconfig == "" {
		return []Result{{prefix + "reachable", Unverified,
			"no kubeconfig, so the target was never contacted"}}
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		return []Result{{prefix + "reachable", Unverified,
			"no kubeconfig at " + kubeconfig}}
	}

	var out []Result
	for _, c := range health.ConvergedCheckers() {
		if err := c.Check(ctx, kubeconfig); err != nil {
			out = append(out, Result{prefix + c.Name(), Unverified, firstLine(err.Error())})
			continue
		}
		out = append(out, Result{prefix + c.Name(), Pass, "holds"})
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// semverLess compares two dotted versions numerically, ignoring any prerelease
// suffix.
//
// Numerically because string comparison puts 0.1.9 after 0.1.10, which would
// pass a cluster under a floor it is genuinely below. The prerelease suffix is
// dropped rather than ordered: an -rc is never a floor a release declares, and
// ordering it would add a rule nobody needs to be right about.
func semverLess(a, b string) bool {
	pa, pb := splitSemver(a), splitSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func splitSemver(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				n = 0
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}
