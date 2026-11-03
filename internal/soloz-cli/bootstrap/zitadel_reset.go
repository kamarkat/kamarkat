package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Zitadel's initialisation is not transactional, so a failed one cannot be
// retried -- it can only be started again from an empty database.
//
// cmd/setup writes to two places: the event stream, and projections.migrations,
// which is the record of what it has done. If it fails between them it leaves a
// database in which work has happened and nothing recorded it. Every retry then
// re-runs 03_default_instance, collides with the instance_domain constraint its
// predecessor wrote, fails with AlreadyExists, and -- by its own log -- "setup
// failed, skipping cleanup". The Job retries until backoff, identically, for
// ever.
//
// The comment in the chart's values assumed the opposite: "restartable by
// design -- cmd/setup runs cleanup on interruption so a retry can pick up where
// we left off". That is true of INTERRUPTION. It is not true of FAILURE, which
// takes the path that skips cleanup.
//
// This was found the slow way. The visible symptom was an ExternalSecret in a
// different namespace stalling for eleven minutes on every run, because Zitadel
// never came up, so iam-admin-pat was never created, so the identity token was
// never uploaded for ESO to deliver.
//
// So initialisation is treated as ATOMIC AT THE DATABASE, which is the same
// contract CNPG already states with `bootstrap: initdb` versus `recovery`, and
// the same one each application's database-recovery overlay applies: a database
// whose initialisation did not complete is recreated, not repaired. That is safe
// precisely because incomplete initialisation means there is nothing to
// preserve.
const (
	zitadelDatabase = "zitadel"

	// Where the identity provider's Application lives, and what it is called.
	zitadelApplication = "zitadel"
	argoCDNamespace    = "platform-ops"
	zitadelNamespace   = "platform-identity"

	// How long a sync of this Application may legitimately run before it is
	// treated as wedged. Its hooks finish in seconds; an hour was observed.
	zitadelSyncStaleAfter = 5 * time.Minute

	// NOTE: there is deliberately no "half-initialised database" signature here,
	// and no code that drops the database.
	//
	// There was. It tested `eventstore.events2 IS NOT NULL AND
	// projections.migrations IS NULL`, justified as "a Zitadel that initialised
	// properly HAS projections.migrations and therefore cannot match it".
	//
	// That is false. Zitadel v4.15.3 has no `projections.migrations` table at
	// all -- it keeps migration state as events in the eventstore, and its
	// projections schema holds a hundred other tables. The signature was inferred
	// from a broken box and never checked against a working one, so it matched
	// EVERY healthy database. Verified afterwards on a running, serving box: the
	// query returned true.
	//
	// Only the "is it serving" gate stood between that and dropping a live
	// identity provider's database, and a Zitadel restarting briefly satisfies it.
	//
	// The drop is gone rather than re-specified. It never fixed anything on this
	// platform: every recovery came from the hook mechanics -- `ensure-schema`,
	// and a failed hook Job deleting itself. A destructive operation that has
	// earned nothing does not get a second signature.
)

