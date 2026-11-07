package spoke

import (
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/teardown"
)

// discoverNames is the label parse discoverSpokeClusters performs, extracted so
// it can be tested without a Hetzner client.
func discoverNames(o *Orchestrator, labelSets []map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, labels := range labelSets {
		for labelKey := range labels {
			name := strings.TrimPrefix(labelKey, "caph-cluster-")
			if name == labelKey || name == "" {
				continue
			}
			if o.MgmtCluster != "" && name == o.MgmtCluster {
				continue
			}
			out[name] = true
		}
	}
	return out
}

// Discovery must find tenant-named workload clusters and must never find the
// management cluster.
//
// It found neither before. The match was on the prefix "caph-cluster-spoke",
// which a tenant-named cluster (nutgraf-01, ADR-082) does not carry, so
// `kakt spoke teardown` discovered nothing and reported success -- servers
// kept running and kept billing. The extraction was wrong too: parts[2:5] of
// caph-cluster-spoke-pool-eu-prod-01-xxxxx is "spoke-pool-eu", not the
// "spoke-pool-eu-prod-01" its comment claimed, so even the old naming produced
// a name matching no cluster.
//
// Widening the match to caph-cluster- is what makes it work, and is also what
// makes the management cluster exclusion load-bearing: CAPH labels its servers
// identically, and ClusterName is optional, so "delete all" would otherwise
// take the one cluster that cannot be rebuilt from what remains.
func TestDiscoveryFindsWorkloadClustersAndNeverTheMgmtCluster(t *testing.T) {
	labels := []map[string]string{
		{"caph-cluster-nutgraf-01": "owned"},  // tenant-named workload cluster
		{"caph-cluster-nutgraf-hub": "owned"}, // the management cluster
		{"caph-cluster-acme-prod-02": "owned"},
		{"unrelated-label": "x"},
	}

	got := discoverNames(&Orchestrator{MgmtCluster: "nutgraf-hub"}, labels)

	for _, want := range []string{"nutgraf-01", "acme-prod-02"} {
		if !got[want] {
			t.Errorf("did not discover %q; a tenant-named workload cluster would "+
				"survive teardown and keep billing", want)
		}
	}
	if got["nutgraf-hub"] {
		t.Error("discovered the MANAGEMENT cluster: teardown would delete the cluster " +
			"holding the CAPI resources every other cluster is defined by")
	}
	if got["unrelated-label"] {
		t.Error("matched a label that is not a CAPH ownership label")
	}
}

// The old naming still has to be found -- a box built before ADR-082 has
// servers labelled with it, and teardown is exactly what runs against those.
func TestDiscoveryStillFindsPreADR082Names(t *testing.T) {
	got := discoverNames(&Orchestrator{MgmtCluster: "acme-hub"}, []map[string]string{
		{"caph-cluster-spoke-pool-hybrid-dev-01": "owned"},
	})
	if !got["spoke-pool-hybrid-dev-01"] {
		t.Errorf("lost the pre-ADR-082 name; got %v", got)
	}
}

// spoke teardown deletes by the same ownership rule as `kakt teardown`, so a
// resource marked `shared`, one belonging to another cluster, or one with no
// ownership label at all is left alone.
//
// This matcher used to match a name prefix, a name substring, a label-key
// substring and a label-value substring, and never looked at the value -- so a
// shared resource other clusters depend on was deleted as readily as an owned
// one, and caph-cluster-nutgraf-01 also admitted nutgraf-01-2's resources.
func TestSpokeDeletionUsesTheSharedOwnershipRule(t *testing.T) {
	matches := func(labels map[string]string) bool {
		got := teardown.Attribute([]teardown.Resource{{Labels: labels}}, []string{"nutgraf-01"})
		return len(got) == 1 && got[0].Eligible
	}

	for _, c := range []struct {
		why    string
		labels map[string]string
		want   bool
	}{
		{"the cluster's own server", map[string]string{"caph-cluster-nutgraf-01": "owned"}, true},
		{"shared with other clusters", map[string]string{"caph-cluster-nutgraf-01": "shared"}, false},
		{"the management cluster's", map[string]string{"caph-cluster-nutgraf-hub": "owned"}, false},
		{"a cluster whose name extends this one's", map[string]string{"caph-cluster-nutgraf-01-2": "owned"}, false},
		{"unlabelled, name alone", nil, false},
	} {
		if got := matches(c.labels); got != c.want {
			t.Errorf("%s: selected=%v, want %v", c.why, got, c.want)
		}
	}
}
