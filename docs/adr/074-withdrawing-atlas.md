# ADR-074: Withdrawing Atlas

**Date:** 2026-09-10

**Status:** Proposed

## Context

ADR-023 assigned schema migrations to Atlas Operator as one leg of a tri-state ownership contract: CloudNativePG owns the physical cluster, Crossplane `provider-sql` owns logical databases and roles, Atlas owns schema and drift detection. ADR-020 then built migration governance on top of that assignment — naming, immutability, checksums, rollback philosophy, locking, failure recovery and review, all expressed in terms of Atlas.

Atlas is being removed from the platform. Four facts bear on what that means.

**The assignment was never completed.** `operators/hub-operator/internal/database/migrator.go` and `roles.go` still carry the imperative DDL and DCL that ADR-023 said would be removed, under a header stating they "MUST be removed once provider-sql and Atlas Operator are fully operational." That sentence stood for the life of the decision. What the platform actually ran on the hub was the stopgap, not the contract.

**The tenant path reached into the platform's repository.** The `AINativeSaaS` XRD defaulted `database.migrations.baseline.gitRepo` to `https://github.com/soloz-io/zero-ops` at `migrations/tenant-baseline`. A published bundle carrying that default gave every tenant a runtime dependency on the platform's own git, which ADR-063 requires a release to refuse. The release gate did not catch it, because it inspects Application sources and this was an XRD field default.

**Three mechanisms delivered migrations, not one.** The spoke ran a vendored Atlas Operator; the hub applied `AtlasMigration` CRs against rendered ConfigMaps; tenants also had a `migration-job` kustomize base running the `arigaio/atlas` image directly, with its own ServiceAccount and egress policy. ADR-020 governs the first two and does not describe the third.

**Removal is not a substitution.** No decision has been taken about what owns schema migrations instead. Recording the withdrawal without recording that absence would leave two ADRs describing a mechanism the repository does not contain, and a reader would reasonably conclude migrations are handled.

## Decision

**Atlas is withdrawn from the platform, and schema migration has no owner until one is chosen.**

### What is removed

The vendored spoke operator and its CRDs; the hub and control-plane `AtlasMigration` resources and their migration ConfigMaps; the `atlasmigration` and `baseline-migrations` resources in the tenant composition; the `migrations` schema and `migrationsApplied` status on the `AINativeSaaS` XRD; the `migration-job` kustomize base with its ServiceAccount and egress contract; and the migration configmap template in the universal-tenant chart.

The `migrations` block is withdrawn from the fleet values schema with them. A field describing where migrations come from has nothing to describe.

### What this un-decides

ADR-023's tri-state contract becomes a two-state one. CloudNativePG owns the physical cluster and Crossplane `provider-sql` owns logical databases, roles and grants; both stand. The third leg is vacant.

ADR-020 is withdrawn entire. Its governance — immutability, checksum policy, lineage, locking, drift, recovery, review — was reasoning about Atlas's own mechanics, and none of it survives the tool it governed. A future migration mechanism will need governance, and ADR-020 is worth reading then, but it is not a decision in force.

### What is now load-bearing that was not

`hub-operator`'s imperative migrator is the only migration path the platform has. ADR-023 designated it for deletion; that designation is void, because deleting it now would leave nothing. It is retained deliberately and its header says so, replacing a comment that pointed at a removal condition which can no longer be met.

This is a regression against ADR-023's own reasoning — the platform asymmetry that ADR argued against is now the state — and it is accepted rather than concealed. The asymmetry was real throughout; what changes is that it is no longer described as temporary.

## Addendum (2026-11-03): schema migration has an owner, and this ADR's stopgap is removed

**Each platform application owns and runs its own schema migration, at its own
cluster.** ADR-112 gave every application its own PostgreSQL cluster, and with it the
only grain on which this question has an answer: the schema belongs to the thing that
wrote it, and every application the platform ships already migrates its own.

| Application | How its schema is applied |
|---|---|
| Zitadel | `zitadel-init` and `zitadel-setup`, Helm `pre-install,pre-upgrade` hook Jobs at weights 1 and 2; `zitadel setup cleanup` runs first, in `setupJob.initContainers`, so a step killed mid-migration is re-runnable |
| Infisical | knex, in-process at pod startup; single-flight through `Recreate`, with the `migration-unlock` initContainer clearing a stale `infisical_migrations_lock` |
| Grafana | its own sqlstore migrator, in-process at startup, on every replica |
| Credit Authority | the ledger migrates before it answers `/_healthcheck`; `provision.sh` waits on that |

This is not a mechanism chosen under the pressure this ADR warned about. It is what
the applications do, which the platform was duplicating badly.

### What the platform therefore does not do

`operators/hub-operator/internal/database/migrator.go` and
`operators/hub-operator/internal/embed/migrations/` are **removed**.

