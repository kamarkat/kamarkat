package teardown

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrchestratorInit(t *testing.T) {
	orch := &Orchestrator{
		ClusterName: "test-cluster",
		Force:       true,
		Debug:       true,
	}

	if orch.ClusterName != "test-cluster" {
		t.Errorf("expected ClusterName 'test-cluster', got %s", orch.ClusterName)
	}
	if !orch.Force {
		t.Errorf("expected Force to be true")
	}
	if !orch.Debug {
		t.Errorf("expected Debug to be true")
	}
}

// A teardown with no Kubernetes access REFUSES rather than proceeding.
//
// It used to proceed and report success: every kubectl read came back empty, the
// load balancers were never drained, the finalizers never stripped and the DNS
// records never released -- and the last line said the teardown completed. The
// cluster's own cleanup and provider-only destruction are different operations
// with different outcomes, so the command does not become the second because the
// first is unavailable.
func TestRunRefusesWithoutKubernetesAccess(t *testing.T) {
	orch := &Orchestrator{ClusterName: "nonexistent-test-cluster", Force: true}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := orch.Run(ctx)
	if err == nil {
		t.Fatal("a teardown with no kubeconfig was allowed to proceed")
	}
	for _, want := range []string{"no Kubernetes access", "--infrastructure-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// Asking for the recovery path on a box that CAN be read is refused too: the
// full teardown is available, and the recovery path leaves DNS records and load
// balancers behind by construction.
func TestRunRefusesRecoveryModeWhenTheApiIsReachable(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orch := &Orchestrator{
		ClusterName:        "nonexistent-test-cluster",
		Kubeconfig:         kubeconfig,
		InfrastructureOnly: true,
		Force:              true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := orch.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "IS reachable") {
		t.Fatalf("recovery mode was accepted on a readable box: %v", err)
	}
}

// Ownership is the CAPH label, and a box's clusters share a name prefix by
// construction (ADR-082: nutgraf.in gives nutgraf-hub and nutgraf-01). So the
// thing this asserts is that a hub teardown does not claim a workload cluster's
// resources -- which prefix and substring matching both did.
func TestOwnershipIsTheLabelNotTheName(t *testing.T) {
	owned := []string{"nutgraf-hub"}
	ownedByTarget := func(labels map[string]string) bool {
		for _, cluster := range owned {
			if cluster == "" {
				continue
			}
			if _, ok := labels["caph-cluster-"+cluster]; ok {
				return true
			}
		}
		return false
	}

	for _, c := range []struct {
		name   string
		labels map[string]string
		want   bool
		why    string
	}{
		{"nutgraf-hub-control-plane-abc", map[string]string{"caph-cluster-nutgraf-hub": "owned"}, true,
			"the hub's own server, labelled by CAPH"},
		{"nutgraf-01-md-xyz", map[string]string{"caph-cluster-nutgraf-01": "owned"}, false,
			"a workload cluster's server: same prefix, different owner"},
		{"nutgraf-hub-something", nil, false,
			"an unlabelled server whose NAME matches: ownership cannot be established"},
		{"unrelated", map[string]string{"description": "for nutgraf-hub"}, false,
			"a label VALUE mentioning the cluster is not ownership"},
		{"nutgraf-hub-2-cp", map[string]string{"caph-cluster-nutgraf-hub-2": "owned"}, false,
			"another cluster whose name extends ours: an exact key, not a prefix"},
	} {
		if got := ownedByTarget(c.labels); got != c.want {
			t.Errorf("%s: owned=%v, want %v (%s)", c.name, got, c.want, c.why)
		}
	}
}

// A run that could not do everything says so, and says so in its exit status.
//
// Tested through the verdict rather than by executing a teardown: Run's later
// steps delete kind clusters and prune Docker, and a test is not entitled to do
// that to the machine it runs on.
func TestIncompleteCleanupIsReportedAndFails(t *testing.T) {
	o := &Orchestrator{ClusterName: "acme-hub", InfrastructureOnly: true}

	if err := o.result(); err != nil {
		t.Fatalf("a run with nothing outstanding reported failure: %v", err)
	}

	// What an infrastructure-only run cannot do, and a provider step that failed.
	o.recordOutstanding("DNS records were not released: the owner id lives in the cluster", nil)
	o.recordOutstanding("provider infrastructure cleanup", errors.New("403 from the provider"))

	err := o.result()
	if err == nil {
		t.Fatal("a run with outstanding work reported success")
	}
	for _, want := range []string{
		"2 cleanup operation(s)",
		"DNS records were not released",
		"provider infrastructure cleanup: 403 from the provider",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the verdict does not mention %q: %v", want, err)
		}
	}
}
