package bootstrap

import (
	"context"
	"fmt"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
	"os/exec"
	"strings"
	"time"
)

// What every Day-0 boundary must answer before the next one may start.
//
// ADR-055 decides when a boundary is ACTIVATED. Nothing decided when one is
// DONE, and the gap was not theoretical: `awaitBoundaryInventory` became the
// de-facto contract by being the only thing all six called, and it answers "did
// ArgoCD generate the Applications" -- never "is what this boundary produced
// usable by the next one".
//
// So each boundary grew its own assertions reactively, one outage at a time.
// Before this table, four of the six assertions in the platform had been added
// after debugging a specific failure, and boundaries 02, 05 and 06 asserted
// nothing at all -- not as a decision, but because nobody had been burned there
// yet. Boundary 03 was the one that cost the most: it reported success when
// ArgoCD had applied the HubEnvironment, while the hub-operator went on to spend
// nine more minutes creating the database roles, and boundary 04 deployed
// Zitadel against a database whose roles did not exist.
//
// # Why this cannot be escaped
//
// The declarations below are UNKEYED struct literals. Go requires every field of
// an unkeyed literal to be present, so:
//
//   - a boundary that omits an answer does not compile;
//   - a field added here breaks every boundary until each one answers it.
//
// The compiler is the enforcement, not review. `readiness` adds the second half:
// its zero value is not a valid declaration, so "I forgot" and "I decided
// nothing is needed" cannot be spelled the same way. Declaring nothing is
// allowed and requires a reason, exactly as the architecture gate's waivers do.
type boundaryContract struct {
	// The boundary's number and the name it prints under.
	number int
	label  string

	// Whether this boundary's ApplicationSet is applied here. Boundary 01 is
	// activated by the seed before the CLI reaches it, so it applies nothing.
	activates bool

	// Whether ArgoCD's schema cache must be refreshed before the boundary's
	// Applications can be diffed. Needed where a boundary is the first to manage
	// a kind established after the controller started.
	refreshesSchema bool

	// Work that must happen after the boundary is open and its Applications
	// exist, but before readiness is asserted. Repairs belong here, not in
	// readiness: readiness asks a question, this changes something.
	//
	// NOT before activation. A closed boundary's Applications cannot sync, so a
	// repair that asks one to would be refused by the sync window and then wait
	// out its own budget -- see runBoundary.
	prepare *namedStep

	// What must be TRUE before THIS boundary may start.
	//
	// The half the contract was missing, and its absence produced a deadlock.
	// Boundary 03 was made to assert "the platform's database roles exist"
	// because boundary 04 needs them -- but the hub-operator that creates them
	// cannot start until `bootstrap-infisical-api` writes the infisical-auth
	// Secret, and that phase runs AFTER boundary 03:
	//
	//   11e  boundary-03               deploys the hub-operator
	//   11f  bootstrap-infisical-api   creates infisical-auth
	//
	// So the boundary waited 15 minutes for work that could not begin until it
	// returned. A guarantee belongs to the boundary that can satisfy it; a
	// requirement belongs to the boundary that needs it. Collapsing the two into
	// one field forces every dependency to be expressed as the producer's
	// postcondition, even when the producer is not the one able to wait.
	requires readiness

	// What must be TRUE before the next boundary may start.
	readiness readiness
}

// A capability is a thing one boundary produces and another may need.
//
// Naming them is what makes the dependency between boundaries a GRAPH rather
// than an ordering convention. Without names, "boundary 04 waits for the
// database roles" and "boundary 03 produces them" are two unrelated strings and
// nothing can tell that they are the same fact -- so nothing can notice when
// they point at each other.
type capability string

const (
	capOperatorsReady    capability = "platform operators and CRDs"
	capBootstrapDatabase capability = "the bootstrap database accepts connections"
	capDatabaseRoles     capability = "the platform's database roles exist"
	capIdentityProvider  capability = "the identity provider serves"
)

// namedStep is a piece of work with a name to print when it runs.
type namedStep struct {
	// The capability this step is about, empty for prepare steps which produce
	// nothing another boundary names.
	capability capability
	what       string
	run        func(*Orchestrator, context.Context, string) error
}

// readiness is a boundary's answer to "what must be true before the next
// boundary starts".
//
// A struct rather than a slice so the zero value is distinguishable from a
// deliberate "nothing". `readiness{}` is not a declaration and is refused;
// asserts(...) and assertsNothingBecause(...) are the only ways to make one.
type readiness struct {
	checks   []namedStep
	because  string
	declared bool
}

