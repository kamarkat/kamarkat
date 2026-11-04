package bootstrap

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// The defect this mechanism exists for: a phase is recorded complete, the
// assertions that define "complete" are strengthened afterwards, and every later
// run skips the phase on the strength of a record written before the assertion
// existed. Boundary 04 sat in exactly that state -- recorded done twenty-four
// phases deep, while the identity provider had never once served.
func TestACompletedPhaseWithAFailingPostconditionRunsAgain(t *testing.T) {
	o := &Orchestrator{}
	mgr := state.NewTenantStateManager(t.TempDir(), "acme-hub")
	bs := &state.BootstrapState{
		ClusterName:     "acme-hub",
		CompletedPhases: []state.BootstrapPhase{state.PhaseBoundary04},
	}

	ran := false
	err := o.runPhase(t.Context(), mgr, bs, state.PhaseBoundary04, "boundary04", "",
		func() error { ran = true; return nil },
		nil,
		withPostcondition(func() error { return errors.New("no ready replica") }),
	)
	if err != nil {
		t.Fatalf("re-running the phase failed: %v", err)
	}
	if !ran {
		t.Error("a completed phase whose postcondition does not hold was skipped; " +
			"the completion record was trusted over the state of the cluster")
	}
}

// The complement, and the reason the postcondition must be a cheap probe: when
// it holds, resume stays a no-op. A postcondition that re-did the work would
// make every resume pay for every phase.
func TestACompletedPhaseWithAHoldingPostconditionIsSkipped(t *testing.T) {
	o := &Orchestrator{}
	mgr := state.NewTenantStateManager(t.TempDir(), "acme-hub")
	bs := &state.BootstrapState{
		ClusterName:     "acme-hub",
		CompletedPhases: []state.BootstrapPhase{state.PhaseBoundary04},
	}

	ran := false
	err := o.runPhase(t.Context(), mgr, bs, state.PhaseBoundary04, "boundary04", "",
		func() error { ran = true; return nil },
		nil,
		withPostcondition(func() error { return nil }),
	)
	if err != nil {
		t.Fatalf("skipping the phase failed: %v", err)
	}
	if ran {
		t.Error("a phase whose postcondition holds was run again")
	}
}

