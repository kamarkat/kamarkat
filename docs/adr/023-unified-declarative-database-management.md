# ADR 023: Unified Declarative Database Management

## Status
Accepted, partially amended (2026-09-10)
*Supersedes: ADR-002 (Hub Operator Database Management)*
*Amended by: ADR-074 (Withdrawing Atlas)*
*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — CloudNativePG owns database owner roles (which cannot log in) and login-role existence; OpenBao owns hub login-role passwords. The 2026-08-22 Day-0 bootstrap-role addendum is retired with `infisical-db` (ADR-117 §5).*

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

`HubEnvironment.spec.database.roles[].clusterRef` is **required**: a role whose cluster was
assumed would be provisioned on a plausible cluster, against the wrong application's
database, and reported as a success. The box-level `spec.database.clusterRef` is no longer a
default for it — it names the bootstrap cluster, which is the one the operator itself reads.

### Amended 2026-11-03

**"Still outstanding" above counts six application roles; there are three** —
`hub_grafana`, `hub_zitadel` and `hub_credits`, one per application cluster. `hub_hydra`,
`hub_kratos` and `hub_keto` went with the Ory stack; `hub_control_plane` and
`hub_centralized` went with the databases nothing consumed. The mandate itself is
unchanged: those three move to Crossplane `provider-sql` when it is deployed, and the
Day-0 role stays with CNPG.

**The third leg is no longer vacant.** ADR-074's 2026-11-03 addendum gives schema
migration an owner — each application, at its own cluster — and removes the imperative
migrator this ADR designated for deletion. What remains in `internal/database/` is role
DCL only, which is this ADR's second leg and still outstanding as described.


## Amendment (2026-11-24): one password authority per role — **Accepted**

Approved by the architecture review of 2026-11-24. Supersedes ADR-112's
2026-11-18 amendment where it reads *"Role reconciliation stays with the authority
the cluster declares — RoleManager for the three roles"*.

### The ownership, stated once

| Responsibility | Owner |
| :- | :- |
| Credential generation and lifecycle | hub-operator |
| Credential storage | the box's self-hosted Infisical |
| Credential delivery | External Secrets Operator |
| **PostgreSQL password reconciliation** | **CloudNativePG, alone** |
| Credential consumption | the application |

Two namespace-local Secrets carrying one authoritative value are fine. Two password
generators, or two things writing the password into PostgreSQL, are not.

### What was wrong

Two mechanisms wrote the same field from different sources:

- CloudNativePG reconciles the application owner from the Secret
  `GetApplicationSecretName()` resolves — `refreshCredentialsFromSecret()` →
  `reconcileUser()` → `SetUserPassword()`, on **every instance reconcile**, not only
  at bootstrap (v1.30.1, `internal/management/controller`). Chart pin `0.29.1`
  packages that operator.
- hub-operator's `RoleManager` issued `ALTER ROLE … WITH PASSWORD` from the
  Infisical value, from two places: `createOrUpdateRole` and the HubEnvironment
  controller's `handlePasswordRotation`.

Whichever ran last won. On `nutgraf-mgmt-1794929668` the role `hub_zitadel` ended up
holding CloudNativePG's generated bootstrap password while Zitadel presented
Infisical's. Proven by testing each value against the role directly: Infisical's was
refused with `password authentication failed`, CloudNativePG's was accepted. The
PgBouncer pooler was never implicated — it relayed a password the role did not have,
which surfaced as `SASL authentication failed` and sent the first investigation at
the pooler.

`credits-db` and `grafana-db` carried the identical exposure and had not yet failed.

### The correction

Each `Cluster` names its authoritative credential in `bootstrap.initdb.secret`.
`GetApplicationSecretName()` then returns that Secret instead of the generated
`<cluster>-app`, so CloudNativePG reconciles the owner to the delivered value
continuously — on a running cluster, not only at creation. That is the smallest
valid pattern: one source inside CNPG, and no `managed.roles` needed for an owner.

The Secret must satisfy CNPG's contract or it is ignored and CNPG silently keeps its
own password: `kubernetes.io/basic-auth`, `username` equal to the Cluster's `owner`,
and `cnpg.io/reload: "true"`. The username is written **literally**, not from the
remote value, because CNPG takes the role name from the Secret and a renamed
Infisical key would otherwise repoint it at a different role.

`zitadel-db`, `credits-db` and `grafana-db` each gained a dedicated
`<db>-db-owner` Secret in the cluster's namespace. Separate from the consumer's
Secret deliberately: a Secret's `type` is immutable, so converting the existing one
would mean deleting it and interrupting the application, and the consumer wants a
DSN while CNPG wants exactly two keys. `infisical-db` keeps its Day-0 bootstrap
contract (ADR-112 §7) and `billing-db` keeps its existing owner Secret, both now
satisfying the same three requirements.

hub-operator no longer writes passwords. `RoleManager` still creates a role that
does not exist — nothing else does, and a role with no password cannot log in — and
still owns the comment and the grants. `handlePasswordRotation`,
`UpdateRolePassword` and `PasswordNeedsUpdate` are removed, not disabled: a writer
that runs only sometimes is the defect, not a safeguard.

### Readiness

`DatabaseRolesProvisioned` reported `True` throughout the outage. It re-verified that
each declared role **exists**, which a role with the wrong password satisfies.
Existence is not agreement, and reporting one as the other is how a credential
failure presented as a healthy box.

It now also compares each role's stored hash against its authoritative Secret
(`RolesDisagreeingWithSecret`, read-only) and reports `False` with reason
`CredentialNotConverged` when they differ. Reported, not repaired: CloudNativePG
owns the password, so a disagreement means it has not converged yet, and readiness
says that instead of claiming the roles are provisioned.

`preflight/023-one-password-owner-per-role` asserts all of it: every Cluster names an
owner Secret, each satisfies the three contract requirements, hub-operator issues no
password-changing SQL, and readiness compares rather than counts. Five mutations,
all caught.
