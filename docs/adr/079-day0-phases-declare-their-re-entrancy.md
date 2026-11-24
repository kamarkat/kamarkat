# ADR-079: A Day-0 Phase Declares Whether It Can Be Re-Run

*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — A failed OpenBao self-initialization is not re-entrant. Bootstrap fails closed and preserves `openbao-db` (ADR-117 §3).*

**Date:** 2026-09-13

**Status:** Proposed

*Constrained by: ADR-040 (Day-0 vs Day-1 Lifecycle Boundary), ADR-072 (Tenant-Controlled Day-0)*

> Every phase is retried. Not every phase can be.

## Context

Day-0 is a state machine. `runPhase` records each completed phase, skips what is
already done, and resumes from the first that is not — which is what makes a
bootstrap survive a laptop closing, a token expiring, or a cluster that took
longer than a timeout allowed. It is one of the better things about this
platform: a failed run costs the failed phase, not the cluster.

That guarantee holds for anything whose effect is a Kubernetes object. Applying
a manifest twice is applying it once. It does not hold for a phase that writes
somewhere else, and nothing said so.

Three failures in three days, all the same shape, each found by a resume:

**Infisical secret upload.** `CreateOrUpdateSecret` checked existence and then
created. The check missed, the create was refused with `Secret already exists`,
and a resumed bootstrap died on `infisical-db-username` after three and a half
minutes of work that had all succeeded. The secret was there because the run
being resumed had put it there.

**The ArgoCD repository credential.** Day-0 wrote the correct organisation-scoped
credential; an ExternalSecret owned the same Secret and overwrote it within the
hour. The imperative write was not wrong, it was simply not the thing that
decides.

**Zitadel's setup.** `cmd/setup` writes event data and, separately,
`projections.migrations` — its record of what it has done. A failure between
them leaves a database in which work happened and nothing recorded it. Every
retry re-runs `03_default_instance`, collides with the constraint its
predecessor wrote, fails with `AlreadyExists`, and by its own log "setup failed,
skipping cleanup". No retry can ever succeed. The chart's values carried a
comment asserting the opposite — *"restartable by design — cmd/setup runs
cleanup on interruption"* — which is true of interruption and false of failure.

Each was fixed where it was found. That is three fixes and no rule, and the
fourth instance is already somewhere in the tree.

The common property is not "external system". It is that **the effect of the
phase is not transactional**, so a partial effect is a state the phase itself
cannot reason about. Retrying is then not recovery; it is repetition against
evidence of the last attempt.

## Decision

**A Day-0 phase declares whether re-running it is safe, and the state machine
refuses to resume one that has not said so.**

Three kinds, and every phase is exactly one:

**`reentrant`** — running it again is the same as running it once. Applying
manifests, waiting for a condition, reading state. The overwhelming majority,
and the current behaviour is already correct for them.

**`resettable`** — running it again is safe only against a clean slate, and the
slate can be cleaned. The phase names how. Zitadel is this: its initialisation
is atomic at the database, so an unfinished one is recovered by recreating the
database, not by repairing it.

**`once`** — running it again is not safe and cannot be made safe. The phase
must not be retried automatically; a human decides. Nothing is in this category
today, and a phase that lands here is a design problem, not a classification.

### Initialisation is atomic at the thing it initialises

For a `resettable` phase the reset is not a workaround, it is the rollback that
the absent transaction would have provided. This platform already states that
contract elsewhere: CNPG's `bootstrap: initdb` versus `recovery`, and the
`database-recovery` overlay that rebuilds `platform-db` from its archive rather
than repairing it in place (ADR-014).

Recreating is safe precisely because incomplete initialisation means there is
nothing to preserve. The guard is therefore not "is the data important" but "did
initialisation finish", and the state machine already knows.

### A signature inferred from a broken system is not a signature

**The reset described below has been removed.** What follows is why, because the
reasoning that justified it is the more useful record.

The condition was `eventstore.events2 IS NOT NULL AND projections.migrations IS
NULL`, and the safety argument was explicit: *"a Zitadel that initialised properly
HAS projections.migrations and therefore cannot match it."*

Zitadel v4.15.3 has no `projections.migrations` table. Migration state lives in
the eventstore as events; the `projections` schema holds a hundred other tables.
The signature was read off a broken box — the table was absent, and absence was
assumed to mean *incomplete* — and never once checked against a working one.