// Adoption is per phase. Every other phase passes no postcondition and must keep
// its existing behaviour exactly, or this change is a rewrite of the state
// machine rather than an addition to it.
func TestAPhaseWithoutAPostconditionIsStillSkippedWhenComplete(t *testing.T) {
	o := &Orchestrator{}
	mgr := state.NewTenantStateManager(t.TempDir(), "acme-hub")
	bs := &state.BootstrapState{
		ClusterName:     "acme-hub",
		CompletedPhases: []state.BootstrapPhase{state.PhaseBoundary04},
	}

	ran := false
	if err := o.runPhase(t.Context(), mgr, bs, state.PhaseBoundary04, "boundary04", "",
		func() error { ran = true; return nil }, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ran {
		t.Error("a completed phase with no declared postcondition was re-run")
	}
}

// The outermost claim is the one that shut the door: `complete` was accepted
// without being checked, so the box could not be repaired by the tool that
// built it. Withdrawing `complete` is what lets the ordinary resume path reach
// the phase at all.
func TestWithdrawCompletionClearsCompleteAndTheFailingPhase(t *testing.T) {
	bs := &state.BootstrapState{
		CompletedPhases: []state.BootstrapPhase{
			state.PhaseBoundary03,
			state.PhaseBoundary04,
			state.PhaseBoundary05,
			state.PhaseComplete,
		},
		CurrentPhase: state.PhaseComplete,
	}
	withdrawCompletion(bs, map[state.BootstrapPhase]error{
		state.PhaseBoundary04: errors.New("no ready replica"),
	})

	for _, gone := range []state.BootstrapPhase{state.PhaseComplete, state.PhaseBoundary04} {
		for _, p := range bs.CompletedPhases {
			if p == gone {
				t.Errorf("%s is still recorded complete", gone)
			}
		}
	}
	// Phases that DID hold keep their records. Withdrawing them would re-run
	// hours of work to repair one phase.
	var kept int
	for _, p := range bs.CompletedPhases {
		if p == state.PhaseBoundary03 || p == state.PhaseBoundary05 {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("phases whose postconditions were never in question were withdrawn: %v",
			bs.CompletedPhases)
	}
}

// A phase that was never completed is not a phase whose postcondition failed.
// Reporting one would make a first bootstrap look like a broken one.
func TestUnmetPostconditionsIgnoresPhasesThatNeverRan(t *testing.T) {
	o := &Orchestrator{}
	bs := &state.BootstrapState{CompletedPhases: []state.BootstrapPhase{}}
	if got := o.unmetPostconditions(t.Context(), bs, "/nonexistent-kubeconfig"); len(got) != 0 {
		t.Errorf("a phase that never ran was reported as unmet: %v", got)
	}
}

// Every postcondition must live in the registry, because two readers consult it
// and they must not be able to disagree.
//
// They did. `withPostcondition` at a call site is read by runFresh; the registry
// is read by handleExistingState, which decides whether a bootstrap recorded
// `complete` may be resumed at all. pivot-ready and on-prem-join were declared
// only at their call sites, so a box whose state said `complete` was answered
// "Cluster already bootstrapped. No further CLI operations permitted per ADR-040"
// while the tailnet credential it needed had never been staged -- the phase-level
// check existed and was never reached.
func TestEveryCallSitePostconditionComesFromTheRegistry(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// Each withPostcondition closure must delegate to phasePostconditions.
	for i := 0; ; {
		j := strings.Index(body[i:], "withPostcondition(func() error {")
		if j < 0 {
			break
		}
		start := i + j
		end := strings.Index(body[start:], "}),")
		if end < 0 {
			t.Fatal("unterminated withPostcondition closure")
		}
		closure := body[start : start+end]
		if !strings.Contains(closure, "phasePostconditions") {
			t.Errorf("a withPostcondition closure does not read the registry:\n%s", closure)
		}
		i = start + end
	}
}

// The registry must cover every phase that declares one at a call site.
func TestTheRegistryCoversThePhasesThatDeclarePostconditions(t *testing.T) {
	o := &Orchestrator{}
	reg := o.phasePostconditions("/nonexistent")
	for _, p := range []state.BootstrapPhase{
		state.PhaseBoundary04,
		state.PhasePivotReady,
		state.PhaseOnPremJoin,
	} {
		if _, ok := reg[p]; !ok {
			t.Errorf("phase %s declares a postcondition at its call site but is absent "+
				"from the registry, so handleExistingState cannot check it", p)
		}
	}
}

// The phase and its postcondition must ask the hub exactly one question.
//
// They used to ask two. onPremWorkersPresent matched `workload-location=on-prem`
// and ignored readiness; the phase's own gate matched `hub-role=worker` and
// required Ready=True. A registered-but-NotReady node therefore SATISFIED the
// postcondition and FAILED the phase gate, so runPhase reported
// "Skipped (already completed, postcondition holds)" over a cluster whose only
// worker could schedule nothing -- the ADR-046 §11 failure the postcondition was
// added to catch, walking straight through it.
//
// Pinned by source rather than behaviour because the defect is a second query
// existing at all, which no amount of exercising one of them can show.
func TestOnlyOneProbeAsksTheHubAboutOnPremCapacity(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}

	// Comments quote the selectors they explain, so strip them before searching
	// or this test matches its own rationale.
	var body strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	code := body.String()

	if n := strings.Count(code, `"get", "nodes", "-l"`); n != 1 {
		t.Errorf(`%d node-label queries in orchestrator.go, want exactly 1 (probeHubWorker).
A second one is how the phase and its postcondition came to disagree.`, n)
	}

	// Both labels, together. Either one alone is a weaker claim than the
	// placement rules the platform actually applies.
	for _, label := range []string{"hub-role=worker", "workload-location=on-prem"} {
		if !strings.Contains(hubWorkerSelector, label) {
			t.Errorf("hubWorkerSelector %q does not require %q", hubWorkerSelector, label)
		}
	}
}

// A hub that cannot be asked must never be read as a node that needs rebuilding.
//
// Provisioning deletes the Node object and replaces the VM's disk from the base
// image. readyHubWorker returned a bare false when kubectl itself failed, which
// is indistinguishable from "no such node" -- so one unreachable API server, or a
// kubeconfig naming a cluster that had just been pivoted, destroyed a node that
// was serving and cost ten minutes rebuilding it.
func TestAnUnreadableHubDoesNotTriggerAProvision(t *testing.T) {
	o := &Orchestrator{Provider: NewCloudProvider(
		&HybridDriver{Driver: &HetznerDriver{}, OnPremEnabled: true}, "acme-hub", false)}

	got, name := o.probeHubWorker(t.Context(), "/nonexistent-kubeconfig")
	if got != hubWorkerUnknown {
		t.Errorf("probe of an unreadable hub = %v, want hubWorkerUnknown", got)
	}
	if name != "" {
		t.Errorf("probe of an unreadable hub named %q", name)
	}

	// That the phase refuses is asserted at source, not by calling it: a
	// regression here would shell out to provision-flatcar-worker.sh, and a test
	// that rebuilds a workstation VM to prove it should not have is worse than
	// the bug.
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func (o *Orchestrator) joinHomeWorkers"):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	handled := strings.Index(fn, "case hubWorkerUnknown:")
	provisions := strings.Index(fn, "resolveScriptPath")
	if handled < 0 {
		t.Fatal("joinHomeWorkers does not handle hubWorkerUnknown; an unreadable hub reads as a missing node")
	}
	if provisions < 0 || handled > provisions {
		t.Error("joinHomeWorkers reaches the provisioning script before deciding whether the hub could be read")
	}
	if !strings.Contains(fn[handled:provisions], "return fmt.Errorf") {
		t.Error("the hubWorkerUnknown branch does not refuse; provisioning is destructive and must not run on a guess")
	}
}

// The roles gate must ask the database, not only the operator's memory.
//
// On 2026-09-15 platform-db's node-local volume was destroyed with the worker VM
// and CNPG re-ran initdb against an empty disk. The hub-operator's
// DatabaseRolesProvisioned condition still read True, stamped 10.5 hours earlier
// against a database that no longer existed. This gate passed on it in ONE
// SECOND and printed "✓ database roles provisioned", over a cluster in which
// hub_zitadel did not exist -- and boundary 04 then waited for an identity
// provider that could not authenticate, behind an operator that would never
// re-provision the role because its own record said it already had.
//
// Source-level, because the defect is what the gate is willing to pass on. A
// behavioural test would need a Postgres to be convincing, and the claim here is
// narrower and exact: the condition alone must not be sufficient.
func TestTheRolesGateDoesNotPassOnTheConditionAlone(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func (o *Orchestrator) awaitDatabaseRolesProvisioned"):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	// Strip comments: they quote the condition they explain.
	var code strings.Builder
	for _, line := range strings.Split(fn, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	got := code.String()

	if !strings.Contains(got, "missingDatabaseRoles") {
		t.Error("awaitDatabaseRolesProvisioned never asks the database which roles exist; " +
			"a stale DatabaseRolesProvisioned=True passes it")
	}
	// The success path must require the database's answer too, not just the
	// condition. Both terms have to appear in the same return.
	ret := strings.Index(got, "✓ database roles provisioned")
	if ret < 0 {
		t.Fatal("success path not found")
	}
	guard := got[:ret]
	last := strings.LastIndex(guard, "if ")
	if last < 0 || !strings.Contains(guard[last:], "len(missing) == 0") {
		t.Error("the success path does not require the declared roles to actually exist")
	}
}

// Infrastructure objects must be selected by the cluster they belong to.
//
// hubIngressAddress asked for `items[0]`, which is a position, not a cluster.
// That was right only while the hub was the sole HetznerCluster in platform-capi
// -- and every spoke pool the hub provisions adds one. On acme-hub the hub kept
// winning only because `acme-hub-gmj6h` sorts before
// `spoke-pool-hybrid-dev-01-n5r6z`. The hub Gateway would otherwise publish a
// SPOKE's address for every hub hostname: DNS resolves, the port answers, and it
// is the wrong cluster.
func TestInfrastructureIsSelectedByClusterNotByPosition(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	const sig = "func (o *Orchestrator) hubIngressAddress(ctx context.Context, kubeconfig string) string {"
	at := strings.Index(body, sig)
	if at < 0 {
		t.Fatal("hubIngressAddress not found by its full signature; if it was renamed or " +
			"resignatured, this test must follow it rather than silently match a neighbour")
	}
	fn := body[at:]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	if strings.Contains(fn, "items[0]") {
		t.Error("hubIngressAddress selects a HetznerCluster by position; " +
			"spoke pools share that namespace, so position is not identity")
	}
	if !strings.Contains(fn, "cluster.x-k8s.io/cluster-name") {
		t.Error("hubIngressAddress does not select by the CAPI cluster-name label")
	}
	if !strings.Contains(fn, "o.ClusterName") {
		t.Error("hubIngressAddress does not scope its lookup to THIS cluster")
	}
}

// A handshake must not end a ninety-minute bootstrap.
//
// The hub's API server is reached over the public internet (the kubeconfig names
// the CAPH load balancer), so a single kubectl can fail on the network while the
// cluster is healthy. One did, at boundary 03:
//
//	failed to activate boundary 01: exit status 1
//	Unable to connect to the server: net/http: TLS handshake timeout
//
// with the control plane's four static pods showing zero restarts between them.
//
// The instinct is to raise a timeout. That is the wrong lever: TLSHandshakeTimeout
// is a client-go transport constant with no kubectl flag, and a handshake that has
// not completed in ten seconds did not need eleven. The operation is idempotent by
// construction -- the patch sets syncWindows to null -- so it can simply be retried.
func TestBoundaryActivationRetriesTransportFailures(t *testing.T) {
	for _, out := range []string{
		"Unable to connect to the server: net/http: TLS handshake timeout",
		"Unable to connect to the server: dial tcp 65.109.41.89:6443: connect: connection refused",
		"Unable to connect to the server: read tcp: connection reset by peer",
		"Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded)",
		"etcdserver: request timed out",
		"error: the server was unable to return a response in the time allotted",
	} {
		if !transientTransportError(out) {
			t.Errorf("not retried, so it ends the bootstrap: %q", out)
		}
	}

	// The request being wrong is not the connection being wrong.
	for _, out := range []string{
		`Error from server (NotFound): appprojects.argoproj.io "boundary-01" not found`,
		`Error from server (Forbidden): appprojects.argoproj.io is forbidden`,
		"error: unable to parse ... invalid character",
	} {
		if transientTransportError(out) {
			t.Errorf("retried, turning a clear error into a slow one: %q", out)
		}
	}
}

// The retry is only safe because the patch is idempotent. If that ever stops
// being a null-merge, the retry has to be reconsidered with it.
func TestBoundaryActivationStaysIdempotent(t *testing.T) {
	src, err := os.ReadFile("seed.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func patchWithTransportRetry"):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	if !strings.Contains(fn, `{"spec":{"syncWindows":null}}`) {
		t.Error("the activation patch is no longer a null merge; retrying it may no longer be safe")
	}
	if !strings.Contains(fn, "--type") || !strings.Contains(fn, "merge") {
		t.Error("the activation patch is no longer a merge patch")
	}
}
