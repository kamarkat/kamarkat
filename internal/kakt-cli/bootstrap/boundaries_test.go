package bootstrap

import (
	"os"
	"strings"
	"testing"
)

// Every boundary the platform activates must answer the contract.
//
// This is the gap the contract exists to close. Before it, what a boundary
// guaranteed was whatever someone had needed to debug: boundaries 02, 05 and 06
// asserted nothing — not as a decision, but because nobody had been burned there
// yet. `awaitBoundaryInventory` was the de-facto contract by being the only
// thing all six called, and it answers "did ArgoCD generate the Applications",
// never "is what this produced usable by the next boundary".
func TestEveryBoundaryDeclaresItsReadiness(t *testing.T) {
	if len(boundaryContracts) == 0 {
		t.Fatal("no boundaries declared")
	}
	for _, c := range boundaryContracts {
		if !c.readiness.declared {
			t.Errorf("boundary %d declares no readiness; use asserts(...) or "+
				"assertsNothingBecause(...)", c.number)
		}
		if len(c.readiness.checks) == 0 && c.readiness.because == "" {
			t.Errorf("boundary %d asserts nothing and gives no reason", c.number)
		}
		if len(c.readiness.checks) > 0 && c.readiness.because != "" {
			t.Errorf("boundary %d both asserts and waives", c.number)
		}
		for _, chk := range c.readiness.checks {
			if chk.what == "" || chk.run == nil {
				t.Errorf("boundary %d has an unnamed or empty check", c.number)
			}
		}
	}
}

// Numbered 1..N with no gaps, so "boundary 5 was never written" is a failure
// rather than a boundary that silently does nothing.
func TestBoundariesAreContiguousAndUnique(t *testing.T) {
	seen := map[int]bool{}
	for _, c := range boundaryContracts {
		if seen[c.number] {
			t.Errorf("boundary %d declared twice", c.number)
		}
		seen[c.number] = true
	}
	for n := 1; n <= len(boundaryContracts); n++ {
		if !seen[n] {
			t.Errorf("boundary %d is not declared", n)
		}
	}
}

// The zero value must not read as "nothing to assert". That equivalence is what
// made the old absence invisible: a boundary that had been thought about and one
// that had not looked identical.
func TestAnUndeclaredReadinessIsNotAnEmptyOne(t *testing.T) {
	var zero readiness
	if zero.declared {
		t.Fatal("the zero readiness claims to be declared")
	}
	if got := asserts(); !got.declared {
		t.Error("asserts() does not mark a readiness declared")
	}
	if got := assertsNothingBecause("because"); !got.declared || got.because == "" {
		t.Error("assertsNothingBecause does not record the reason")
	}
}

// A waiver must argue its case. "Nothing to assert" and "nobody has looked" are
// indistinguishable when both are an absence; they are not when one has to be
// written down where a reviewer reads it.
func TestAWaivedBoundaryGivesASubstantiveReason(t *testing.T) {
	for _, c := range boundaryContracts {
		if len(c.readiness.checks) > 0 {
			continue
		}
		if len(c.readiness.because) < 40 {
			t.Errorf("boundary %d waives readiness with a reason too short to be one: %q",
				c.number, c.readiness.because)
		}
	}
}