Evaluated later against a healthy, serving box, the query returned **true**. The
repair would have dropped a live identity provider's database. Only the "is it
serving" gate stood in the way, and a Zitadel restarting briefly satisfies that.

Two failures, and the second is the one worth keeping:

1. A state predicate was derived from a single observation of a failure, where
   every symptom is present at once and none is distinguishable from the others.
2. **The claim was written down as the safety argument and never tested.** It
   read as reasoning and was a guess, and being written down made it harder to
   doubt rather than easier.

A predicate that authorises destruction must be verified against the state it
must NOT match. Confirming it matches the broken case is the easy half and proves
nothing.

The drop is gone rather than re-specified with a corrected query. It never fixed
anything here: every recovery came from hook mechanics — `ensure-schema`, and a
failed hook Job that deletes itself. A destructive operation that has earned
nothing does not get a second signature. A test now forbids `DROP DATABASE`,
`CREATE DATABASE`, `TRUNCATE` and `pg_terminate_backend` in this file.

### A failed hook blocks every later sync unless it deletes itself

The chart's hook delete policy was `before-hook-creation`, which covers success
and not failure. A Job left terminally Failed is read by ArgoCD as the hook's
result for the *next* sync too:

```
PreSync/1 hook Job/zitadel-init (,Failed,Job was active longer than deadline)
"sync/terminate complete" duration=41.670788ms
"Skipping auto-sync: application status is Synced"
```

A sync that completes in 41 milliseconds ran nothing. Auto-sync then declines
because the Application reads `Synced`, and every explicit request re-reads the
same stale failure. One Job that failed once made the identity provider
permanently unreachable — and it is why six revisions of the repair above could
get no traction: they requested syncs that were structurally incapable of running.

`hook-delete-policy: before-hook-creation,hook-failed` removes the Job as soon as
it fails. The cost is the failed Job's logs, so the phase now falls back to
ArgoCD's recorded hook message, which outlives it.

### A global is a fact about the box, not about the code path

The environment-manager assembled Applications two ways — from the git tree and
from the published distribution — and each emitted its own `global:` block. The
copies drifted: `dns` and `gitOrgURL` were added to one and not the other, so
every RELEASED box rendered external-dns with `--provider=` and `--txt-owner-id=`
empty and crash-looped on `enum value must be one of ..., got ''`.

The unreleased path was correct throughout, which is exactly why it survived: the
broken path is the one nobody runs while developing. Duplication between a tested
path and a shipped one is not symmetrical — it fails toward the customer.

One emitter now, included by both, and a gate refuses a second `global:` literal
anywhere in the templates.



The condition to reset is: **the state carries the signature of a partial
effect.** One half, not two.

An earlier draft of this ADR required a second condition — that the phase had
not completed — and argued the phase gate was what stopped a reset becoming data
loss. That was wrong, and the box it was written on proved it within one run.
Boundary 04 had been recorded complete by a build that asserted only that ArgoCD
had generated the boundary's Applications. The identity provider had never
served. So on every subsequent run `phaseDone` returned true, the reset was
suppressed, and the phase was skipped — including on the run whose entire
purpose was to perform that reset. The guard did not protect the database; it
protected the defect.

The lesson generalises past this one gate. **A guard whose input is a record of
past intent, rather than an observation of present state, can be switched off by
a stale record — and it will be switched off exactly when the system is broken,
because a broken system is how the record got stale.**

So the signature is the only guard, and it is sufficient precisely because it is
an observation. It must be specific enough to distinguish *half-done* from *not
started* and from *done*. For Zitadel that is `eventstore.events2` present and
`projections.migrations` absent: work written, nothing recorded. A Zitadel that
initialised properly **has** `projections.migrations` and therefore cannot match
the signature, whatever any phase record claims. A vaguer test would eventually
drop a healthy database; this one cannot.

### A completion record is a claim to be verified, not a fact to be trusted

A phase declares what must be TRUE for it to count as done, separately from what
it does. On resume, a phase recorded complete is re-checked against that
postcondition; if it does not hold, the record is withdrawn and the phase runs
again.

This is the complement of the re-entrancy declaration above, and without it that
declaration is unreachable. Re-entrancy answers *may this phase be re-run*.
Nothing answered *is this phase actually done* — and so the state machine
answered it by reading a file written by a previous binary, under whatever
assertions that binary happened to carry.

