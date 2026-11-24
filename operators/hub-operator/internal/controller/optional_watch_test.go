package controller

import (
	"os"
	"strings"
	"testing"
)

// An optional watch must never gate startup.
//
// Registering a watch on a kind whose CRD is absent makes controller-runtime
// wait for a cache that can never sync, and the manager exits with
// "timed out waiting for cache to be synced". That turned an optional capability
// into a hard prerequisite and deadlocked two boundaries: the SpokeMachineIdentity
// CRD ships in boundary 04, this operator runs in boundary 03, and boundary 04
// does not open until this operator has provisioned the database roles it never
// reached — surfacing two boundaries later as Zitadel unable to authenticate.
func TestCertificateWatchStaysUnconditional(t *testing.T) {
	src, err := os.ReadFile("spokepool_controller.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (r *SpokePoolReconciler) SetupWithManager(")
	if start < 0 {
		t.Fatal("SetupWithManager is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	// Certificate must stay unconditional: cert-manager is boundary 01
	// infrastructure, and marking a genuinely required kind optional hides a
	// broken cluster.
	if strings.Contains(fn, "kindIsInstalled(mgr, certGVK)") {
		t.Error("Certificate has been made optional; cert-manager is a hard " +
			"prerequisite and its absence should be loud")
	}
}