// The specific orderings this platform has already paid for.
//
// The database-role requirement belongs to boundary 04, not boundary 03. It was
// put on 03 first, as that boundary's postcondition, and deadlocked: 03 deploys
// the hub-operator, the operator cannot authenticate until phase 11f writes
// infisical-auth, and 11f runs after 03. The boundary waited 15 minutes for work
// that could not start until it returned, with the operator in CrashLoopBackOff
// on "INFISICAL_CLIENT_SECRET is missing".
//
// A guarantee belongs to the boundary that can satisfy it; a requirement belongs
// to the boundary that needs it and can wait for it.
func TestTheDatabaseChainIsGuardedWhereItCanBeSatisfied(t *testing.T) {
	byNumber := map[int]boundaryContract{}
	for _, c := range boundaryContracts {
		byNumber[c.number] = c
	}

	has := func(r readiness, substr string) bool {
		for _, chk := range r.checks {
			if contains(chk.what, substr) {
				return true
			}
		}
		return false
	}

	if !has(byNumber[2].readiness, "database") {
		t.Error("boundary 02 no longer guarantees the database it builds")
	}
	if !has(byNumber[4].requires, "roles") {
		t.Error("boundary 04 no longer requires the database roles; Zitadel's hooks " +
			"would start before hub_zitadel exists and burn their retries on it")
	}
	if has(byNumber[3].readiness, "roles") {
		t.Error("boundary 03 asserts the database roles again. The hub-operator " +
			"cannot create them until phase 11f, which runs after this boundary — " +
			"this is the deadlock that cost a bootstrap run")
	}
	if !has(byNumber[4].readiness, "identity provider") {
		t.Error("boundary 04 no longer guarantees the identity provider serves")
	}
}

// A shared check must not print a boundary tag of its own.
//
// runBoundary prints the tag, because only the runner knows which boundary is
// running the check. A check that names one reports the wrong boundary the
// moment it is declared elsewhere -- awaitDatabaseRolesProvisioned moved from 03
// to 04 and produced "[boundary04] requires the platform's database roles exist"
// immediately followed by "[boundary03] waiting for the platform's database
// roles", from inside the same call.
func TestChecksDoNotPrintTheirOwnBoundaryTag(t *testing.T) {
	for _, file := range []string{"boundaries.go", "orchestrator.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !strings.Contains(line, "fmt.Print") {
				continue
			}
			// runBoundary's own tag is built, not literal.
			if strings.Contains(line, "%s requires") || strings.Contains(line, "%s waiting until") ||
				strings.Contains(line, "%s ✓ %s boundary") || strings.Contains(line, "boundary%02d") {
				continue
			}
			for _, n := range []string{"[boundary01]", "[boundary02]", "[boundary03]",
				"[boundary04]", "[boundary05]", "[boundary06]"} {
				if strings.Contains(line, n) && isSharedCheckLine(file) {
					t.Errorf("%s:%d prints %s from inside a check; the runner owns the tag",
						file, i+1, n)
				}
			}
		}
	}
}

// Only boundaries.go holds checks; orchestrator.go still has phase-level output
// that legitimately names its phase.
func isSharedCheckLine(file string) bool { return file == "boundaries.go" }

