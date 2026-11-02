# ADR 023: Unified Declarative Database Management

## Status
Accepted, partially amended (2026-09-10)
*Supersedes: ADR-002 (Hub Operator Database Management)*
*Amended by: ADR-074 (Withdrawing Atlas)*

> The tri-state contract below is now two-state. CloudNativePG and Crossplane
> `provider-sql` stand; the Atlas leg is vacant, and schema migration has no owner.
> The instruction to strip `hub-operator` of DDL is void while that code is the
> only migration path the platform has -- see ADR-074.

## Context
Previously (per ADR-002), the custom `hub-operator` contained imperative Go logic to execute database migrations (via `golang-migrate`) and provision database roles (via raw SQL queries) for the Hub cluster. Concurrently, the Spoke clusters utilized declarative operators (Atlas Operator and Crossplane `provider-sql`) to achieve the same goals.

This resulted in:
1. **Platform Asymmetry:** The Hub and Spokes managed stateful resources in entirely different ways.
2. **Split-Brain Conditions:** If Crossplane was introduced to the Hub, it would fight the `hub-operator` over role ownership.
3. **Operator Bloat:** The `hub-operator` was acting as an infrastructure script runner rather than a high-level business control plane.

## Decision
We will unify our database management strategy and enforce strict, declarative ownership boundaries across both Hub and Spoke environments.

1. **Remove Imperative Logic:** The `hub-operator` is stripped of all DDL (migrations) and DCL (roles/grants) responsibilities.
2. **Tri-State Ownership Contract:**
   - **CloudNativePG** strictly owns the physical cluster (Pods, PVCs, Replication, Superuser).
   - **Crossplane (`provider-sql`)** strictly owns logical databases, roles, and grants.
   - **Atlas Operator** strictly owns schema migrations and drift detection.
3. **Operator Scope:** The `hub-operator` will now exclusively manage external API orchestration (Infisical, Ory Hydra, NATS) and track high-level platform status (`Provisioning`, `Available`, `Degraded`, `Failed`) by observing the `Ready` conditions of Crossplane and Atlas resources.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Database Clusters (physical) | Kubernetes API | CNPG | CNPG | Crossplane, Applications | Day-1+ |
| Database Roles / Grants | PostgreSQL | Crossplane | Crossplane provider-sql | Tenant Apps | Day-1+ |
| Database Schemas | Git | Atlas Operator | Atlas Operator | provider-sql | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences
* **Positive:** Symmetrical architecture across Hub and Spokes, reducing cognitive load for platform engineers.
* **Positive:** The `hub-operator` codebase becomes significantly lighter, safer, and focused purely on SaaS business logic.
* **Positive:** Eliminates race conditions between custom operators and standardized declarative tools.

## Addendum (2026-08-22): the Day-0 bootstrap role

The Tri-State Ownership Contract above covers Day-1+, which the Ownership table
states explicitly in its Phase column. It does not model a role that must exist
**before** any of the three owners can function. One does, and omitting it has now
caused the same outage twice.

### The gap

`infisical` is not an application role. Every other role's credential reaches its
provisioner through ESO from Infisical, so any Day-1 owner — Crossplane
`provider-sql` as mandated here, or hub-operator's RoleManager as currently
implemented — depends on Infisical already running. Infisical cannot run until the
`infisical` Postgres role exists. The dependency is circular:

```
role provisioner  ──needs──▶  credential Secret
                                    │ from ESO
                                    ▼
                               Infisical
                                    │ needs
                                    ▼
                            `infisical` role
```

Concretely, in hub-operator: Phase 2 provisions roles but requires
ApplicationSecretsReady; Phase 0 blocks on Infisical readiness; so Phase 2 never
runs and Phase 0 never completes. The visible symptom names neither — Infisical
sits in CrashLoopBackOff on `DatabaseError: no such user` while the CNPG cluster
reports `Cluster in healthy state`.

Swapping the provisioner does not help. Crossplane `provider-sql` would hit the
identical circularity, because the problem is the *credential path*, not the
provisioner.

### Decision

**The `infisical` role and database are Day-0 and are owned by CNPG**, declared on
the `platform-db` Cluster (`spec.managed.roles`) and as a `Database` CR
(`owner: infisical`). This extends CNPG's remit beyond "physical cluster +
superuser" for this one bootstrap case, and only this case.

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Phase |
|---|---|---|---|---|
| Bootstrap role + database (`infisical`) | PostgreSQL | CNPG | CNPG | **Day-0** |

It is removed from `HubEnvironment.spec.database.roles` so exactly one controller
owns it — the split-brain this ADR's Context warns about applies just as much to
hub-operator vs CNPG as it does to hub-operator vs Crossplane.

The classification mirrors the platform's existing bootstrap-vs-application split
for *secrets*; this is the same principle applied to roles.

### Why not a Job

A one-shot Job (`setup-infisical-role`) was the original mechanism. Its `Complete`
status records what happened in **Kubernetes**, not what exists in **PostgreSQL**:
after a rebuild, restore, failover or PVC loss it stays Complete while the role is
gone. `spec.managed.roles` is reconciled every loop, so desired and actual converge
on their own, and rotation becomes declarative.

`postInitSQL` is likewise insufficient on its own — it runs once, at initdb, so it
can neither repair nor re-own an existing database. The `Database` CR maps to
`CREATE DATABASE ... OWNER` on a new cluster and `ALTER DATABASE ... OWNER TO` on an
existing one.

### Still outstanding

The mandate to strip DCL from `hub-operator` is **not implemented**: RoleManager
still provisions the six application roles, which `internal/database/roles.go`
acknowledges with `TODO(ADR-023)`. That migration to Crossplane `provider-sql` is
unaffected by this addendum — when it happens, the six move to Crossplane and the
Day-0 role stays with CNPG.

### Amended 2026-11-02 (ADR-112)

`platform-db` is replaced by one cluster per platform application. The Day-0 `infisical`
role and database are declared on `infisical-db`, Infisical's own cluster, and the exception
stays limited to that one case. Every other role is declared against its application's
cluster, by whichever reconciler owns application roles at the time. The `control_plane` and
`hub` roles have no consuming workload and go with their databases (ADR-112 §1).