This ADR recorded that stopgap as "the only migration path the platform has" and
that "deleting it now would leave nothing". **Both halves were already false when
they were written.** The HubEnvironment controller's call to `RunMigrations` was
deleted on 2026-08-13, in `0d768ad0` ("remove SPIRE dependency and associated
manifests from hub core services") — a month before this ADR. `NewMigrator` had no
caller, so nothing was load-bearing and nothing was running. The file was retained,
and documented as dangerous to remove, on the strength of a reading of the code that
a search for its caller would have settled.

What it would have applied, had anything called it, was `agent_infra_status` and its
RLS policies — a table for a PostgREST-fronted agent registry this platform does not
deploy, in the `hub` database, which went with `platform-db` (ADR-112 §1). The SQL
under `manifests/hub-core-services/database-migrations/` is removed for the same
reason: its own README said nothing applied it, and the databases it described no
longer exist.

**The alternative "delete the migration SQL along with the mechanism", rejected
above, is therefore taken for the HUB only.** Its stated ground was that the SQL
"describes schemas that exist in running databases". For the hub and control-plane
schemas that ceased to be true when those databases were dropped. The tenant
baseline is untouched and still has the owner ADR-089 gave it.

### What is not decided here

*Amended 2026-09-25 by ADR-089, which closes this for the TENANT BASELINE only:
`users`, `identities`, `sessions`, `buckets` and `objects` are applied by a
platform-owned PreSync Job in the universal-tenant chart, idempotently and with
no ledger. An application's own schema is still its own problem and the question
below still stands for it.*

*The cost of leaving it open is recorded there. The baseline SQL survived this
withdrawal while the thing that applied it did not, so it applied to nothing for
a fortnight, and the symptom reached a live fleet as a swallowed log line —
`relation "public.identities" does not exist`, once per authenticated request,
with nothing failing and nothing alerting.*

What owns schema migrations next. Atlas's replacement, if any, is a decision with its own trade-offs — in-cluster operator against pipeline step, declarative against imperative, and who holds the credential that runs DDL — and it should be taken on its merits rather than inherited from what was removed.

## Alternatives considered

**Amend ADR-020 and ADR-023 in place to remove the Atlas references.** Rejected. A decision record that is edited to match the code stops being a record: ADR-020's reasoning was sound for the mechanism it governed, and rewriting it would erase both the reasoning and the fact that it was once in force. Superseding preserves what was decided and why it stopped applying, which is the same treatment ADR-062 gave ADR-004.

**Withdraw Atlas and name a replacement in the same decision.** Rejected, though it would leave a smaller gap. Choosing a migration mechanism under the pressure of having just removed one biases toward whatever most resembles it. The gap is uncomfortable and that is appropriate: it is a real gap, and recording it as one is what makes it visible.

**Retain Atlas on the spoke and remove it only from the hub.** Not taken. It would keep a declarative owner where most schema changes land, but it preserves exactly the asymmetry ADR-023 existed to end, and leaves a vendored operator to maintain for one of three paths.

**Delete the migration SQL along with the mechanism.** Rejected. The hub, control-plane and tenant-baseline SQL describe schemas that exist in running databases. They are inert without a runner and they are not regenerable from anything else, so they are retained where they are retained and their absence from a delivery path is not a reason to discard them.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Physical database cluster | `zero-ops` | Platform | CloudNativePG | Tenants | Day-1+ |
| Logical databases, roles, grants | `zero-ops` | Platform | Crossplane `provider-sql` | Tenants | Day-1+ |
| Schema migrations | the application | the application | the application's own migrator, at its own cluster | itself | Day-1+ |

That row was empty, and the emptiness was the decision. It is filled by this ADR's
2026-11-03 addendum. See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

A published bundle no longer carries a default pointing at the platform's git repository, so one instance of the defect ADR-063 forbids is gone from the tenant path.

Three delivery mechanisms for one concern become zero, which is a smaller distance to a single one than three were.

The stopgap in `hub-operator` is now described accurately. It was marked for imminent removal for months while being the only thing that ran; a reader now learns what is true.

### Negative

The platform has no declarative schema migration. A tenant database can be provisioned and cannot be migrated by any supported mechanism, and the hub's own schema advances only through imperative Go that ADR-023 classified as a violation.

ADR-023's asymmetry argument now describes the platform rather than the problem it solved.

Governance is withdrawn along with the tool. Immutability, checksums and lineage were policies worth having independently of Atlas, and nothing asserts them until a replacement does.

Migration SQL is retained with no mechanism that applies it, so the repository holds schema definitions whose relationship to any running database is unverified.

## Impact

- **Withdraws ADR-020.** Its governance applied to Atlas and does not survive it. Status changed accordingly.
- **Amends ADR-023.** The tri-state ownership contract becomes two-state; the Atlas leg is vacant. The instruction to strip `hub-operator` of DDL is void while that code is the only migration path.
- **Amends ADR-047.** The `migrations` block is withdrawn from the fleet values schema.
- **Confirms ADR-063.** Removing the XRD's git default eliminates a runtime reference from a published bundle to the platform's repository; the gate that missed it inspects Application sources only, which is recorded here as a known limit.
- No change to ADR-006 or ADR-014: database ownership and platform-owned stateful infrastructure are unaffected.

## References

- ADR-006: Multi-Tenant Database Pattern
- ADR-014: Platform-Owned Stateful Infrastructure
- ADR-020: Enterprise Migration Governance with Atlas (withdrawn by this ADR)
- ADR-023: Unified Declarative Database Management
- ADR-039: Platform Ownership Model
- ADR-047: Fleet Tenant Deployment Contract
- ADR-063: The Platform Bundle and its Version
