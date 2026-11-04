package health

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/platform"
	"os"
	"path/filepath"
	"strings"
)

// BoundaryInventoryChecker asserts that a boundary generated the Applications it
// declares (ADR-061).
//
// Before ADR-061 a boundary's Applications were literal elements of the
// ApplicationSet, so applying the seed made them exist: there was nothing to
// check. Composing the inventory from descriptors makes it the result of a
// repository read that can return nothing, and a generator that returns nothing
// is not an error — the ApplicationSet reports healthy having produced zero
// Applications, which is indistinguishable from having nothing to produce.
//
// Without this gate the omission surfaces differently in each ADR-055 creation
// mode, and badly in both. Sequenced: the boundary phase reports success and the
// first symptom is a LATER phase timing out on a component whose Application was
// never created, attributing the failure to the component instead of the
// boundary. Converged: nothing is gated at all, so an absent Application is
// indistinguishable from the red-then-green convergence the mode exists to
// permit, and it may never surface.
//
// The expected count is assembled from the two sources the boundary is composed
// of, so it cannot drift from the inventory it describes:
//
//	descriptors on disk      manifests/argocd/components/<NN>/*.yaml
//	inline elements          list-generator elements on the live ApplicationSets
//
// Only STATICALLY KNOWABLE inventory is counted. Boundaries 05 and 06 generate
// from a matrix over the fleet registry, so their Application count is a
// function of how many tenants exist and is legitimately zero on a hub with
// none. Requiring a count there would fail every fresh bootstrap. What is still
// asserted for them is that the boundary rendered at all, and that nothing it
// produced is unable to read its source.
//
// This checker only reads. Day-0 continues to mutate boundary activation and
// nothing else, so ADR-055's disjoint authorities are unaffected.
type BoundaryInventoryChecker struct {
	// Boundary is the two-digit boundary number, e.g. "03".
	Boundary string
	// RepoRoot is the platform repository working tree. Descriptors are counted
	// from it. Empty means the process working directory, which is what
	// scripts/hub-bootstrap.sh provides.
	RepoRoot string
}

// NewBoundaryInventoryChecker constructs a checker for one boundary.
func NewBoundaryInventoryChecker(boundary, repoRoot string) *BoundaryInventoryChecker {
	return &BoundaryInventoryChecker{Boundary: boundary, RepoRoot: repoRoot}
}

// Name returns the checker's identifier.
func (b *BoundaryInventoryChecker) Name() string {
	return fmt.Sprintf("boundary %s inventory", b.Boundary)
}

// project is the AppProject that scopes this boundary.
func (b *BoundaryInventoryChecker) project() string { return "boundary-" + b.Boundary }

// descriptorCount counts the component descriptors declared for this boundary.
//
// A read failure is returned as an error rather than treated as zero. Zero is a
// legitimate value — a boundary may be composed entirely of inline elements —
// and silently conflating "no descriptors" with "could not look" would defeat
// the whole check.
func (b *BoundaryInventoryChecker) descriptorCount() (int, error) {
	root := b.RepoRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return 0, fmt.Errorf("cannot determine working directory: %w", err)
		}
		root = wd
	}

	// Repository-relative unless a caller named a root. The resolver reads an
	// absolute path directly and a relative one from the embedded tree in a
	// released build, so a caller supplying a root still gets that root, and one
	// that does not gets what the binary carries (ADR-063, ADR-068).
	dir := filepath.Join("manifests", "argocd", "components", b.Boundary)
	if root != "" {
		dir = filepath.Join(root, dir)
	}
	entries, err := platform.ReadDir(dir)
	if err != nil && !platform.Exists(dir) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cannot read descriptor directory %s: %w", dir, err)
	}

	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			n++
		}
	}
	return n, nil
}

// appSetSummary is what one ApplicationSet contributes to the inventory.
type appSetSummary struct {
	Name string
	// InlineElements is the number of list-generator elements it declares.
	InlineElements int
	// Dynamic reports a generator whose cardinality is not knowable from the
	// object itself — a matrix over a git repository, or the registered clusters.
	// Boundaries 05 and 06 are built entirely from these.
	Dynamic bool
	// ReadsDescriptors reports a git files generator pointed at this boundary's
	// component directory.
	ReadsDescriptors bool
}

