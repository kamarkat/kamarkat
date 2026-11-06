# ADR 037: Directory-Based Environment Promotion and Gating

## Status
Accepted

*Revised 2026-11-06: clause 2 is superseded. The management hub is environment-independent
(ADR-111 Revision 7): one shared hub manages workload clusters in every environment, and a
workload environment is declared by the managed cluster, never by the hub. Clauses 1 and 3-8
are unchanged -- the promotion gates were always enforced in the repository, not by which hub
could reach which directory.*

## Context
The platform requires a structured mechanism to promote infrastructure changes and tenant configurations through isolated lifecycle stages (ephemeral preview, dev, staging, production). Relying on a flat manifest structure or branch-based promotion risks configuration drift, obscures cross-environment visibility, and allows unverified changes to impact production workloads.

## Decision
We will enforce environment progression and access gating using a Directory-Based Overlay pattern combined with Git repository controls:

1. **Directory-Based Overlays:** Environments will be defined by strictly segregated Kustomize overlay directories (e.g., `manifests/environments/dev`, `stg`, `prod`). We explicitly reject branch-based promotion; all environment states will reside in the `main` branch to maintain a single, auditable source of truth.

2. **Environment-Independent Management Hub** *(revised 2026-11-06; supersedes "Environment-Bound Hubs")*

   The platform runs one shared management hub. It does not declare an environment, and no environment-scoped value is derived from it. A workload environment is a property of the managed cluster: each declares its own `environmentSlug` and provider, written by the SpokePool composition from the claim the tenant wrote, and every environment-scoped read -- overlay path, secret scope, supported-topology check -- resolves from the cluster's own declaration.

   The original clause bought isolation physically: a prod hub could not reach a dev directory because it had no Application pointing at one. That is replaced by declaration, not by a second mechanism. The hazard it was guarding against is real and is recorded where it was found -- a workload cluster that read the hub's slug instead of its own would read another environment's secrets out of Infisical -- and the fix is that nothing reads the hub's slug, because the hub does not have one.

   What this clause no longer provides is stated in Consequences rather than hidden: one hub reconciling every environment's overlay is one ArgoCD that can, in principle, apply any of them.

3. **Automated Promotion Workflow:** Promotion between environments SHALL be performed through automated repository workflows that create promotion pull requests. Direct manual duplication of configuration between environment overlay directories is discouraged. This prevents configuration drift caused by human error during file copying.

4. **Sequential Promotion:** Infrastructure and application changes must be promoted sequentially by mutating the overlays from lower environments to higher environments via Pull Requests. The promotion path is ephemeral through dev through stg to prod. Promotion workflows SHALL verify that the target revision has successfully reconciled and passed all required validation checks in the preceding environment before promotion is permitted. This prevents promotion from bypassing verification gates and provides an auditable compliance trail.

5. **Gated Deployment Approval:** Production deployment workflows SHALL use GitHub Environment Protection Rules requiring explicit approval before deployment execution. This creates a two-gate model where both change approval (PR review) and deployment approval (environment protection) are independently enforced.

6. **Cryptographic Gating:** GitHub `CODEOWNERS` SHALL mandate explicit Platform Engineering review and approval for any modifications targeting the `prod` overlay directories.

7. **Differentiated CI Validation:** Continuous Integration pipelines will execute lightweight, fast-fail validation (manifest linting, policy dry-runs) on all Pull Requests. Heavy end-to-end deployments (spinning up local Hub and CAPD Spoke clusters) will be decoupled and triggered via explicit PR labels to optimize pipeline velocity and compute costs.

8. **GitOps-Driven Rollback:** Rollback SHALL be performed through Git revert operations and GitOps reconciliation rather than direct cluster mutations. This preserves the integrity of Git as the single source of truth and ensures rollbacks are fully auditable.

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Kubernetes Resources | Git | ArgoCD | ArgoCD | Platform, Tenants | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences
### Positive
- Cross-environment differences and configuration drift are explicitly visible within a single repository commit history.
- Production modifications are securely gated by mandatory reviews and deployment approvals without requiring complex Git branching strategies.
- Blast radius is isolated by declaration: a manifest reaches a cluster only if that cluster declares the environment it belongs to, so a malformed commit in the `dev` overlay is applied to clusters that declared `dev` and to no others.
- Rollbacks are fully auditable through Git history rather than imperative cluster operations.

### Negative
- Promotion depends on automated workflow coordination across environment overlays, introducing additional pipeline complexity and operational ownership requirements.
- A single management hub reconciles every environment, so the physical impossibility clause 2 originally asserted is gone. One ArgoCD holds credentials for, and can apply to, dev and prod alike. The remaining controls are the repository gates (clauses 4-6) and per-cluster declaration -- both of which were already load-bearing, and neither of which is a physical boundary. Accepted deliberately: environment-bound hubs required a production-grade hub per environment, which is three times the management-plane cost to defend a boundary that the repository gates were already defending.