// resetZitadelIfInitIncomplete drops and recreates the Zitadel database when a
// previous bootstrap left it half-initialised.
//
// Gated on the database's own signature and nothing else. An earlier version
// also required boundary 04 to be unfinished, which sounded like a safety
// property and was in fact the bug: the phase was recorded complete by a binary
// that never checked Zitadel served, so the guard suppressed the repair on every
// run that could have performed it. The signature is the real guard -- a Zitadel
// that initialised properly HAS projections.migrations and therefore cannot
// match it -- and being the only guard, it is one that cannot be bypassed by a
// stale completion record.
func (o *Orchestrator) prepareZitadelForRetry(ctx context.Context, kubeconfig string) error {
	// Nothing to repair on a box where the identity provider already works. This
	// is also what keeps the repair off the happy path entirely: a first
	// bootstrap reaches here with no Application and no database, both probes say
	// "nothing", and the function returns.
	if err := o.zitadelServing(ctx, kubeconfig); err == nil {
		return nil
	}

	// On a first bootstrap boundary 04 has not been applied yet, so there is no
	// Application, nothing is wrong, and the sync below would be meaningless.
	if !o.zitadelApplicationExists(ctx, kubeconfig) {
		return nil
	}

	// Three conditions, and the third is the one that took longest to see.
	//
	// The first two name specific damage. The third names none: the Application
	// is simply not Synced and nothing is going to change that on its own,
	// because automated sync does not retry a failed sync on the same revision.
	// A box sat in exactly that state -- operation Failed (terminal, so not
	// stuck), database clean (so not half-initialised), OutOfSync with no
	// operation in flight -- and both damage probes correctly said "nothing to
	// repair" while the identity provider had never started.
	//
	// The mistake was modelling WHAT IS BROKEN when the thing that makes progress
	// is the same in every case: request a sync. So the probes decide the
	// PREPARATORY work -- clear a stale operation, recreate a database -- and the
	// sync is what the function is actually for.
	// A sync that is CURRENTLY RUNNING needs waiting, not repairing.
	//
	// OutOfSync is the normal state of an Application mid-sync, so it cannot on
	// its own mean "nothing is happening". This repair fired against a sync that
	// had started 20 seconds earlier and was correctly running Zitadel's PreSync
	// hooks; each of its three sync requests found startedAt unchanged -- because
	// the same operation was still going -- and reported "settled the previous
	// operation rather than starting a sync". It then failed the phase over a
	// sync that was working.
	//
	// zitadelSyncIsStuck already distinguishes a wedged operation from a busy one
	// by age, so deferring to it here is the whole fix: in flight and not stale
	// means leave it alone and let awaitZitadelReady do the waiting.
	if o.zitadelSyncInFlight(ctx, kubeconfig) && !o.zitadelSyncIsStuck(ctx, kubeconfig) {
		return nil
	}

	stuckSync := o.zitadelSyncIsStuck(ctx, kubeconfig)
	outOfSync := o.zitadelOutOfSync(ctx, kubeconfig)
	schemaMissing := o.zitadelSchemaMissing(ctx, kubeconfig)

	if !stuckSync && !outOfSync && !schemaMissing {
		// Not serving, but Synced, with nothing in flight and a database that
		// looks right. Re-syncing would not address that, so say nothing and let
		// awaitZitadelReady report what it finds: a repair that guesses is worse
		// than a wait that names the symptom.
		return nil
	}

	if schemaMissing {
		fmt.Println("[boundary04] Zitadel's database carries no schema yet.")
		fmt.Println("[boundary04]   Either nothing has initialised it, or the hooks that would")
		fmt.Println("[boundary04]   have ran out of retries before they could. Requesting a sync,")
		fmt.Println("[boundary04]   which re-runs init and setup against it.")
	}
	if !stuckSync && !schemaMissing {
		fmt.Println("[boundary04] The zitadel Application is not synced and will not sync itself.")
		fmt.Println("[boundary04]   ArgoCD does not retry a failed sync on the same revision, so an")
		fmt.Println("[boundary04]   Application left this way stays OutOfSync however long it waits.")
		fmt.Println("[boundary04]   Requesting a sync.")
	}
	if stuckSync {
		fmt.Println("[boundary04] The zitadel Application is wedged on a sync that cannot finish.")
		fmt.Println("[boundary04]   An Application runs one operation at a time, so a stuck one is")
		fmt.Println("[boundary04]   a lock on every later sync -- including the one that would")
		fmt.Println("[boundary04]   re-run init and setup. Cancelling it.")
	}
	// Order matters, and this order was learned the hard way.
	//
	// The in-flight sync goes FIRST, and the sync request at the end is not
	// optional -- the two are one operation split around the database work.
	// Zitadel's init and setup Jobs are Helm pre-install hooks, which ArgoCD runs
	// as PreSync, and an operation sits waiting on them. Dropping the database
	// underneath a running setup is a race, and an operation left in flight
	// blocks every later sync, so the re-run this function exists to cause could
	// never start.
	if err := o.clearZitadelSync(ctx, kubeconfig); err != nil {
		return err
	}

	// Finally, ask for a sync. THIS is what re-runs the hooks: ArgoCD deletes and
	// recreates a `before-hook-creation` hook on every sync, so init and setup
	// both run again, in weight order, against the empty database.
	//
	// The Jobs are deliberately NOT deleted here. An earlier version did, on the
	// reasoning that the exhausted setup Job was residue like the database and
	// its owner would rebuild it. Both halves were wrong. A Helm hook is not a
	// tracked resource whose deletion selfHeal repairs -- it is an artifact of an
	// operation -- so nothing rebuilt it; and deleting the very Job the in-flight
	// operation was waiting on left that operation waiting for something that
	// could no longer exist. The sync stuck in `Running` for an hour, retry 8,
	// `waiting for completion of hook batch/Job/zitadel-setup`, and the box was
	// further from working than before the repair ran.
	if err := o.requestZitadelSync(ctx, kubeconfig); err != nil {
		return err
	}

	fmt.Println("[boundary04] ✓ a fresh sync is requested; ArgoCD re-runs init and setup")
	return nil
}

