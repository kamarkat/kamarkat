# ADR-092: Recovery is exceptional, and its exit is machine-enforced

*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — OpenBao's recovery mode omits consumer admission. Settling the manifest requires a complete recovery record for the incarnation (ADR-117 §7).*

**Date:** 2026-09-28
**Status:** Accepted
**Relates to:** ADR-014 (platform-owned stateful infrastructure), ADR-063 (bundle versions), ADR-047 addendum (the identifier check, the same enforcement shape)

## Context

CloudNativePG has no in-place recovery. `spec.bootstrap` is read **once**, when the
cluster is created, and ignored forever after. Restoring a physical backup *is* the
creation of a new cluster. So "restore the database" is a manifest transition
followed by a delete and recreate, not a command run against a live cluster:

```
steady state      bootstrap.initdb                      archiving to vN
disaster          bootstrap.recovery <- vN, archive v(N+1)
                  DELETE the cluster; it is recreated   <- THIS is the restore
settled           bootstrap.initdb                      archive stays v(N+1)
```

**The steady state is `initdb` because of a local constraint, not a CNPG one.**
`providers/<provider>/cnpg-cluster.yaml` is one template shared by every spoke,
including spokes that do not exist yet, and `destinationPath` is rewritten per
spoke. A template that permanently declared `bootstrap.recovery` would make every
first-ever provisioning fail, because a new spoke has no archive under any
`serverName`. Other deployments legitimately keep a permanent recovery stanza by
giving each restored cluster a new *object* name; this platform keeps the object
name stable and moves the archive incarnation instead.

That choice has a consequence worth stating plainly: **a settled manifest is not a
disaster-recovery configuration.** If the cluster is lost entirely, a rebuild
creates an EMPTY database. Recovering data requires an operator to deliberately
enter recovery mode first. We accept that, because the alternative breaks every
new spoke, and because an empty database that was expected is safer than a stale
one that was not.

## The failure this decides against

Between entering recovery and settling, the repository holds a manifest that
**destroys data if it is applied**. A rebuild in that window restores the old
incarnation and silently discards everything written since the restore.

On 2026-09-25 `shared-cnpg` was restored and the manifest was never settled. The
window stayed open for **three days** and closed only because someone happened to
look. Everything written in it -- the ADR-090 role migration, a second tenant's
database and baseline schema, and every workflow row the first tenant had written
-- would have been lost to any rebuild.

The bump script already warns about exactly this, in its own words: *"settle is not
optional bookkeeping."* It also opens with *"A comment is not a mechanism"* --
written about a different field, the `serverName` suffix, which got a mechanism.
Settle did not. The rule was known, documented, and unenforced, which is the
precise combination that produced a three-day silent window.

## Decision

**No deployable steady-state provider manifest may declare
`spec.bootstrap.recovery`.** A preflight check parses the manifests and fails when
one does, naming the file, the cluster, the mode and the required action.

The check enforces the invariant; it does **not** settle the manifest. Mutating a
recovery procedure automatically would make the dangerous state disappear without
anyone deciding it was safe -- and whether a restore has actually succeeded is a
judgement about the data, not about the YAML. The sequence stays:

```
a human deliberately enters recovery
  -> restore
  -> verification that the recovered cluster is correct
  -> a human settles the manifest
  -> preflight proves no recovery mode remains
```

**It parses, it does not grep.** A string search for `recovery:` is defeated by
ordering, comments, an unrelated key of the same name, or a differently formatted
block, and it produces false positives on documentation. The check loads each
manifest as YAML and inspects `spec.bootstrap` on objects that are actually
`kind: Cluster`.

**It is scoped to deployable manifests.** Runbooks, ADRs and examples must be free
to show a recovery stanza -- that is how the procedure is documented. Only the
provider manifests that a spoke actually renders are in scope.

## The general pattern

This is the first explicit instance of a rule worth applying beyond CNPG:

> **Any recovery procedure that temporarily edits a steady-state declarative
> manifest needs a machine-enforced exit invariant.**

The platform already enforces parity of this shape for tenant identifiers
(ADR-047 addendum) and for the Day-0 ArgoCD seed. Disaster recovery had none,
despite having the largest blast radius of the three: the identifier check
protects against a broken bootstrap, this one protects against silent data loss.

## Consequences

- A restore cannot be committed and forgotten. The window between recovery and
  settle becomes loud instead of silent, and closing it is a commit rather than a
  memory.
- Holding a manifest in recovery deliberately -- mid-restore, with the cluster
  already deleted -- will fail preflight. That is intended: it is a state that
  should not be committed to a shared branch, and if it must be, failing loudly is
  the correct cost.
- `initdb` remains the steady state, so a rebuild after total loss produces an
  empty database until an operator enters recovery. This ADR does not change that;
  it makes the trade explicit so it is not mistaken for a DR configuration.
