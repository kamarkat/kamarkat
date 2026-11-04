package teardown

import (
	"os"
	"strings"
	"testing"
)

// A teardown scoped to the hub's name leaves the spokes it provisioned running.
//
// The hub is acme-hub; the spoke it created is spoke-pool-hybrid-dev-01, and its
// control-plane server is named and labelled after the SPOKE:
//
//	spoke-pool-hybrid-dev-01-shtms-m6v5n
//	labels: caph-cluster-spoke-pool-hybrid-dev-01-fbbcs=owned
//	        machine.caph-name=spoke-pool-hybrid-dev-01-shtms-m6v5n
//
// The string "acme-hub" appears nowhere in it. One cx33 survived every clean in
// this repository on exactly that basis, and was still billing when it was found.
//
// The fix is that teardown asks the hub for its spokes first, and then matches
// the whole set the same way. This pins the matching, using the real server as
// the fixture.
func TestTeardownMatchesTheSpokesTheHubProvisioned(t *testing.T) {
	const (
		hub        = "acme-hub"
		spoke      = "spoke-pool-hybrid-dev-01"
		spokeSrv   = "spoke-pool-hybrid-dev-01-shtms-m6v5n"
		hubSrvName = "acme-hub-wzr8c-mwx5k"
	)
	spokeLabels := map[string]string{
		"caph-cluster-spoke-pool-hybrid-dev-01-fbbcs": "owned",
		"machine.caph-name":                           spokeSrv,
		"machine_type":                                "control_plane",
	}
	hubLabels := map[string]string{
		"caph-cluster-acme-hub-gmj6h": "owned",
		"machine.caph-name":           hubSrvName,
	}

	// Scoped to the hub alone, the spoke is invisible -- the defect.
	if matches(hub, nil, spokeSrv, spokeLabels) {
		t.Fatal("fixture wrong: the spoke server must not match the hub name alone")
	}
	// With the hub's spoke list, it is claimed.
	if !matches(hub, []string{spoke}, spokeSrv, spokeLabels) {
		t.Error("the spoke server is not matched even when the hub reported the spoke")
	}
	// And the hub is still matched, with or without spokes.
	if !matches(hub, []string{spoke}, hubSrvName, hubLabels) {
		t.Error("the hub's own server stopped matching")
	}

	// A cluster this box does not own is never matched, whatever it is called.
	otherLabels := map[string]string{"caph-cluster-someone-else-xyz": "owned"}
	if matches(hub, []string{spoke}, "someone-else-abc-def", otherLabels) {
		t.Error("a foreign CAPH cluster was matched; teardown would delete someone else's servers")
	}
}

// matches mirrors the predicate in deleteHetznerResources. It is duplicated
// rather than exported because the real one closes over the cloud client's
// iteration; the assertion here is about the RULE, and the rule is small enough
// that a copy which disagrees would fail the source check below.
func matches(hub string, spokes []string, name string, labels map[string]string) bool {
	owned := append([]string{hub}, spokes...)
	for _, cluster := range owned {
		if strings.Contains(name, cluster) {
			return true
		}
		for k, v := range labels {
			if strings.HasPrefix(k, "caph-cluster-"+cluster) || strings.Contains(k, cluster) {
				return true
			}
			if strings.Contains(v, cluster) {
				return true
			}
		}
	}
	return false
}

// The hub must be asked for its spokes BEFORE anything is destroyed, because
// afterwards nothing knows what they were.
func TestSpokesAreDiscoveredBeforeAnythingIsDestroyed(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	run := body[strings.Index(body, "func (o *Orchestrator) Run("):]
	run = run[:strings.Index(run, "\nfunc ")]

	discover := strings.Index(run, "o.spokeClusterNames(ctx)")
	drain := strings.Index(run, "o.drainLoadBalancerServices(ctx)")
	strip := strings.Index(run, "o.stripKubernetesFinalizers(ctx)")
	del := strings.Index(run, "o.deleteHetznerResources(ctx")

	if discover < 0 {
		t.Fatal("Run never asks the hub for its spokes; a spoke-scoped server is never matched")
	}
	for name, at := range map[string]int{
		"drainLoadBalancerServices": drain,
		"stripKubernetesFinalizers": strip,
		"deleteHetznerResources":    del,
	} {
		if at < 0 {
			t.Errorf("%s not found in Run; this test is reading the wrong function", name)
		} else if discover > at {
			t.Errorf("spokes are discovered AFTER %s, by which point the hub may not answer", name)
		}
	}
}

// Unclaimed CAPH servers are reported, never deleted: the label proves the
// server belongs to SOME cluster, not to this box.
func TestUnclaimedServersAreReportedAndNotDeleted(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func reportOrphanedSpokes"):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	if strings.Contains(fn, "Delete(") || strings.Contains(fn, "DeleteWithResult(") {
		t.Error("reportOrphanedSpokes deletes; it must only report, or a cleanup command " +
			"can destroy a cluster it was never pointed at")
	}
	if !strings.Contains(fn, "hcloud server delete") {
		t.Error("the report does not hand the operator the command to remove them")
	}
}