// zitadelSchemaMissing reports that the database carries no eventstore at all.
//
// Distinct from a half-finished initialisation: there, init succeeded and setup
// died part-way. Here init's work is simply not present -- most often because
// this CLI recreated the database, which is exactly what the reset above does.
func (o *Orchestrator) zitadelSchemaMissing(ctx context.Context, kubeconfig string) bool {
	primary, err := o.cnpgPrimary(ctx, kubeconfig)
	if err != nil || primary == "" {
		return false
	}
	out, err := o.psql(ctx, kubeconfig, primary, zitadelDatabase,
		"SELECT to_regclass('eventstore.events2') IS NULL")
	if err != nil {
		return false // No database yet. Not this function's problem.
	}
	return strings.TrimSpace(out) == "t"
}

// NOTHING HERE DELETES A HOOK JOB, and nothing added here ever should.
//
// This file briefly did, to make ArgoCD re-run `zitadel-init` after the database
// was recreated -- its completion record describes a schema that no longer
// exists, so ArgoCD reads it as Succeeded and hands setup an empty database. The
// reasoning was sound; the mechanism was not, and it wedged the Application for
// the second time in one session:
//
//	op=Running started=18:09:45
//	msg=waiting for completion of hook batch/Job/zitadel-init
//	jobs=(none)
//
// The code justified itself with "safe, because the caller has already cleared
// any in-flight operation". That is false, and this same file says so twenty
// lines up: clearing `.operation` orphans the status, it does not stop the
// controller processing the operation it already took. There is no window in
// which a hook Job has no reader, so there is no safe moment to delete one.
//
// The stale-completion problem is real and is NOT solved here. It belongs to
// whatever owns the hooks -- see ADR-079.

// zitadelApplicationExists reports whether boundary 04 has ever been applied.
//
// Distinguishes "a first bootstrap, nothing to repair" from "a box whose
// identity provider is broken", which otherwise look identical from here: in
// both cases nothing is serving.
func (o *Orchestrator) zitadelApplicationExists(ctx context.Context, kubeconfig string) bool {
	err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "application", zitadelApplication, "-n", argoCDNamespace).Run()
	return err == nil
}

// zitadelOutOfSync reports whether the Application's live state differs from
// what it declares.
//
// The condition with no damage behind it. An Application can be OutOfSync with
// nothing stuck and nothing corrupt -- a sync simply failed once -- and stay
// there indefinitely, because automated sync does not retry a failed sync on the
// same revision. Only a request moves it.
func (o *Orchestrator) zitadelOutOfSync(ctx context.Context, kubeconfig string) bool {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "application", zitadelApplication, "-n", argoCDNamespace,
		"-o", "jsonpath={.status.sync.status}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "Synced"
}

// zitadelSyncInFlight reports whether an operation is currently running.
//
// Distinct from zitadelSyncIsStuck, which asks whether a running one has been
// running too long. "Something is happening" and "something is jammed" are
// different questions and conflating them makes a repair interrupt healthy work.
func (o *Orchestrator) zitadelSyncInFlight(ctx context.Context, kubeconfig string) bool {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "application", zitadelApplication, "-n", argoCDNamespace,
		"-o", "jsonpath={.status.operationState.phase}").Output()
	if err != nil {
		return false
	}
	phase := strings.TrimSpace(string(out))
	return phase == "Running" || phase == "Terminating"
}

// zitadelSyncIsStuck reports whether the Application is wedged on an operation
// that is never going to finish.
//
// "Running for longer than a sync of this Application can legitimately take."
// Zitadel's hooks complete in seconds when they complete at all, and this is
// only ever asked on a box whose identity provider is already not serving, so a
// long-running operation here is a wedged one rather than a busy one. The
// threshold exists so that a healthy sync that happens to be in flight when a
// bootstrap resumes is left alone.
func (o *Orchestrator) zitadelSyncIsStuck(ctx context.Context, kubeconfig string) bool {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "application", zitadelApplication, "-n", argoCDNamespace,
		"-o", "jsonpath={.status.operationState.phase} {.status.operationState.startedAt}").Output()
	if err != nil {
		return false // No Application yet, or no cluster. Not this function's problem.
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || (fields[0] != "Running" && fields[0] != "Terminating") {
		return false
	}
	startedAt, err := time.Parse(time.RFC3339, fields[1])
	if err != nil {
		return false
	}
	return time.Since(startedAt) > zitadelSyncStaleAfter
}

