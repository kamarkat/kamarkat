package bootstrap

import (
	"context"
	"os"
	"strings"
	"testing"
)

type contextT = context.Context

// The signature is the ONLY guard, deliberately.
//
// An earlier version also required boundary 04 to be unfinished. That read as a
// second safety net and was the defect: the phase had been recorded complete by
// a binary that never checked Zitadel served, so the extra condition suppressed
// the repair on precisely the runs that existed to perform it. A guard a stale
// completion record can switch off is not a guard.
//
// Re-adding a phase parameter here is the regression this pins.
func TestResetIsGatedOnTheDatabaseAloneAndNotOnPhaseState(t *testing.T) {
	// Compiles only while the signature is (ctx, kubeconfig). A phase argument
	// would break this line, which is the point of it.
	var _ func(ctx contextT, kubeconfig string) error = (&Orchestrator{}).prepareZitadelForRetry
}

// A sync that has only just begun is a busy Application, not a wedged one.
// Without a threshold this would cancel healthy syncs.
func TestAStaleSyncThresholdExists(t *testing.T) {
	if zitadelSyncStaleAfter <= 0 {
		t.Error("no staleness threshold: every in-flight sync would be cancelled")
	}
}

// No database yet is a first bootstrap, not a failure.
func TestNoDatabaseIsNotAnError(t *testing.T) {
	o := &Orchestrator{}
	if err := o.prepareZitadelForRetry(t.Context(), "/nonexistent-kubeconfig"); err != nil {
		t.Errorf("an unreachable database was treated as an error: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// Clearing the operation and requesting a new one are ONE repair, split around
// the database work. Clearing alone orphans the Application: ArgoCD updates
// status.operationState only while processing an operation read from
// `.operation`, so with that field gone the phase freezes at whatever it last
// said. Observed on a live box -- Terminating for 37 minutes across a controller
// restart, while a sibling Application that still had `.operation` was being
// processed normally.
//
// So prepareZitadelForRetry must not be able to return between the two.
func TestClearIsAlwaysFollowedByASyncRequest(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	clear := strings.Index(body, "o.clearZitadelSync(ctx, kubeconfig)")
	sync := strings.Index(body, "o.requestZitadelSync(ctx, kubeconfig)")
	if clear < 0 || sync < 0 {
		t.Fatal("the repair no longer clears and re-requests the sync")
	}
	if sync < clear {
		t.Error("the sync is requested before the operation is cleared; the new " +
			"operation would be removed by the clear that follows it")
	}
}

// The wait that used to sit inside the clear was the deadlock: it waited for a
// phase that only the un-sent sync request could move, then failed the phase so
// the request was never sent. Re-adding any wait keyed on operationState.phase
// inside the clear re-creates it.
func TestClearDoesNotWaitForThePhaseToSettle(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *Orchestrator) clearZitadelSync(")
	if start < 0 {
		t.Fatal("clearZitadelSync is gone")
	}
	end := strings.Index(body[start:], "\n}\n")
	fn := body[start : start+end]
	for _, banned := range []string{"time.After", "time.Now", "for {", "deadline"} {
		if strings.Contains(fn, banned) {
			t.Errorf("clearZitadelSync waits (%q): the phase it would wait on cannot "+
				"move until the sync is requested, which is what the wait prevents", banned)
		}
	}
}

// The clear must succeed whether or not an operation is in flight, and the
// mechanism -- not an error-message match -- is what guarantees it.
//
// JSON-patch `remove` on an absent path is an error whose text names neither the
// path nor the reason ("The request is invalid: the server rejected our request
// due to an error in our request"). A version that used it and tried to classify
// that prose matched the wrong strings and failed boundary 04 on the ORDINARY
// case: an Application with nothing in flight, which is what most resets meet.
//
// A merge patch setting the field to null has no absent case to classify.
func TestClearUsesAnIdempotentPatchNotErrorMatching(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "func (o *Orchestrator) clearZitadelSync(")
	if start < 0 {
		t.Fatal("clearZitadelSync is gone")
	}
	fn := string(src)[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	// Code only. The comment above this function quotes the API server's error
	// prose in order to explain why classifying it was wrong, and a check that
	// scanned comments would fire on the explanation rather than the behaviour.
	var code strings.Builder
	for _, line := range strings.Split(fn, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	fn = code.String()

	if !strings.Contains(fn, `{"operation":null}`) || !strings.Contains(fn, `"merge"`) {
		t.Error("the clear no longer uses a merge patch setting operation to null, " +
			"which is what makes it safe to run when nothing is in flight")
	}
	if strings.Contains(fn, `"op":"remove"`) {
		t.Error("JSON-patch remove is back: it errors when no operation is in " +
			"flight, which is the common case")
	}
	for _, prose := range []string{"missing value", "not found", "request is invalid"} {
		if strings.Contains(fn, prose) {
			t.Errorf("the clear classifies the API server's error prose (%q); "+
				"that is what broke it before -- use an idempotent patch instead", prose)
		}
	}
}

// Requesting a sync is not the same as a sync starting.
//
// When a stale non-terminal operation is recorded, ArgoCD spends the request
// settling THAT operation -- writing the terminal phase it never reached -- and
// clears `.operation` without running anything. The observed transition was
// Terminating -> Failed with startedAt unchanged at 14:46:13 and no hook run.
// Nothing else supplies the missing sync: automated sync does not retry a failed
// sync on the same revision, so the Application sits OutOfSync with selfHeal on
// and never moves.
//
// startedAt changing is the only signal that separates the two outcomes.
func TestTheSyncRequestConfirmsANewOperationBegan(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *Orchestrator) requestZitadelSync(")
	if start < 0 {
		t.Fatal("requestZitadelSync is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	if !strings.Contains(fn, "awaitNewZitadelOperation") {
		t.Error("the sync request no longer confirms a new operation began; a request " +
			"spent settling a stale operation would be reported as a successful sync")
	}
	if !strings.Contains(fn, "for attempt") {
		t.Error("the sync request is not retried; the first request is consumed by the " +
			"stale operation, so a single one can never start a sync on a wedged app")
	}
}

// The retry must be bounded. Two consumed requests mean something other than a
// stale operation is refusing them, and an unbounded loop would hide that behind
// a hang.
func TestTheSyncRequestRetryIsBounded(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "const attempts = ") {
		t.Error("no bound on the sync-request retry")
	}
}

// The confirmation waits for the controller to NOTICE the request, never for the
// sync to finish. awaitZitadelReady owns the outcome and owns reporting it; a
// wait here that outlived that would double the phase's budget and report the
// worse error of the two.
func TestTheConfirmationDoesNotWaitForTheSyncToFinish(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (o *Orchestrator) awaitNewZitadelOperation(")
	if start < 0 {
		t.Fatal("awaitNewZitadelOperation is gone")
	}
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]
	if strings.Contains(fn, "time.Minute") && !strings.Contains(fn, "90 * time.Second") {
		t.Error("the confirmation waits in minutes; it should only wait for the " +
			"controller to pick the request up")
	}
}

// The third condition, and the one the first design missed.
//
// An Application can need a sync with nothing stuck and nothing corrupt: a sync
// failed once, the operation reached a terminal phase, and automated sync does
// not retry a failed sync on the same revision. Observed exactly -- operation
// Failed, database clean, OutOfSync, no operation in flight -- while both damage
// probes correctly reported nothing to repair and the identity provider had
// never started.
//
// The error was modelling what is BROKEN when the thing that makes progress is
// the same in every case. The damage probes decide preparatory work; being
// out of sync is on its own enough to act.
func TestBeingOutOfSyncIsEnoughToTriggerTheRepair(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "func (o *Orchestrator) zitadelOutOfSync(") {
		t.Fatal("there is no out-of-sync probe; an Application parked OutOfSync " +
			"with a terminal failed operation would never be repaired")
	}

	start := strings.Index(body, "func (o *Orchestrator) prepareZitadelForRetry(")
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]
	if !strings.Contains(fn, "outOfSync") {
		t.Error("the repair does not consider whether the Application is out of sync")
	}
	if !strings.Contains(fn, "!stuckSync && !outOfSync && !schemaMissing") {
		t.Error("the early return does not account for every condition, so one " +
			"of them cannot trigger the repair on its own")
	}
}

// A first bootstrap reaches this code before boundary 04 has ever been applied:
// nothing is serving because nothing exists yet. Requesting a sync for an
// Application that is not there is meaningless, and treating it as damage would
// put the repair on the happy path of every clean install.
func TestAFirstBootstrapIsNotTreatedAsDamage(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "func (o *Orchestrator) zitadelApplicationExists(") {
		t.Fatal("nothing distinguishes a first bootstrap from a broken box")
	}
	start := strings.Index(body, "func (o *Orchestrator) prepareZitadelForRetry(")
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	exists := strings.Index(fn, "zitadelApplicationExists")
	probe := strings.Index(fn, "zitadelSyncIsStuck")
	if exists < 0 || probe < 0 || exists > probe {
		t.Error("the existence check must come before the damage probes, or a first " +
			"bootstrap is diagnosed against an Application that does not exist")
	}
}

// Deleting a hook Job is prohibited, and the prohibition is the test.
//
// It was done once, to force ArgoCD to re-run zitadel-init after the database
// was recreated, justified as safe "because the caller has already cleared any
// in-flight operation". Clearing does not stop an operation the controller has
// already taken, so the Application wedged on
// `waiting for completion of hook batch/Job/zitadel-init` against a Job that no
// longer existed -- the second time in one session, by the same mechanism.
//
// There is no window in which a hook Job has no reader.
func TestTheRepairNeverDeletesAHookJob(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	var code strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	body := code.String()
	if strings.Contains(body, `"delete", "job"`) || strings.Contains(body, "deleteZitadelHookJobs") {
		t.Error("the repair deletes a hook Job. An in-flight operation waiting on it " +
			"deadlocks permanently, and clearing .operation does not stop that " +
			"operation -- see the note in zitadel_reset.go")
	}
}

// Nothing in this repair may drop, create or truncate a database.
//
// It once did, gated on `eventstore.events2 IS NOT NULL AND
// projections.migrations IS NULL` and justified as "a Zitadel that initialised
// properly HAS projections.migrations and therefore cannot match it".
//
// Zitadel v4.15.3 has no projections.migrations table -- migration state lives
// in the eventstore -- so the condition held on EVERY healthy database. Verified
// against a running, serving box: the query returned true, meaning the repair
// would have dropped a live identity provider's data. The only thing preventing
// it was the "is it serving" gate, which a brief restart satisfies.
//
// Every recovery this platform has actually needed came from hook mechanics, not
// from recreating the database.
func TestTheRepairIsNotDestructive(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	var code strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	body := code.String()
	for _, banned := range []string{"DROP DATABASE", "CREATE DATABASE", "TRUNCATE",
		"pg_terminate_backend", "DROP SCHEMA"} {
		if strings.Contains(body, banned) {
			t.Errorf("the repair performs %q. Recreating Zitadel's database has never "+
				"fixed anything here, and the signature that once guarded it matched "+
				"every healthy box", banned)
		}
	}
}

// A sync that is currently running must be waited for, not repaired.
//
// OutOfSync is the normal state of an Application mid-sync, so it cannot on its
// own mean "nothing is happening". The repair fired against a sync that had
// started 20 seconds earlier and was correctly running Zitadel's PreSync hooks;
// each of its three sync requests found startedAt unchanged -- the same
// operation was still going -- reported "settled the previous operation rather
// than starting a sync", and failed the phase over work that was succeeding.
// zitadel-init completed moments later.
func TestARunningSyncIsLeftAlone(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if !strings.Contains(body, "func (o *Orchestrator) zitadelSyncInFlight(") {
		t.Fatal("nothing distinguishes a running sync from an absent one")
	}

	start := strings.Index(body, "func (o *Orchestrator) prepareZitadelForRetry(")
	fn := body[start:]
	fn = fn[:strings.Index(fn, "\n}\n")]

	if !strings.Contains(fn, "zitadelSyncInFlight") {
		t.Error("the repair does not check whether a sync is already running; it will " +
			"interrupt healthy syncs whenever the Application is OutOfSync")
	}
	// In flight AND stale is a wedge and must still be repaired; in flight and
	// fresh must not.
	if !strings.Contains(fn, "!o.zitadelSyncIsStuck") {
		t.Error("the in-flight check does not defer to the staleness threshold, so a " +
			"genuinely wedged operation would now be left alone too")
	}
}

// The repair must judge itself on the schema existing, not on who created it.
//
// On 2026-09-15 the schema was genuinely missing when boundary 04 opened, so the
// repair set about requesting a sync. While it did, ArgoCD's own sync (started
// 11:45:05, Succeeded 11:49:14) ran zitadel-init and zitadel-setup and built the
// schema. Every request the repair made was spent settling that operation, so
// startedAt never changed, so after three attempts it failed the boundary and
// ended the bootstrap -- over a Zitadel that was 1/1 Running with 8 eventstore
// tables and its hub_zitadel role in place.
//
// requestZitadelSync must therefore re-check zitadelSchemaMissing before giving
// up, and return nil when the goal has been met by other means.
func TestTheSyncRequestGivesUpOnlyIfTheSchemaIsStillMissing(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func (o *Orchestrator) requestZitadelSync"):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	// Strip comments: they quote the very symbols being searched for.
	var code strings.Builder
	for _, line := range strings.Split(fn, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	got := code.String()

	if !strings.Contains(got, "zitadelSchemaMissing") {
		t.Fatal("requestZitadelSync never re-checks the schema; it fails the boundary " +
			"whenever something else supplied the sync")
	}

	// The GIVE-UP error is the last one in the function; the first is the
	// unrelated "could not request a sync" path, and matching that made this
	// test read the wrong guard.
	giveUp := strings.LastIndex(got, "return fmt.Errorf(")
	if giveUp < 0 {
		t.Fatal("give-up path not found")
	}
	guard := strings.LastIndex(got[:giveUp], "zitadelSchemaMissing")
	if guard < 0 {
		t.Fatal("the give-up path is not guarded by a final schema check")
	}
	if !strings.Contains(got[guard:giveUp], "return nil") {
		t.Error("the final schema check does not return success when the schema exists")
	}
}

// The failure message must name the real condition, so the next reader is not
// sent after the sync mechanism when the schema is what matters.
func TestTheGiveUpMessageNamesTheSchema(t *testing.T) {
	src, err := os.ReadFile("zitadel_reset.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := body[strings.Index(body, "func (o *Orchestrator) requestZitadelSync"):]
	fn = fn[:strings.Index(fn, "\nfunc ")]

	if !strings.Contains(fn, "schema is still absent") {
		t.Error("the error reports only that no sync began, which was true of a " +
			"perfectly healthy Zitadel")
	}
}
