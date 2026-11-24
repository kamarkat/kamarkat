# ADR-040: Day-0 vs Day-1 Lifecycle Boundary

**Date:** 2026-06-08
**Status:** Accepted

*Amended by: ADR-063 (The Platform Bundle and its Version)*
*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — Seal keys, recovery material and the root CA are Day-0 CLI artefacts. Seal rotation and intermediate rollover are maintenance operations under ADR-035's rollover provision (ADR-117 §3, §6).*

> Day-0 is unchanged as a boundary. What Day-0 installs is a published bundle at a
> version rather than content resolved from the platform's repository.

## Context

The platform's bootstrap sequence (ADRs 003, 021, 025, 035) mixes Day-0 imperative actions with Day-1+ declarative reconciliation. The CLI creates secrets, uploads them to Infisical, injects trust anchors, and gates ArgoCD boundaries — but nowhere is there a formal definition of when Day-0 ends and Day-1 begins, what each phase may do, or how ownership transfers between them.

Without this boundary, every bootstrap decision invites the question: "Should the CLI do this, or should the operator?" The answer changes depending on who is asking and when. The platform requires an unambiguous lifecycle contract.

## Decision

### Phase Definitions

#### Day-0: Imperative Bootstrap

Day-0 is executed exactly once during initial Hub cluster creation. It is the minimum set of operations required to establish the trust, identity, and configuration foundations that Day-1 controllers depend on.

**Allowed components:** CLI only.

**Responsibilities:**
- Create organizations and projects in Infisical.
- Create the PKI trust hierarchy (Root CA, Fleet Intermediate CA).
- Create Machine Identities for Spoke Pools and operators.
- Generate and store bootstrap secrets in Infisical.
- Inject Secret Zero and trust anchors into the Hub cluster.
- Create the initial ArgoCD GitOps bootstrap (boundary ApplicationSets).
- Gate the activation of ArgoCD boundaries (`01-platform-infra`, `02-platform-data`, `03-platform-services`).

**Forbidden:**
- No controller involvement (ArgoCD, Crossplane, ESO, cert-manager, Hub Operator, CNPG, Atlas).
- No runtime reconciliation.
- No tenant or Spoke provisioning.
- No continuous operations of any kind.

**Exit condition:** All Day-0 artifacts are stored in their Systems of Record and the CLI exits permanently.

#### Day-1+: Declarative Continuous Reconciliation

Day-1+ begins when ArgoCD takes control of the bootstrap boundaries and controllers begin reconciliation. Day-1+ runs continuously for the life of the platform.

**Allowed components:** Hub Operator, Spoke Identity Operator, Crossplane, ESO, cert-manager, ArgoCD, Atlas Operator, CNPG, Kyverno, SPIRE, Billing Operator.

**Responsibilities:**
- Secret rotation and renewal.
- Certificate issuance, renewal, and lifecycle.
- Tenant provisioning and de-provisioning.
- Spoke provisioning and teardown.
- Runtime state reconciliation.
- Database schema migration and role management.
- Infrastructure composition and lifecycle.

**Forbidden:**
- CLI execution for any operational purpose.
- Imperative mutation of Day-0 artifacts.
- Bypassing the System of Record for any state change.

### Day-0 Ownership Handoff Rule

A Day-0 artifact becomes owned by its Day-1 Lifecycle Owner only after the Lifecycle Owner successfully reconciles the artifact at least once. Until that first successful reconciliation:

1. Ownership remains with the CLI bootstrap process.
2. The artifact is considered in a transitional state.
3. The CLI is responsible for reporting bootstrap failures; the Lifecycle Owner is not.

This rule is critical for failure recovery. If the Hub Operator is deployed but never reconciles the machine identity, the fault does not belong to the Hub Operator. The CLI (or the operator monitoring the bootstrap) is responsible for diagnosing and reporting the failure.

### Artifact Immutability Rule

Day-0 artifacts become immutable inputs for Day-1 controllers. A Day-1 controller may read a Day-0 artifact from its System of Record but must never modify it.

Day-0 artifacts that require lifecycle operations must have a Day-1 counterpart. For example:
- Day-0 bootstrap certificates (72h TTL) are replaced by cert-manager-issued certificates during Day-1.
- Day-0 machine identities are consumed as input by the Hub Operator but the Hub Operator must not rotate them unless authorized as the Lifecycle Owner.

### Transition Trigger

The Day-0 → Day-1 transition is triggered when all three ArgoCD boundaries (`01-platform-infra`, `02-platform-data`, `03-platform-services`) report `Healthy` and `Synced`. This trigger is defined in ADR-042 (Bootstrap State Machine).

## Consequences

### Positive

- Unambiguous boundary prevents "should the CLI or the operator do this" debates.
- Ownership handoff rule provides a clear answer to "who is responsible when bootstrap succeeds but the operator never starts."
- Artifact immutability prevents Day-1 controllers from overwriting bootstrap foundations.
- New contributors can determine which phase owns any operation by consulting this ADR.

### Negative

- A failed ownership handoff requires operator intervention to diagnose — the system cannot self-heal a bootstrap failure.
- Transition from Day-0 to Day-1 is a hard gate; there is no partial handoff.
- Any resource not covered by this ADR defaults to Day-1+ (controller-owned), which may be incorrect for future bootstrap requirements.