// The provider belongs to the cluster, not to the invocation.
//
// A hybrid box has one control plane, zero cloud workers, and expects workers
// from the tenant's own hardware. Resumed as hetzner, `--on-prem` is absent,
// on-prem-join reports "Not a home-worker cell — skipping", and the bootstrap
// carries on toward deploying the platform onto a single tainted control plane
// with nowhere to schedule. The state recorded "hybrid" throughout; nothing
// compared it.
func TestResumingWithADifferentProviderIsRefused(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *Orchestrator) handleExistingState(")
	if start < 0 {
		t.Fatal("handleExistingState is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	if !strings.Contains(fn, "bs.Provider") {
		t.Error("resume does not compare the recorded provider against the one asked " +
			"for; a hybrid cluster resumed as hetzner silently skips on-prem-join")
	}
	if !strings.Contains(fn, "o.providerName()") {
		t.Error("resume does not read the provider this invocation was given")
	}
}

// A repair must not run before the boundary it repairs is open.
//
// ADR-055 gates a boundary with a deny sync window on its AppProject. Its
// Applications EXIST while it is closed -- the ApplicationSet generates them --
// and refuse to sync. A repair that asks one of them to sync before activation
// is answered "Sync prevented by sync window", then waits out its own
// confirmation budget for a sync that was never permitted to begin.
//
// Observed on boundary 04: the Zitadel repair ran first, requested a sync against
// a closed boundary, and the phase stalled with the Application OutOfSync and no
// operation in flight.
func TestRepairsRunAfterActivationNotBefore(t *testing.T) {
	src, err := os.ReadFile("boundaries.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *Orchestrator) runBoundary(")
	if start < 0 {
		t.Fatal("runBoundary is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	activate := strings.Index(fn, "if c.activates {")
	inventory := strings.Index(fn, "awaitBoundaryInventory")
	prepare := strings.Index(fn, "if c.prepare != nil {")
	readiness := strings.Index(fn, "range c.readiness.checks")

	for name, idx := range map[string]int{
		"activation": activate, "inventory": inventory,
		"prepare": prepare, "readiness": readiness,
	} {
		if idx < 0 {
			t.Fatalf("runBoundary no longer has a %s step", name)
		}
	}
	if !(activate < inventory && inventory < prepare && prepare < readiness) {
		t.Errorf("order must be activate -> inventory -> prepare -> readiness; got "+
			"activate=%d inventory=%d prepare=%d readiness=%d", activate, inventory, prepare, readiness)
	}
}

// HubEnvironment is cluster-scoped, so a query for it must not pass -A.
//
// `kubectl get hubenvironment hub-environment -A` exits 0 and prints NOTHING --
// it does not error. The gate therefore polled for its whole 45-minute deadline
// against a condition it could not observe, while that condition had been True
// since minutes after the boundary began, and then reported "the hub-operator did
// not provision the platform's database roles" -- indistinguishable from the real
// fault the gate exists to catch.
func TestClusterScopedQueriesDoNotPassAllNamespaces(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	for _, fnName := range []string{
		"func (o *Orchestrator) awaitDatabaseRolesProvisioned(",
		"func (o *Orchestrator) hubEnvironmentBlocker(",
	} {
		start := strings.Index(body, fnName)
		if start < 0 {
			t.Fatalf("%s is gone", fnName)
		}
		fn := body[start:]
		fn = fn[:strings.Index(fn, "\n}\n")]

		var code strings.Builder
		for _, line := range strings.Split(fn, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			code.WriteString(line)
			code.WriteString("\n")
		}
		c := code.String()

		if strings.Contains(c, `"hubenvironment"`) && strings.Contains(c, `"-A"`) {
			t.Errorf("%s queries the cluster-scoped HubEnvironment with -A; kubectl "+
				"returns empty and exits 0, so the caller sees no condition at all", fnName)
		}
		if strings.Contains(c, ".items[*]") && strings.Contains(c, "hubenvironment") {
			t.Errorf("%s indexes .items[*] on a single cluster-scoped object", fnName)
		}
	}
}

// An absent condition and a stuck operator are different failures and need
// different messages. They shared one, so a malformed query blamed the operator.
func TestAnUnreadableConditionIsReportedAsSuch(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *Orchestrator) awaitDatabaseRolesProvisioned(")
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	if !strings.Contains(fn, "never readable") {
		t.Error("the timeout does not distinguish a condition that was never readable " +
			"from one that never became true")
	}
}

// The completion banner must name a kubeconfig on a resumed run too.
//
// kubeconfigPath is assigned inside the finalize phase's closure, and a resumed
// run skips finalize — so the closure never ran, the variable stayed empty, and
// the banner printed "Kubeconfig:" followed by nothing and offered
// `kubectl --kubeconfig= get nodes`, which is not a command. The state file has
// held the path since the pivot wrote it.
func TestTheCompletionBannerFallsBackToRecordedKubeconfig(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	banner := strings.Index(body, `"\n✓ Hub Cluster bootstrap complete!"`)
	if banner < 0 {
		t.Fatal("the completion banner is gone")
	}
	// The fallback must be established before the banner prints.
	fallback := strings.Index(body, `kubeconfigPath = bs.MgmtKubeconfig`)
	if fallback < 0 {
		t.Fatal("no fallback for kubeconfigPath; a resumed run prints an empty path")
	}
	if fallback > banner {
		t.Error("the fallback is applied after the banner is printed")
	}
}