// asserts declares the conditions that must hold before the next boundary runs.
func asserts(checks ...namedStep) readiness {
	return readiness{checks: checks, declared: true}
}

// assertsNothingBecause declares that a boundary needs no readiness gate, and
// says why.
//
// A reason is required because the silent version is what this table replaces.
// "Nothing to assert" and "nobody has looked" are indistinguishable when both
// are written as an absence; they are not when one has to be argued in a string
// that a reviewer reads.
func assertsNothingBecause(reason string) readiness {
	return readiness{because: reason, declared: true}
}

// requires declares what must hold before this boundary starts.
func requires(checks ...namedStep) readiness {
	return readiness{checks: checks, declared: true}
}

// requiresNothingBecause declares that a boundary needs nothing in place first.
func requiresNothingBecause(reason string) readiness {
	return readiness{because: reason, declared: true}
}

// checkNamed is a step that verifies something no other boundary names. Used for
// the sub-parts of a capability -- boundary 01's CRDs and pods are both facets of
// capOperatorsReady, and naming each separately would invent capabilities nothing
// requires.
func checkNamed(what string, run func(*Orchestrator, context.Context, string) error) namedStep {
	return namedStep{what: what, run: run}
}

func check(c capability, run func(*Orchestrator, context.Context, string) error) namedStep {
	return namedStep{capability: c, what: string(c), run: run}
}

// The six boundaries, in order.
//
// Unkeyed on purpose -- see boundaryContract. Field order is:
//
//	number, label, activates, refreshesSchema, prepare, readiness
var boundaryContracts = []boundaryContract{
	{
		1, "01-platform-infra", false, false, nil,
		requiresNothingBecause(
			"the first boundary. The seed has activated it and the phases before it " +
				"created the cluster; there is no earlier boundary to depend on."),
		asserts(
			check(capOperatorsReady, func(o *Orchestrator, ctx context.Context, kc string) error {
				return waitForOperators(ctx, kc, o.Provider.OperatorWebhookPatterns())
			}),
			checkNamed("CRDs are queryable", (*Orchestrator).waitForCRDs),
			checkNamed("operator pods are Ready", (*Orchestrator).waitForOperatorPods),
		),
	},
	{
		2, "02-platform-data", true, false, nil,
		requiresNothingBecause(
			"boundary 01 guarantees the operators and CRDs this boundary's objects " +
				"are reconciled by; nothing else is needed to start."),
		// The database this boundary builds is what boundary 03 provisions roles
		// in. Asserting it here is what makes that a sequence rather than a race.
		asserts(
			check(capBootstrapDatabase, (*Orchestrator).awaitBootstrapDatabaseReady),
		),
	},
	{
		3, "03-platform-services", true, true, nil,
		requiresNothingBecause(
			"boundary 02 guarantees the database these services connect to. What " +
				"they then DO with it is gated by phase 11f, not here."),
		// Deliberately NOT "the database roles exist", which was tried here and
		// deadlocked. This boundary deploys the hub-operator; the operator cannot
		// authenticate to Infisical until `bootstrap-infisical-api` (phase 11f)
		// writes the infisical-auth Secret, and that phase runs after this one.
		// Waiting here waits for work that cannot begin until the wait ends --
		// observed as a 15-minute timeout with the operator in CrashLoopBackOff
		// on "INFISICAL_CLIENT_SECRET is missing". The requirement is real and is
		// declared by boundary 04, which is the first boundary able to wait for it.
		assertsNothingBecause(
			"the services this boundary deploys cannot complete their own work " +
				"until phase 11f supplies the Infisical credential they authenticate " +
				"with. Their deployment is what this boundary guarantees; what they " +
				"produce is required by boundary 04 and waited for there."),
	},
	{
		4, "04-tenant-services", true, false,
		&namedStep{"", "clear anything blocking Zitadel's hooks", (*Orchestrator).prepareZitadelForRetry},
		// The gate that boundary 03 could not hold. By the time this boundary
		// starts, phase 11f has run, so the hub-operator has its credential and
		// can do the work this waits for. Zitadel's init/setup hooks authenticate
		// as hub_zitadel on their first attempt; without this they start roughly
		// nine minutes before that role exists, burn their retry budget on
		// "password authentication failed", and die as it becomes usable.
		requires(
			check(capDatabaseRoles, (*Orchestrator).awaitDatabaseRolesProvisioned),
		),
		asserts(
			check(capIdentityProvider, (*Orchestrator).awaitZitadelReady),
		),
	},
	{
		5, "05-tenant-app", true, true, nil,
		requiresNothingBecause(
			"the application's provisioning reads the credentials and CRDs earlier " +
				"boundaries guarantee; nothing further is needed to begin."),
		assertsNothingBecause(
			"spoke provisioning is asynchronous by design (ADR-047): a spoke takes " +
				"longer to build than Day-0 runs, and the fleet converges after " +
				"bootstrap rather than within it. Gating here would make every " +
				"bootstrap wait for machines that the tenant can watch converge."),
	},
	{
		6, "06-tenant-public-tls", true, false, nil,
		requiresNothingBecause(
			"issuance depends on DNS and the ACME issuer, both outside this " +
				"sequence; no earlier boundary gates it."),
		assertsNothingBecause(
			"public certificates are issued by ACME against DNS this box has just " +
				"published, on the issuer's schedule and not the platform's. A box " +
				"serves on its internal PKI meanwhile, so nothing downstream of " +
				"Day-0 waits on them."),
	},
}