// clearZitadelSync removes any in-flight operation from the zitadel
// Application, so a fresh one can be requested.
//
// Removing `.operation` is what `argocd app terminate-op` does, and it is only
// half a repair. It does NOT cancel work and it does NOT settle the status:
// ArgoCD updates `status.operationState` only while processing an operation it
// reads from `.operation`, so once that field is gone the status is orphaned at
// whatever it last said, for ever.
//
// This was established on a live box, and the contrast is the proof:
//
//	platform-argocd   .operation present   phase Running       -- being processed
//	zitadel           .operation absent    phase Terminating   -- frozen, 37 min
//
// So there is deliberately NO WAIT here for the phase to settle. An earlier
// version waited two minutes for exactly that and then failed the phase, which
// made the repair its own deadlock: the only thing that could have moved the
// phase was the sync request that the failure prevented from ever being sent.
// It left the Application permanently unrecoverable and the box worse off than
// before the repair ran.
//
// The caller must therefore always follow this with requestZitadelSync.
func (o *Orchestrator) clearZitadelSync(ctx context.Context, kubeconfig string) error {
	// A MERGE patch setting the field to null, not a JSON-patch `remove`.
	//
	// The two differ exactly where it matters: JSON-patch `remove` on an absent
	// path is an error, and the error is prose -- "The request is invalid: the
	// server rejected our request due to an error in our request" -- which names
	// neither the path nor the reason. An earlier version used it and tried to
	// tell the two cases apart by matching that text. It matched the wrong
	// strings and failed the phase on the ordinary case: an Application with no
	// operation in flight, which is what most resets meet.
	//
	// Merge-null is idempotent by construction, so there is no case to tell
	// apart. Verified against both states on a live cluster: "patched (no
	// change)" when the field is absent, "patched" when it is present.
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"patch", "application", zitadelApplication, "-n", argoCDNamespace,
		"--type", "merge", "-p", `{"operation":null}`).CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not clear the in-flight zitadel sync: %s: %w",
			strings.TrimSpace(string(out)), err)
	}
	return nil
}