That is the defect in its general form. A completion record says a phase ran to
the end of its action once. It does not say the effect is still in place, and it
cannot say the phase was checked against an assertion added afterwards. Every
time the platform strengthens a phase's definition of success — which is what
`awaitZitadelReady` was — every already-recorded cluster keeps the weaker one,
permanently, and the strengthening reaches exactly the clusters that never
needed it.

The postcondition must be a **point-in-time probe, never a wait**. Resume is on
the hot path of every run; the waiting belongs to the action that runs when the
probe says the phase is unfinished.

**The outermost claim is subject to the same rule.** A bootstrap recorded
`complete` previously refused all further work — *"Cluster already bootstrapped.
No further CLI operations permitted per ADR-040"* — before checking anything. A
box whose identity provider had never started therefore could not be repaired by
the tool that built it, and the phase-level check would never be reached to
notice. `complete` is now verified against the same postconditions before it is
honoured, and withdrawn with the phases that failed.

This does not weaken ADR-040. ADR-040 bars the CLI from *operating* a cluster
that is built. It does not require the CLI to believe a record over the cluster
standing in front of it. A phase whose postcondition fails was never built.

Adoption is per phase and deliberately sparse: a phase that declares no
postcondition behaves exactly as before. The bar for declaring one is that the
check is cheap, read-only, and answers *did this phase's effect happen* rather
than *did the command exit zero*.

### A reset hands work back to the reconciler; it does not reach around it

The repair has to re-run Zitadel's init and setup. Both are Helm `pre-install`
hooks, which ArgoCD runs as PreSync, and that placed three traps in a row:

1. **A hook is not a tracked resource.** Deleting the setup Job did not create
   drift, so `selfHeal` never rebuilt it. A hook is an artifact of an operation,
   not part of the desired state the Application reconciles toward.
2. **A refresh is not a sync.** Annotating `refresh=hard` re-compares desired
   against live. Hooks are not in that comparison, so nothing ran.
3. **Deleting a hook an operation is waiting on deadlocks the operation.** The
   in-flight sync sat in `Running` for an hour at retry 8, reporting *"waiting
   for completion of hook batch/Job/zitadel-setup"* — a Job that had been
   deleted and could not come back. An Application runs one operation at a time,
   so that stuck one was a lock on every later sync, and the cluster was further
   from working after the repair than before it.

So the rule: **request the operation that legitimately re-runs the work, and let
the reconciler own its own objects.** Cancel the in-flight operation (removing
`.operation`, which is what `argocd app terminate-op` does), then request a sync
(writing `.operation`, which is what `argocd app sync` does). ArgoCD deletes and
recreates `before-hook-creation` hooks on each sync, so init and setup run again
in weight order, against the empty database. No Job is touched directly.

### Cancelling is not a step; it is half of one

`argocd app terminate-op` removes `.operation`. That reads like "cancel", and it
is not: ArgoCD updates `status.operationState` **only while processing an
operation it reads from `.operation`**. Remove the field and nothing cancels —
the status is orphaned at whatever it last said, permanently.

The contrast on the box that showed this is the whole argument:

```
platform-argocd   .operation present   phase Running       being processed
zitadel           .operation absent    phase Terminating   frozen, 37 minutes,
                                                           across a controller restart
```

Removing `.operation` had made the Application *less* recoverable, not more.
Restarting the controller — the obvious remedy, and one that happened on its own
when ArgoCD self-synced — changed nothing, because there was no longer anything
for the restarted controller to process.

So clear and re-request are **one operation split around the database work**, and
the repair may not return between them. Writing a fresh `.operation` is what
gives the controller something to process again, and therefore the only thing
that moves the phase.

The first version put a two-minute wait inside the cancel, for the phase to
"settle" before requesting the sync. That wait made the repair its own deadlock:
the only thing that could have moved the phase was the request the wait's
timeout then prevented from being sent. It failed the phase and left the
Application permanently wedged.

**The general shape, and the reason this sits in this ADR:** a repair built out
of steps that are individually reasonable can still be wrong, because the
intermediate state between two of them is not a state the system can sit in. A
step that leaves the system less recoverable than it found it is not a step — it
is half of one, and the half must not be separable by an early return, a timeout,
or a failure between them.

