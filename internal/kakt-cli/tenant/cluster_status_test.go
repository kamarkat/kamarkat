package tenant

import (
	"strings"
	"testing"
)

// obs builds an observation of a cluster whose two Applications share a state.
func obs(cluster string, declared bool, apps Presence, claim, capi Presence) Observation {
	return Observation{
		Cluster:  cluster,
		Declared: declared,
		Applications: map[string]Presence{
			cluster + "-infrastructure": apps,
			cluster + "-generated":      apps,
		},
		Claim:       claim,
		CAPICluster: capi,
	}
}

// REMOVAL FROM GIT IS NOT DESTRUCTION, and this is the test that says so.
//
// The commit that deletes the claim returns at once; the cluster takes minutes to
// unwind. Reporting "removed" on the strength of the declaration being gone would
// be the same defect as a teardown that read a missing kubeconfig as "already
// gone" -- and the consequence is the same, servers nobody is looking for.
func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		in    Observation
		want  ClusterState
		cause string
	}{{
		name: "declared and held by the platform",
		in:   obs("acme-01", true, Present, Present, Present),
		want: ClusterDeclared,
	}, {
		name: "claim gone, platform still unwinding",
		in:   obs("acme-01", false, Deleting, Present, Present),
		want: ClusterDeleting,
	}, {
		name: "claim gone, objects gone, provider still holds resources",
		in: func() Observation {
			o := obs("acme-01", false, Absent, Absent, Absent)
			o.ProviderQueried, o.ProviderOwned = true, 3
			return o
		}(),
		want:  ClusterFailed,
		cause: "nothing on the management cluster is left to delete them",
	}, {
		name: "claim gone, objects gone, provider asked and empty",
		in: func() Observation {
			o := obs("acme-01", false, Absent, Absent, Absent)
			o.ProviderQueried = true
			return o
		}(),
		want: ClusterRemoved,
	}, {
		name: "claim gone, objects gone, provider never asked",
		in:   obs("acme-01", false, Absent, Absent, Absent),
		want: ClusterUnknown,
	}, {
		name:  "declared but nothing generated it",
		in:    obs("acme-01", true, Absent, Absent, Absent),
		want:  ClusterFailed,
		cause: "does not exist on the management cluster",
	}, {
		name: "the management cluster could not be asked",
		in:   obs("acme-01", false, Unknown, Unknown, Unknown),
		want: ClusterUnknown,
	}, {
		name:  "declared while an Application is being deleted by hand",
		in:    obs("acme-01", true, Deleting, Present, Present),
		want:  ClusterFailed,
		cause: "will recreate it",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := Classify(c.in)
			if got != c.want {
				t.Errorf("state %q, want %q (%v)", got, c.want, why)
			}
			if c.cause != "" && !strings.Contains(strings.Join(why, "\n"), c.cause) {
				t.Errorf("the reason does not mention %q: %v", c.cause, why)
			}
		})
	}
}

// A removed cluster is reported with what outlives it, because the name is never
// reused and someone has to know why.
func TestRenderRemovedSaysWhatSurvives(t *testing.T) {
	o := obs("acme-01", false, Absent, Absent, Absent)
	o.ProviderQueried = true
	state, why := Classify(o)
	var b strings.Builder
	o.Render(&b, state, why)
	for _, want := range []string{"/spoke-pool/acme-01/", "not reused", "state    removed"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the report does not mention %q:\n%s", want, b.String())
		}
	}
}

// The evidence is printed before the verdict, so the verdict can be disagreed
// with.
func TestRenderPutsEvidenceBeforeTheVerdict(t *testing.T) {
	o := obs("acme-01", true, Present, Present, Present)
	state, why := Classify(o)
	var b strings.Builder
	o.Render(&b, state, why)
	out := b.String()
	if strings.Index(out, "Observed") > strings.Index(out, "state    ") {
		t.Error("the verdict is printed before what was observed")
	}
}

func TestApplicationPresence(t *testing.T) {
	if got := ApplicationPresence(false, ""); got != Absent {
		t.Errorf("a missing Application is %q", got)
	}
	if got := ApplicationPresence(true, ""); got != Present {
		t.Errorf("an Application with no deletion timestamp is %q", got)
	}
	if got := ApplicationPresence(true, "2026-11-09T10:00:00Z"); got != Deleting {
		t.Errorf("an Application being deleted is %q", got)
	}
}