// requestZitadelSync asks ArgoCD to sync the zitadel Application, and confirms
// a NEW sync actually began.
//
// Writing `.operation` is what `argocd app sync` does, and it is the only thing
// that re-runs a PreSync hook. A refresh annotation does not: refresh
// re-compares desired against live, and hooks are not part of that comparison --
// which is why an earlier version that annotated `refresh=hard` achieved nothing.
//
// The confirmation is the part that is not obvious, and skipping it left a box
// unrepaired for three hours. A request made while a STALE non-terminal
// operation is recorded does not start a sync: ArgoCD spends it settling the old
// operation instead, writing the terminal phase that operation never reached,
// and clears `.operation` again. Observed exactly:
//
//	before   phase Terminating, startedAt 14:46:13, .operation absent
//	request  -> .operation written
//	after    phase Failed "Operation terminated (retried 8 times)",
//	         startedAt STILL 14:46:13, .operation absent, no hooks run
//
// That is progress -- the Application is no longer wedged -- but it is not a
// sync, and nothing else will supply one. Automated sync does not retry a failed
// sync on the same revision, so an Application left like this sits `OutOfSync`
// with `selfHeal: true` and never moves. Observed for 100s before this was
// understood.
//
// So the request is repeated until `startedAt` changes, which is the only signal
// that distinguishes "a new sync began" from "my request was spent on the old
// one". Bounded: two consumed requests would mean something other than a stale
// operation is refusing them, and a loop would hide that.
func (o *Orchestrator) requestZitadelSync(ctx context.Context, kubeconfig string) error {
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		before := o.zitadelOperationStartedAt(ctx, kubeconfig)

		const op = `{"operation":{"initiatedBy":{"username":"soloz-bootstrap"},` +
			`"sync":{"syncOptions":["CreateNamespace=true","ServerSideApply=true"]}}}`
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
			"patch", "application", zitadelApplication, "-n", argoCDNamespace,
			"--type", "merge", "-p", op).CombinedOutput()
		if err != nil {
			return fmt.Errorf("could not request a zitadel sync: %s: %w",
				strings.TrimSpace(string(out)), err)
		}

		started, err := o.awaitNewZitadelOperation(ctx, kubeconfig, before)
		if err != nil {
			return err
		}
		if started {
			return nil
		}
		// Before asking again, check whether the thing the sync was FOR has
		// happened. See the note on the give-up path below.
		if !o.zitadelSchemaMissing(ctx, kubeconfig) {
			fmt.Println("[boundary04]   the schema now exists; the sync this was requesting has " +
				"already run")
			return nil
		}

		fmt.Printf("[boundary04]   request %d settled the previous operation rather than "+
			"starting a sync; asking again\n", attempt)
	}

	// Last look before failing, at the GOAL rather than the mechanism.
	//
	// This repair exists to get Zitadel's schema created. Requesting a sync is
	// how it tries; it is not what it is for. The two came apart on 2026-09-15:
	// the schema was genuinely missing when the phase began, so the repair set
	// about requesting a sync -- and while it did, ArgoCD's OWN sync (started
	// 11:45:05, Succeeded 11:49:14) ran zitadel-init and zitadel-setup and built
	// the schema. Every request the repair made was spent settling that
	// operation, so `startedAt` never changed, so after three attempts it failed
	// the boundary and ended the bootstrap:
	//
	//	eventstore tables: 8       hub_zitadel role: 1
	//	zitadel        1/1 Running      zitadel-init   Completed
	//	zitadel-setup  Completed        operationState Succeeded
	//
	// A healthy identity provider, reported as a failure, because the repair
	// judged itself on whether IT started the sync rather than on whether the
	// schema exists. The in-flight guard in prepareZitadelForRetry catches the
	// case where a sync is already running when the phase opens; it cannot catch
	// one that starts a moment later, and this is that race.
	if !o.zitadelSchemaMissing(ctx, kubeconfig) {
		fmt.Println("[boundary04]   no new sync began, but the schema now exists -- something " +
			"else supplied the sync, which is the outcome this was after")
		return nil
	}

	return fmt.Errorf("asked ArgoCD to sync the zitadel Application %d times, no new "+
		"sync began, and the schema is still absent; inspect `kubectl get application %s "+
		"-n %s -o jsonpath='{.status.operationState}'`",
		attempts, zitadelApplication, argoCDNamespace)
}

// zitadelOperationStartedAt returns the recorded start of the current operation,
// which identifies it. Empty when there has never been one.
func (o *Orchestrator) zitadelOperationStartedAt(ctx context.Context, kubeconfig string) string {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "application", zitadelApplication, "-n", argoCDNamespace,
		"-o", "jsonpath={.status.operationState.startedAt}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// awaitNewZitadelOperation reports whether a sync distinct from `before` began.
//
// Short, because this waits only for the controller to NOTICE the request, never
// for the sync to finish -- awaitZitadelReady owns that, and owns saying what
// went wrong if it does not.
func (o *Orchestrator) awaitNewZitadelOperation(ctx context.Context, kubeconfig, before string) (bool, error) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		if now := o.zitadelOperationStartedAt(ctx, kubeconfig); now != "" && now != before {
			return true, nil
		}
	}
	return false, nil
}

// cnpgPrimary returns the name of the platform database's primary pod.
func (o *Orchestrator) cnpgPrimary(ctx context.Context, kubeconfig string) (string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"get", "pods", "-n", "platform-data", "-l", "cnpg.io/instanceRole=primary",
		"-o", "jsonpath={.items[0].metadata.name}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// psql runs one statement in the primary and returns its output, unaligned and
// without headers so a caller can compare it directly.
//
// The error carries psql's own stderr. Without it a failure here surfaces as
// "exit status 1" and nothing else, which is what a malformed statement in this
// file looked like from the outside: the reset announced itself, stopped, and
// named neither the statement nor the reason. Postgres had said exactly what was
// wrong; the helper discarded it.
func (o *Orchestrator) psql(ctx context.Context, kubeconfig, pod, database, statement string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig,
		"exec", "-n", "platform-data", pod, "-c", "postgres", "--",
		"psql", "-d", database, "-tAc", statement)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return string(out), fmt.Errorf("%s: %w", msg, err)
		}
	}
	return string(out), err
}
