package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/health"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Coverage for every declared workload cluster, and the distinction the hub's
// gates depend on: none declared is a topology fact, several means the hub phase
// asserts about none of them because naming one would report partial coverage as
// full.
func TestDeclaredSpokePoolsCoversEveryDeclaration(t *testing.T) {
	ws := t.TempDir()
	pool := func(dir, name string) {
		writeFile(t, filepath.Join(ws, "registry", "clusters", dir, "infrastructure", "spokepool.yaml"),
			"kind: SpokePool\nmetadata:\n  name: "+name+"\n")
	}

	got, err := declaredSpokePools(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a box with no workload cluster declared %v", got)
	}
	// A management cluster is built on its own: a first bootstrap has no
	// workload cluster, and that is a topology fact rather than a failure. The
	// hub's spoke gates are skipped; the bootstrap proceeds.

	pool("wl-b", "pool-b")
	pool("wl-a", "pool-a")
	// Not a SpokePool, so not a workload cluster however its directory looks.
	writeFile(t, filepath.Join(ws, "registry", "clusters", "wl-c", "infrastructure", "spokepool.yaml"),
		"kind: SomethingElse\nmetadata:\n  name: not-a-pool\n")

	got, err = declaredSpokePools(ws)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted, so the order a run reports and rechecks them in does not depend on
	// the filesystem. Every declaration is covered, not the first or a default.
	if strings.Join(got, ",") != "pool-a,pool-b" {
		t.Errorf("declared pools = %v, want [pool-a pool-b]", got)
	}
}

// A bootstrap that reached convergence without a kubeconfig cannot verify, and
// says which path it looked in rather than reporting the cluster unhealthy.
func TestDayZeroKubeconfigNamesThePathItExpected(t *testing.T) {
	t.Setenv("KAMARKAT_HOME", t.TempDir())
	_, err := dayZeroKubeconfig("acme-mgmt")
	if err == nil {
		t.Fatal("a missing kubeconfig was accepted")
	}
	if !strings.Contains(err.Error(), "acme-mgmt.kubeconfig") {
		t.Errorf("the refusal does not name the path: %v", err)
	}

	home := os.Getenv("KAMARKAT_HOME")
	writeFile(t, filepath.Join(home, "run", "kubeconfig", "acme-mgmt.kubeconfig"), "apiVersion: v1\n")
	if _, err := dayZeroKubeconfig("acme-mgmt"); err != nil {
		t.Errorf("Day-0's own kubeconfig was refused: %v", err)
	}
}

// A convergence checker that cannot be satisfied fails, within the budget, and
// says what it was waiting on. Exercised against an unreachable API server, so
// no cluster is required: what is asserted is that an unsatisfied check produces
// a failure rather than a pass, which is the property a bootstrap verdict rests
// on.
func TestConvergenceFailureIsAFailureAndNamesWhatIsOutstanding(t *testing.T) {
	kc := filepath.Join(t.TempDir(), "unreachable.kubeconfig")
	writeFile(t, kc, `apiVersion: v1
kind: Config
clusters:
- name: nowhere
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: nowhere
  context: {cluster: nowhere, user: nobody}
current-context: nowhere
users:
- name: nobody
  user: {}
`)
	var passed []string
	w := &health.HealthWaiter{
		Checkers: health.ConvergedCheckers(),
		Interval: 200 * time.Millisecond,
		Timeout:  2 * time.Second,
		OnCheckPass: func(c health.HealthChecker) {
			passed = append(passed, c.Name())
		},
	}
	start := time.Now()
	err := w.Wait(context.Background(), kc)
	if err == nil {
		t.Fatal("a hub that never converged was reported as converged")
	}
	if len(passed) != 0 {
		t.Errorf("checks reported as passing against an unreachable cluster: %v", passed)
	}
	// The budget is honoured rather than waited out indefinitely: a bootstrap
	// that hung here would hold a billing cluster open with no verdict.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the waiter ran %v past a 2s budget", elapsed)
	}
	if err.Error() == "" {
		t.Error("the failure says nothing about what was outstanding")
	}
	t.Logf("reported: %v", err)
}