// runBoundary executes one boundary's contract.
//
// The single path every boundary takes. A boundary cannot skip its readiness by
// being written differently, because there is no longer anywhere else to write
// it.
func (o *Orchestrator) runBoundary(ctx context.Context, kubeconfig string, c boundaryContract) error {
	tag := fmt.Sprintf("[boundary%02d]", c.number)

	// Preconditions first: before any work, before activation. A boundary that
	// activates and then discovers its dependency missing has already started
	// controllers against it.
	if !c.requires.declared {
		return fmt.Errorf("%s declares no precondition contract", tag)
	}
	// The tag is printed HERE, by the runner, and never inside a check. A check
	// is declared by whichever boundary needs it -- awaitDatabaseRolesProvisioned
	// moved from 03 to 04 -- and a check that names a boundary reports the wrong
	// one the moment it moves. It did: "[boundary04] requires ..." was followed
	// by "[boundary03] waiting for ..." from inside the same call.
	for _, req := range c.requires.checks {
		fmt.Printf("%s requires %s\n", tag, req.what)
		if err := req.run(o, ctx, kubeconfig); err != nil {
			return fmt.Errorf("%s requires %s: %w", tag, req.what, err)
		}
	}

	if c.activates {
		if err := o.deployBoundary(ctx, kubeconfig, c.number); err != nil {
			return err
		}
	}

	if c.refreshesSchema {
		if err := o.refreshArgoCDSchemaCache(ctx, kubeconfig); err != nil {
			return err
		}
	}

	if err := o.awaitBoundaryInventory(ctx, kubeconfig, c.number); err != nil {
		return err
	}

	// Repairs run AFTER the boundary is open, never before.
	//
	// A boundary is gated by a deny sync window on its AppProject (ADR-055), and
	// its Applications exist while it is closed -- generated by the ApplicationSet,
	// refusing to sync. A repair that asks one of them to sync before activation
	// is answered "Sync prevented by sync window", and then waits out its own
	// confirmation budget for a sync that was never permitted to start.
	//
	// That is what this ordering cost: boundary 04's Zitadel repair ran first,
	// requested a sync against a still-closed boundary, and the phase stalled with
	// the Application OutOfSync and no operation in flight.
	//
	// After the inventory rather than merely after activation: a repair acts on
	// Applications, so it should not run until the boundary has actually
	// generated them.
	if c.prepare != nil {
		fmt.Printf("%s %s\n", tag, c.prepare.what)
		if err := c.prepare.run(o, ctx, kubeconfig); err != nil {
			return fmt.Errorf("%s %s: %w", tag, c.prepare.what, err)
		}
	}

	if !c.readiness.declared {
		// Unreachable through the table above, which init() validates. Kept
		// because the whole point of this type is that an undeclared readiness
		// must never be treated as an empty one.
		return fmt.Errorf("%s declares no readiness contract", tag)
	}

	for _, chk := range c.readiness.checks {
		fmt.Printf("%s waiting until %s\n", tag, chk.what)
		if err := chk.run(o, ctx, kubeconfig); err != nil {
			return fmt.Errorf("%s %s: %w", tag, chk.what, err)
		}
	}

	fmt.Printf("%s ✓ %s boundary activated\n", tag, c.label)
	return nil
}

