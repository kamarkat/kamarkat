package teardown

import (
	"strings"
	"testing"
)

// Ownership is the exact label key AND the `owned` value. CAPH v1.0.7
// (api/v1beta1/tags.go) defines both lifecycles, and they mean different things:
// `shared` marks a resource other clusters also use.
func TestAttributeRequiresOwnedOnAnExactKey(t *testing.T) {
	const hub = "nutgraf-hub"
	for _, c := range []struct {
		why      string
		labels   map[string]string
		eligible bool
		reason   string
	}{
		{
			why:      "the hub's own server",
			labels:   map[string]string{"caph-cluster-nutgraf-hub": "owned"},
			eligible: true,
		},
		{
			// The case that must never slip: a box's clusters share a name prefix
			// by construction (ADR-082), and differ only in this label.
			why:      "a workload cluster's server",
			labels:   map[string]string{"caph-cluster-nutgraf-01": "owned"},
			eligible: false,
			reason:   "owned by nutgraf-01",
		},
		{
			why:      "shared with another cluster",
			labels:   map[string]string{"caph-cluster-nutgraf-hub": "shared"},
			eligible: false,
			reason:   "shared with other clusters",
		},
		{
			why:      "an unexpected lifecycle value",
			labels:   map[string]string{"caph-cluster-nutgraf-hub": "adopted"},
			eligible: false,
			reason:   `unexpected ownership value "adopted"`,
		},
		{
			why:      "no ownership label at all",
			labels:   map[string]string{"name": "nutgraf-hub-control-plane"},
			eligible: false,
			reason:   "no CAPH ownership label",
		},
		{
			why:      "a cluster whose name extends the target's",
			labels:   map[string]string{"caph-cluster-nutgraf-hub-2": "owned"},
			eligible: false,
			reason:   "owned by nutgraf-hub-2",
		},
	} {
		got := Attribute([]Resource{{Kind: "server", Name: "s", ID: 1, Labels: c.labels}}, []string{hub})
		if len(got) != 1 {
			t.Fatalf("%s: got %d attributions", c.why, len(got))
		}
		if got[0].Eligible != c.eligible {
			t.Errorf("%s: eligible=%v, want %v (reason %q)", c.why, got[0].Eligible, c.eligible, got[0].Reason)
		}
		if c.reason != "" && !strings.Contains(got[0].Reason, c.reason) {
			t.Errorf("%s: reason %q does not mention %q", c.why, got[0].Reason, c.reason)
		}
	}
}

// Tearing down the hub cannot select the workload cluster, and tearing down both
// together is an explicit scope rather than a side effect of naming.
func TestHubAndWorkloadClustersStayIndependent(t *testing.T) {
	resources := []Resource{
		{Kind: "server", Name: "nutgraf-hub-cp-1", ID: 1, Labels: map[string]string{"caph-cluster-nutgraf-hub": "owned"}},
		{Kind: "server", Name: "nutgraf-01-md-1", ID: 2, Labels: map[string]string{"caph-cluster-nutgraf-01": "owned"}},
	}

	hubOnly := Attribute(resources, []string{"nutgraf-hub"})
	if !hubOnly[0].Eligible || hubOnly[1].Eligible {
		t.Fatalf("a hub-only teardown selected %v", eligibleNames(hubOnly))
	}

	both := Attribute(resources, []string{"nutgraf-hub", "nutgraf-01"})
	if !both[0].Eligible || !both[1].Eligible {
		t.Fatalf("an explicit two-cluster scope selected only %v", eligibleNames(both))
	}
}

func eligibleNames(in []Attribution) []string {
	var out []string
	for _, a := range in {
		if a.Eligible {
			out = append(out, a.Name)
		}
	}
	return out
}

// The four states, and the one that used to be inferred wrongly.
func TestClassify(t *testing.T) {
	owned := []Attribution{{Resource: Resource{Kind: "server", Name: "s"}, Eligible: true}}
	for _, c := range []struct {
		why       string
		inv       Inventory
		reachable bool
		want      State
	}{
		{"provider read, servers present, API answers",
			Inventory{Queried: true, Items: owned}, true, StateReachable},
		{"provider read, servers present, API silent",
			Inventory{Queried: true, Items: owned}, false, StateInfrastructureOnly},
		{"provider read, nothing owned",
			Inventory{Queried: true}, false, StateAbsent},
		{
			// The inference that made a teardown destroy nothing and report
			// success: no kubeconfig read as "already gone". An unread inventory
			// is uncertainty, and uncertainty refuses.
			"provider NOT read",
			Inventory{Queried: false}, false, StateUncertain,
		},
		{"provider not read, API answers",
			Inventory{Queried: false}, true, StateUncertain},
	} {
		if got := Classify(c.inv, c.reachable); got != c.want {
			t.Errorf("%s: %q, want %q", c.why, got, c.want)
		}
	}
}

// The operator confirms against the resolved set, so it has to show the
// identities, the label that admits each resource, and every exclusion.
func TestRenderShowsTheResolvedTarget(t *testing.T) {
	inv := Inventory{
		Management: "nutgraf-hub",
		Workload:   []string{"nutgraf-01"},
		Queried:    true,
		Items: Attribute([]Resource{
			{Kind: "server", Name: "nutgraf-hub-cp-1", ID: 42, Labels: map[string]string{"caph-cluster-nutgraf-hub": "owned"}},
			{Kind: "volume", Name: "pvc-abc", ID: 77, Labels: map[string]string{"caph-cluster-nutgraf-hub": "shared"}},
			{Kind: "server", Name: "someone-else", ID: 99, Labels: map[string]string{"caph-cluster-other": "owned"}},
		}, []string{"nutgraf-hub", "nutgraf-01"}),
	}

	var sb strings.Builder
	inv.Render(&sb, Classify(inv, false), []string{"DNS records were not released"})
	out := sb.String()

	for _, want := range []string{
		"nutgraf-hub", // the management cluster, named
		"nutgraf-01",  // the workload cluster in scope
		"id=42",       // immutable ids, not just names
		"caph-cluster-nutgraf-hub=owned",
		"Excluded (2)", // the shared volume and the other box's server
		"shared with other clusters",
		"owned by other",
		"DNS records were not released", // what will not be done
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the target report does not mention %q:\n%s", want, out)
		}
	}
	// A resource belonging to another box must not appear as deletable.
	if strings.Contains(out, "To delete (2)") || strings.Contains(out, "To delete (3)") {
		t.Errorf("the report offers to delete more than the one owned resource:\n%s", out)
	}
}
