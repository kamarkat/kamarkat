package bootstrap

import (
	"strings"
	"testing"
)

func noop(*Orchestrator, contextT, string) error { return nil }

// The cycle this platform actually had, as a fixture.
//
//	03 hub-operator ── needs SpokeMachineIdentity CRD ──> 04 spoke-identity-operator
//	04              ── needs the database roles       ──> 03 hub-operator
//
// It was found as a fifteen-minute timeout on a live cluster, two boundaries
// away from its cause. It must be a build-time failure.
func TestTheHistoricalCycleIsRefused(t *testing.T) {
	const capCRD capability = "the SpokeMachineIdentity CRD"

	table := []boundaryContract{
		{1, "one", true, false, nil, requiresNothingBecause("first"), asserts(checkNamed("x", noop))},
		{2, "two", true, false, nil, requiresNothingBecause("none"), asserts(checkNamed("y", noop))},
		{3, "three", true, false, nil,
			requires(check(capCRD, noop)),
			asserts(check(capDatabaseRoles, noop))},
		{4, "four", true, false, nil,
			requires(check(capDatabaseRoles, noop)),
			asserts(check(capCRD, noop))},
	}

	err := validateBoundaryGraph(table)
	if err == nil {
		t.Fatal("the 03 → 04 → 03 cycle was accepted")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("refused, but not as a cycle: %v", err)
	}
	for _, want := range []string{"boundary 3", "boundary 4", string(capCRD), string(capDatabaseRoles)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name %q: %v", want, err)
		}
	}
}

// A number comparison catches the two-node case and misses longer ones. The
// check must be real cycle detection, so it keeps working as boundaries are added.
func TestALongerCycleIsRefused(t *testing.T) {
	const (
		capA capability = "a"
		capB capability = "b"
		capC capability = "c"
	)
	// 3 → 5 → 4 → 3, none of them adjacent.
	table := []boundaryContract{
		{1, "one", true, false, nil, requiresNothingBecause("first"), asserts(checkNamed("x", noop))},
		{2, "two", true, false, nil, requiresNothingBecause("none"), asserts(checkNamed("y", noop))},
		{3, "three", true, false, nil, requires(check(capC, noop)), asserts(check(capA, noop))},
		{4, "four", true, false, nil, requires(check(capB, noop)), asserts(check(capC, noop))},
		{5, "five", true, false, nil, requires(check(capA, noop)), asserts(check(capB, noop))},
	}
	err := validateBoundaryGraph(table)
	if err == nil {
		t.Fatal("a three-hop cycle was accepted; the check is not real cycle detection")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("refused, but not as a cycle: %v", err)
	}
}

// An acyclic but backwards edge is satisfiable in graph terms and not by this
// platform, which runs boundaries in numeric order.
func TestABackwardsDependencyIsRefused(t *testing.T) {
	const capLate capability = "something a later boundary makes"
	table := []boundaryContract{
		{1, "one", true, false, nil, requiresNothingBecause("first"), asserts(checkNamed("x", noop))},
		{2, "two", true, false, nil, requires(check(capLate, noop)), asserts(checkNamed("y", noop))},
		{3, "three", true, false, nil, requiresNothingBecause("none"), asserts(check(capLate, noop))},
	}
	err := validateBoundaryGraph(table)
	if err == nil {
		t.Fatal("boundary 2 requiring what boundary 3 produces was accepted")
	}
	if !strings.Contains(err.Error(), "has not run") {
		t.Errorf("refused, but the reason is unclear: %v", err)
	}
}

// A capability produced outside the boundary sequence is legitimate and must not
// be reported as a missing producer. The database roles are made by the
// hub-operator after phase 11f, not by any boundary.
func TestACapabilityProducedOutsideTheSequenceIsAllowed(t *testing.T) {
	table := []boundaryContract{
		{1, "one", true, false, nil, requiresNothingBecause("first"), asserts(checkNamed("x", noop))},
		{2, "two", true, false, nil, requires(check(capDatabaseRoles, noop)), asserts(checkNamed("y", noop))},
	}
	if err := validateBoundaryGraph(table); err != nil {
		t.Errorf("a capability no boundary produces was refused: %v", err)
	}
}

// The real table must be acyclic and forward-ordered.
func TestTheRealBoundaryGraphIsValid(t *testing.T) {
	if err := validateBoundaryGraph(boundaryContracts); err != nil {
		t.Fatalf("the platform's own boundary graph is invalid: %v", err)
	}
}