### Requesting work is not the same as work starting

The repair asks ArgoCD to sync. That request can be silently spent on something
else, and the difference is invisible unless it is checked for.

When a stale non-terminal operation is recorded, ArgoCD uses the incoming request
to **settle that operation** — writing the terminal phase it never reached — and
clears `.operation` again without running anything:

```
before    phase Terminating   startedAt 14:46:13   .operation absent
request   -> .operation written
after     phase Failed        startedAt 14:46:13   .operation absent
          "Operation terminated (retried 8 times)"      no hooks run
```

That is progress — the Application is no longer wedged — but it is not a sync,
and **nothing else will supply one**. Automated sync does not retry a failed sync
on the same revision, so an Application left in this state sits `OutOfSync` with
`selfHeal: true` and does not move. A repair that reported success here would
hand back a box no worse than it found it and no better, having announced that it
fixed something.

So the request is repeated until `status.operationState.startedAt` changes, which
is the only signal separating *a new sync began* from *my request was spent on
the old one*. Bounded at three: a second consumed request means something other
than a stale operation is refusing them, and a loop would hide that.

**The general rule, and the third form of the same mistake in this ADR:** a
repair must verify its own effect, not its own action. Writing the field, issuing
the request and calling the command are all things the repair did; none of them
is the thing the repair is for. This is the same distinction the postcondition
section draws for phases, applied inside a single step — and the reason both
belong here is that a completion record and a successful API call are equally
happy to describe work that did not happen.

### Read the upstream failure path before designing around it

The chart's Jobs are Helm `pre-install,pre-upgrade` hooks, and that is upstream's
default, not a choice made here. The hook is also load-bearing: the weights order
init (1) before setup (2) before the Deployment. It is not the fault.

The values file justified keeping those hooks by quoting upstream — the Jobs are
*"restartable by design: cmd/setup runs cleanup on interruption so a retry can
pick up where we left off rather than booting into a broken state."* The sentence
is real and it is about the cancellation path only. `cmd/setup/setup.go`:

```go
if !errors.Is(setupErr, context.Canceled) {
    logging.OnError(ctx, setupErr).Fatal("setup failed, skipping cleanup")
}
// cleanup runs only below here
```

Interruption cleans up. **Failure does not.** Reading the quote as a general
guarantee is what made a half-written instance look like a transient error worth
retrying, and every retry then collided with the `instance_domain` row its own
predecessor had written.

Upstream does ship a remedy — `zitadel setup cleanup` — and it is worth knowing
precisely what it treats, because it is not this. It finds the last step in
`StepStarted` and pushes a failed event so `shouldExec` re-runs it. Our step was
never `StepStarted`: `internal/migration/migration.go:73` pushes a `failed` event
on migration error, and `shouldExec` returns true for `StepFailed`, so setup was
already retrying. The obstacle was data, not a lock, and no lock-clearing command
can unwrite a row. Recreating the database remains the only remedy, which is why
the decision above stands rather than being replaced by an upstream call.

`cleanup` does treat a real exposure, though, and one this platform had: with
`setupJob.activeDeadlineSeconds: 300`, a migration slower than five minutes is
SIGKILLed mid-step, leaving `StepStarted` — the one state `shouldExec` refuses to
re-run. Every later attempt then logs *"migration already started, will check
again in 5 seconds"* for ever, because the process that would have cleaned up is
the one that was killed. It is wired in through `setupJob.initContainers`, the
extension point the chart documents for pre-setup tasks.

**The rule:** when a dependency's failure mode is the problem, read its failure
path before designing around it. Two of this ADR's decisions were shaped by a
quote that was accurate about a different branch, and the distinction between
`StepStarted` and `StepFailed` — invisible from outside — is what separates the
remedy upstream ships from the one that had to be built.

### Trigger on the goal, not on the catalogue of ways it can be missed

Three things can block boundary 04, and they are not the same thing:

- the database carries a half-finished initialisation;
- the Application is wedged on a sync that cannot finish;
- the Application is simply not synced, and will not sync itself.

The first version treated the second as a consequence of the first and did both
only when the database signature matched. That left a cluster it could not
recover: the database had already been recreated, so the signature was clean,
while the Application stayed locked on a deleted hook. The repair declined to run
on precisely the state it had produced.