// boundaryAppSets finds the ApplicationSets composing this boundary, by the
// AppProject they template rather than by their name.
//
// Name is NOT a reliable key. Boundaries 01-04 name their ApplicationSet after
// the boundary number, but 05 renders three (tenant-app-xr-provisioning,
// -spoke-provisioning, -workload-provisioning) and 06 renders tenant-public-tls,
// none of which carry the number. A prefix match found nothing for those and
// failed for the full timeout reporting "no ApplicationSet named 05-* exists" —
// which describes the checker's assumption, not the cluster.
//
// The project is what the boundary actually means, it is what the Applications
// are counted by below, and it makes the two sides of the comparison agree by
// construction. It also takes in the -multi variants, which target the same
// project and whose Applications would otherwise be counted against an expected
// figure that never included them.
func (b *BoundaryInventoryChecker) boundaryAppSets(ctx context.Context, kubeconfig string) ([]appSetSummary, error) {
	out, err := runKubectl(ctx, []string{
		"--kubeconfig", kubeconfig,
		"get", "applicationsets.argoproj.io",
		"-n", "platform-ops",
		"-o", "json",
	})
	if err != nil {
		return nil, fmt.Errorf("cannot list ApplicationSets: %w", err)
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Generators []map[string]json.RawMessage `json:"generators"`
				Template   struct {
					Spec struct {
						Project string `json:"project"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("cannot parse ApplicationSets: %w", err)
	}

	descriptorDir := "manifests/argocd/components/" + b.Boundary + "/"
	var found []appSetSummary
	for _, item := range list.Items {
		if item.Spec.Template.Spec.Project != b.project() {
			continue
		}
		sum := appSetSummary{Name: item.Metadata.Name}
		for _, gen := range item.Spec.Generators {
			for kind, raw := range gen {
				switch kind {
				case "list":
					var l struct {
						Elements []json.RawMessage `json:"elements"`
					}
					if err := json.Unmarshal(raw, &l); err == nil {
						sum.InlineElements += len(l.Elements)
					}
				case "git":
					var g struct {
						Files []struct {
							Path string `json:"path"`
						} `json:"files"`
					}
					if err := json.Unmarshal(raw, &g); err == nil {
						for _, file := range g.Files {
							if strings.HasPrefix(file.Path, descriptorDir) {
								sum.ReadsDescriptors = true
							}
						}
					}
				case "matrix", "merge", "clusters", "scmProvider", "pullRequest":
					sum.Dynamic = true
				}
			}
		}
		found = append(found, sum)
	}
	return found, nil
}

// Check performs one poll: it asserts the boundary rendered, and that what it
// rendered is usable.
func (b *BoundaryInventoryChecker) Check(ctx context.Context, kubeconfig string) error {
	appSets, err := b.boundaryAppSets(ctx, kubeconfig)
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name(), err)
	}
	if len(appSets) == 0 {
		return fmt.Errorf(
			"%s: no ApplicationSet targets project %s — the boundary was activated but the seed has not rendered it",
			b.Name(), b.project())
	}

	descriptors, err := b.descriptorCount()
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name(), err)
	}

	// Only count what the ApplicationSets themselves can be asked. A matrix over
	// the fleet registry produces one Application per tenant, so its contribution
	// is unknown here and legitimately zero on a hub with no tenants.
	inline := 0
	dynamic := false
	readsDescriptors := false
	for _, as := range appSets {
		inline += as.InlineElements
		dynamic = dynamic || as.Dynamic
		readsDescriptors = readsDescriptors || as.ReadsDescriptors
	}
	if !readsDescriptors {
		// No generator reads this boundary's component directory, so descriptors
		// on disk are not part of what these ApplicationSets were asked to make.
		descriptors = 0
	}

	apps, err := b.boundaryApps(ctx, kubeconfig)
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name(), err)
	}

	expected := descriptors + inline
	got := len(apps)
	if got < expected {
		return fmt.Errorf(
			"%s: %d of %d Applications generated (%d descriptor(s) + %d inline) — "+
				"the ApplicationSet reports healthy while generating fewer than the boundary declares; "+
				"check that the descriptors are reachable at the revision the generator reads",
			b.Name(), got, expected, descriptors, inline)
	}

	// A boundary that declares nothing AND generated nothing is only acceptable
	// when its inventory is genuinely dynamic. Anywhere else it means the
	// ApplicationSet rendered with an empty generator, which is exactly the
	// silent case this gate exists for.
	if expected == 0 && got == 0 && !dynamic {
		return fmt.Errorf(
			"%s: rendered %d ApplicationSet(s) but they declare no components and generated none — "+
				"a boundary that declares nothing cannot be distinguished from one whose generator returned nothing",
			b.Name(), len(appSets))
	}

	// Counting is not enough. An Application whose target state cannot be
	// generated at all still exists and still reports Healthy, because health is
	// computed over resources it manages and it manages none. Counting alone
	// passed a boundary in which every path-based Application carried
	// "app path does not exist", and the failure surfaced two phases later as a
	// missing operator webhook.
	//
	// ComparisonError is the condition ArgoCD raises when it cannot render the
	// source, so it is the boundary's problem by construction: an unreachable
	// repository, a revision without the descriptors, or a descriptor that does
	// not produce a usable source. A genuinely transient one clears on a later
	// poll, because this runs inside the waiter's retry loop.
	if broken := comparisonErrors(apps); len(broken) > 0 {
		return fmt.Errorf(
			"%s: %d of %d Applications cannot render their source: %s — "+
				"they exist and report Healthy because they manage nothing, so a count alone would pass",
			b.Name(), len(broken), got, strings.Join(broken, "; "))
	}

	return nil
}

// application is the subset of an Argo CD Application this checker reads.
type application struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Project string `json:"project"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// boundaryApps returns the Applications scoped to this boundary's AppProject.
func (b *BoundaryInventoryChecker) boundaryApps(ctx context.Context, kubeconfig string) ([]application, error) {
	out, err := runKubectl(ctx, []string{
		"--kubeconfig", kubeconfig,
		"get", "applications", "-n", "platform-ops",
		"-o", "json",
	})
	if err != nil {
		return nil, fmt.Errorf("cannot list Applications for boundary-%s: %w", b.Boundary, err)
	}

	var list struct {
		Items []application `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("cannot parse Applications: %w", err)
	}

	project := "boundary-" + b.Boundary
	var apps []application
	for _, a := range list.Items {
		if a.Spec.Project == project {
			apps = append(apps, a)
		}
	}
	return apps, nil
}

// comparisonErrors returns "<name>: <message>" for each Application that cannot
// render its source, truncated so one broken boundary does not emit a wall of
// identical messages.
func comparisonErrors(apps []application) []string {
	var out []string
	for _, a := range apps {
		for _, c := range a.Status.Conditions {
			if c.Type != "ComparisonError" {
				continue
			}
			msg := strings.TrimSpace(c.Message)
			if len(msg) > 160 {
				msg = msg[:160] + "…"
			}
			out = append(out, a.Metadata.Name+": "+msg)
			break
		}
	}
	const max = 3
	if len(out) > max {
		out = append(out[:max], fmt.Sprintf("and %d more", len(out)-max))
	}
	return out
}