// contractFor returns the declared contract for a boundary number.
func contractFor(number int) (boundaryContract, error) {
	for _, c := range boundaryContracts {
		if c.number == number {
			return c, nil
		}
	}
	return boundaryContract{}, fmt.Errorf("boundary %d has no declared contract", number)
}

// validateBoundaryContracts refuses a table that cannot be honoured.
//
// Separate from init so it can be tested against deliberately bad tables. A rule
// that only ever runs against the one correct table is a rule nobody has checked.
func validateBoundaryContracts(cs []boundaryContract) error {
	seen := map[int]bool{}
	for _, c := range cs {
		switch {
		case c.number < 1:
			return fmt.Errorf("boundary contract with no number")
		case seen[c.number]:
			return fmt.Errorf("boundary %d declared twice", c.number)
		case c.label == "":
			return fmt.Errorf("boundary %d has no label", c.number)
		case !c.requires.declared:
			return fmt.Errorf("boundary %d declares no precondition: use requires(...) "+
				"or requiresNothingBecause(...)", c.number)
		case len(c.requires.checks) == 0 && c.requires.because == "":
			return fmt.Errorf("boundary %d requires nothing and gives no reason", c.number)
		case len(c.requires.checks) > 0 && c.requires.because != "":
			return fmt.Errorf("boundary %d both requires and waives; pick one", c.number)
		case !c.readiness.declared:
			return fmt.Errorf("boundary %d declares no readiness: use asserts(...) "+
				"or assertsNothingBecause(...)", c.number)
		case len(c.readiness.checks) == 0 && c.readiness.because == "":
			return fmt.Errorf("boundary %d asserts nothing and gives no reason", c.number)
		case len(c.readiness.checks) > 0 && c.readiness.because != "":
			return fmt.Errorf("boundary %d both asserts and waives; pick one", c.number)
		}
		seen[c.number] = true
	}
	for n := 1; n <= len(cs); n++ {
		if !seen[n] {
			return fmt.Errorf("boundary %d is not declared; every boundary the "+
				"platform activates must answer this contract", n)
		}
	}
	return nil
}

// init fails the process at start -- and every test run -- rather than letting a
// bad declaration reach a cluster.
func init() {
	if err := validateBoundaryContracts(boundaryContracts); err != nil {
		panic("boundary contracts: " + err.Error())
	}
	if err := validateBoundaryGraph(boundaryContracts); err != nil {
		panic("boundary dependency graph: " + err.Error())
	}
}

// awaitBootstrapDatabaseReady waits until CNPG reports the bootstrap database has
// a ready instance.
//
// Boundary 02 builds this database and boundary 03 provisions roles inside it.
// Neither asserted anything about it, so the sequence was a race that happened
// to be won most of the time -- and when it was not, the failure surfaced two
// boundaries later as Zitadel unable to authenticate.
//
// readyInstances rather than the Cluster merely existing: a CNPG Cluster object
// appears immediately and its first instance takes minutes, so existence is the
// claim that is always true and never useful.
func (o *Orchestrator) awaitBootstrapDatabaseReady(ctx context.Context, kubeconfig string) error {
	const (
		deadline = 45 * time.Minute
		every    = 10 * time.Second
	)
	started := time.Now()
	for {
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"get", "cluster.postgresql.cnpg.io", constants.BootstrapDBCluster,
			"-n", constants.NamespaceData,
			"-o", "jsonpath={.status.readyInstances}").Output()
		if err == nil {
			if ready := strings.TrimSpace(string(out)); ready != "" && ready != "0" {
				fmt.Printf("    ✓ bootstrap database ready after %s\n",
					formatDuration(time.Since(started)))
				return nil
			}
		}
		if time.Since(started) > deadline {
			phase, _ := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
				"get", "cluster.postgresql.cnpg.io", constants.BootstrapDBCluster,
				"-n", constants.NamespaceData,
				"-o", "jsonpath={.status.phase}").Output()
			return fmt.Errorf("%s had no ready instance within %v (CNPG reports %q).\n"+
				"  Boundary 03 provisions this platform's database roles and boundary 04 uses them,\n"+
				"  so nothing past here can work until it serves",
				constants.BootstrapDBCluster, deadline, strings.TrimSpace(string(phase)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