The third condition was missed entirely, and missing it is the more instructive
error. It names no damage at all. A box sat in it: operation `Failed` — terminal,
so not stuck — database clean, so not half-initialised — `OutOfSync` with nothing
in flight. Both damage probes correctly reported nothing to repair, and the
identity provider had never started. Nothing would ever have changed that,
because automated sync does not retry a failed sync on the same revision.

**The mistake was modelling what is BROKEN when the thing that makes progress is
the same in every case.** Requesting a sync is what the repair is for; clearing a
stale operation and recreating a database are preparatory work that some states
additionally need. Written the other way round, the repair could only act on
damage it had a name for, and a state with no damage in it was invisible.

So the goal — a synced Application serving an identity provider — is the trigger,
and the damage probes decide what must happen first. Each condition alone is
enough to act. The sequence when both hold is fixed — cancel the sync, then recreate the
database, then request the sync — because dropping a database underneath a
running setup Job is a race, and requesting a sync before the database is ready
would run setup against the old one.

A first bootstrap is excluded by asking whether the Application exists at all.
Before boundary 04 has ever been applied nothing is serving because nothing is
there, which is indistinguishable from a broken box unless it is asked. Without
that check the repair would sit on the happy path of every clean install.

The staleness threshold matters for the same reason the signature does: an
operation that has been running for seconds is a busy Application, and cancelling
it would be the repair causing the fault. It is judged stuck only when the
identity provider is already not serving *and* the operation has outlived what a
sync of this Application can take.

### A reset clears every piece of the failed attempt's residue

Recreating the database was not enough. Zitadel's setup Job had by then exhausted
its `backoffLimit` and stood `Failed`, which in Kubernetes is terminal — no
further pod is started for it. An empty database would have sat there with
nothing to initialise it, and the phase would have waited out its whole deadline
against a Job that had already given up.

So a reset is scoped to the *attempt*, not to one artifact of it: every place the
failed initialisation left state that blocks a retry. For boundary 04 that is the
database, the Application's stuck operation, and — learned last, and the subtlest
of the three — **the completion records of the hook Jobs**.

A completed Job is a claim about a database. Recreating that database makes the
claim false, and the reconciler has no way to know. The reset dropped the
database; `zitadel-init`, the Job whose entire purpose is creating the schema,
stayed `Complete` from before the drop; ArgoCD read it as Succeeded, skipped it,
and ran setup against tables nothing had created:

```
zitadel-init    hook=PreSync  phase=Succeeded   (Job no longer exists)
zitadel-setup   hook=PreSync  phase=Running
  -> relation "eventstore.events2" does not exist
```

The repair had destroyed the thing the record described and left the record
standing. **State and the attestation about that state are two pieces of residue,
and clearing one without the other is how a repair produces a system that is
confidently wrong.**

**The fix is not to delete those Jobs.** That was tried, on the reasoning that
clearing the in-flight operation first left a safe window. There is no such
window. Clearing `.operation` orphans the status; it does not stop the controller
processing the operation it has already taken — a fact established two sections
above, and contradicted by the code that cited it. The Application wedged a
second time, identically:

```
op=Running started=18:09:45
msg=waiting for completion of hook batch/Job/zitadel-init
jobs=(none)
```

A hook Job never has no reader, so it must never be deleted from outside. The
repair is now forbidden from doing so, and a test enforces it.

The remedy belongs where the dependency is, not where the symptom shows. The
setup Job runs `zitadel init` as its own initContainer before setup, so it
establishes the schema it needs rather than trusting another Job's record that it
was established. `init` is idempotent by construction — every step is a "verify",
and it logs "skipping creation" for anything already present — so paying it on
each attempt costs seconds and is correct whether or not the init Job ran.

**The general rule:** when a step depends on a precondition another actor was
supposed to establish, the cheap fix is to make that actor run again and the
correct fix is to make the step establish the precondition itself. The first
requires reaching into a lifecycle you do not own; the second needs nothing from
anyone. Two deadlocks came from choosing the first.

The treatment of each follows from who owns it. The database has no owner after
the fact — CNPG's bootstrap SQL runs once, at initdb — so it is recreated in
place. The Jobs are owned by the zitadel Application, so they are only *removed*,
never recreated here; requesting a sync is how they come back, built by the thing
whose job that is. See the section above for what happens when that distinction
is ignored.

### Applying a declaration is not the same as the controller having acted on it

The rule below was written for a component and applies just as much to a
CONTROLLER'S WORK. Boundary 03 applies the `HubEnvironment`; the hub-operator
then generates credentials, uploads them to Infisical, waits for ESO to deliver
them, and only then creates the platform's database roles. The boundary reported
success the moment ArgoCD had applied the CR.

Boundary 04 therefore started against a database whose roles did not exist:

```
06:22:58  boundary 04 starts; zitadel init/setup hooks begin
06:25:39  operator uploads application secrets
06:32:28  "All database roles provisioned" -- hub_zitadel finally exists
06:33:30  the hooks exhaust their retries and die
```

Every error in between was real and transient — first `password authentication
failed for user "hub_zitadel"` because the role did not exist, then `permission
denied for database zitadel` while it was being granted. Zitadel spent its entire
retry budget on a database that was still being built, and died as it became
usable.

**The tempting fix is to give the hooks a longer deadline, and it is the wrong
one.** A deadline widens the window in which the race is survivable; it does not
order the two. Twice as long still fails on a box where the operator is twice as
slow, and the failure is indistinguishable from a real one.

Boundary 03 now waits for the operator's own `DatabaseRolesProvisioned`
condition. The operator is asked rather than Postgres: provisioning is
all-or-nothing, and a half-granted role cannot be told from a complete one by
counting rows.

**Where this sits relative to moving the component:** Zitadel is already in the
right boundary — 02 builds the database, 03 declares the roles, 04 uses them.
Nothing is fixed by a new boundary or a later one; the ordering was correct and
the *gate between* two correctly-ordered phases was missing.

### The phase that owns a component asserts the component works

`awaitBoundaryInventory` answers "did the boundary generate its Applications",
which is not "does the identity provider serve". Zitadel's setup failed while
boundary 04 reported activated, and the first visible symptom was an
ExternalSecret in another namespace stalling for eleven minutes — because
`iam-admin-pat` was never created, so the identity token was never uploaded, so
ESO had nothing to deliver.

A phase that deploys a component asserts that component reached its own notion
of ready, within a bounded time, and reports the component's own error when it
does not. The cost of not doing this is not a missed failure; it is a failure
found in the wrong place, by someone reading the wrong logs.

## Consequences

### Positive

1. A resumed bootstrap is either safe or refused, and never silently poisons
   the state it is resuming.
2. Failures are named by the phase that owns them, so the first log a reader
   opens is the right one.
3. The classification is a checkable property. A new phase with no declaration
   is a gate failure, not something discovered by the next resume.

### Negative

1. Every phase must be classified, including the forty that are obviously
   reentrant. That is real work and most of it is uninteresting.
2. A `resettable` phase needs its signature written and kept true. A signature
   that drifts from the component's schema is a reset that stops firing, or one
   that fires when it should not.
3. Resetting destroys state. The guards make it safe; they do not make it
   reversible.
4. A postcondition runs on every resume of a completed phase. It must stay cheap,
   and a check that becomes expensive silently taxes every run.
5. A postcondition that is wrong in the strict direction re-runs a phase that was
   fine. Re-entrancy is what makes that survivable, which is why the two
   decisions in this ADR are one ADR.

## Impact

- `runPhase` takes a re-entrancy declaration and applies the reset before a
  retry of a `resettable` phase.
- `runPhase` takes an optional postcondition (`withPostcondition`) and re-checks
  it before skipping a phase recorded complete.
- `phasePostconditions` is the single registry of those checks, read by both
  `runPhase` and `handleExistingState`, so the phase-level and bootstrap-level
  answers cannot disagree.
- `handleExistingState` verifies a `complete` record before honouring it, and
  withdraws it along with any phase whose postcondition failed.
- Boundary 03 waits for the hub-operator's `DatabaseRolesProvisioned` condition
  (`awaitDatabaseRolesProvisioned`), so boundary 04 starts against a database
  whose roles exist.
- Boundary 04 is the first `resettable` phase and the first with a declared
  postcondition: `prepareZitadelForRetry` clears and re-requests a wedged sync
  (`clearZitadelSync` then `requestZitadelSync`, never separably, the latter
  repeating until a new operation actually begins) and recreates the
  Zitadel database when initialisation did not finish, `zitadelServing` is the
  probe, and `awaitZitadelReady` asserts the identity provider serves before the
  phase reports success.
- **Confirms ADR-040.** Day-0 selects and seeds; this says what it means for a
  step of that to be repeated.
- **Extends ADR-014.** Its atomic-initialisation contract for `platform-db`
  becomes the general rule for any database a platform component initialises.

## References

- ADR-014: Platform-Owned Stateful Infrastructure — initialisation and restore
- ADR-035: Enterprise PKI — the fleet issuer's coordinates, another Day-0 write
  that reached the cluster only through the declared state
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-072: Tenant-Controlled Day-0
- Zitadel `cmd/setup/setup.go`, `cmd/setup/cleanup.go`, `internal/migration/migration.go`
  — the failure path, the official cleanup command, and the step states that
  separate what it treats from what it does not
- ADR-055: Boundary Activation — amended to require a readiness contract per
  boundary. The same rule one level up: this ADR governs a phase's own
  postcondition, that one governs what the next boundary may assume.

## Amendment (2026-11-18): Zitadel's initialisation is Sync-phase work, ordered by waves — **Accepted** (implemented 2026-11-18)

**This supersedes the hook decisions above**, specifically *"A failed hook blocks every
later sync unless it deletes itself"* and its remedy `hook-delete-policy:
before-hook-creation,hook-failed`. The incident evidence those sections carry stands
unchanged and is why the new arrangement has the shape it does.

### What the hook arrangement could not do

`3fad010a` moved Zitadel's long-lived prerequisites — the ServiceAccounts, Role,
RoleBinding and config ConfigMaps — out of the hook phase, because a hook resource is
deleted at the start of every sync (ArgoCD's default delete policy is
`BeforeHookCreation`), and a sync that deleted the ServiceAccount a pending Job was
waiting to use made the Job unstartable. That fix was correct and remains in force.

It left an arrangement no clean install can satisfy. ArgoCD runs every PreSync hook to
completion before entering Sync, so `zitadel-init` waited for a ServiceAccount and a
ConfigMap that only Sync would create:

```
Error creating: pods "zitadel-init-" is forbidden: error looking up service account
platform-identity/zitadel: serviceaccount "zitadel" not found
```

The message is the same one quoted earlier in this ADR, for the opposite reason — there
the account had been deleted, here it had never been created. Only a first install of
this component reaches it, which is why it surfaced on a tenant's Day-0 (2026-11-18) and
not on a box that already had the namespace populated.

### Decision

1. **Long-lived prerequisites are ordinary GitOps-managed resources.** Unchanged from
   `3fad010a`; no Helm lifecycle hook may be restored to them.
2. **`zitadel-init` and `zitadel-setup` are ordinary Sync-phase resources.** Their
   `helm.sh/hook*` annotations are removed through the chart's own values.
3. **Resource-level sync waves order initialisation inside the one Zitadel
   Application**: prerequisites at 0, init at 10, setup at 11, both Deployments at 12.
   Permitted by ADR-021's 2026-10-29 amendment, and numbered inside ADR-029 §4's
   existing "dependent workloads at 10+" tier. **ADR-029 is not amended**: no
   platform-wide initialisation tier is created for one component's sequence.
4. **`Force=true,Replace=true` on both Jobs.** A Job's `spec.template` is immutable, so
   a changed image or deadline would otherwise fail the sync permanently; replacement
   also means no terminal Job is ever read as the current result, which is what
   `hook-failed` was for.
5. **A failed Job and its logs remain** until a later sync replaces it. This is the cost
   `hook-failed` paid and no longer has to be paid.
6. **The CLI requests a synchronisation and never deletes a Job.** Unchanged, and the
   prohibition above on deleting hook Jobs applies to these resources too.
7. **A Job's recorded success is not evidence that the database is still initialised.**
   Replacement removes the stale completion record *when a synchronisation actually
   runs*; it does not make every stale database or failed migration self-recovering, and
   it confers no new destructive power. The prohibition against dropping a Zitadel
   database on an inferred incomplete-initialisation signature stands in full.
8. **The CLI's live postconditions remain authoritative.** `awaitZitadelReady`'s probe,
   not a Job's status and not this phase's own record, decides whether the identity
   provider serves.

### Scope

This amendment governs the Zitadel component's resource lifecycle. It changes nothing
about the phase re-entrancy contract this ADR exists for, and nothing about any other
component's Helm hooks.
