# ADR-022: GitOps Workload Separation via Remote Bases

**Date:** 2026-05-27
**Status:** Accepted
**Supersedes:** ADR-021 §"Tenant Workloads (BYOWR)" (workload placement only)
**Related ADRs:**
- ADR-021: Boundary-Driven GitOps and Day-0 Choreography
- ADR-007: Fleet Registry Spoke Flow

---

*Amended by: ADR-073 (Workload Delivery is Version Pinning)*

> Tenant workload overlays are no longer held in a platform-owned registry
> repository. A workload is published as a versioned chart from its own repository
> and consumed by the tenant's gitops repository naming that version. Remote bases
> remain available to a workload's own chart.

## Context

ADR-021 introduced the BYOWR (Bring Your Own Workload Repo) pattern and the
`tenant-workload-provisioning` ApplicationSet. The initial implementation placed
tenant-specific manifests (`bff.yaml`, `frontend.yaml`, `custom-migrations.yaml`)
inside the `zero-ops` platform repository under
`manifests/tenants/workloads/app-creator/`.

This created three violations:

1. **Monorepo bloat and lifecycle coupling.** Tenant application changes required
   commits to the platform repository, mixing platform engineering and product
   engineering change streams.

2. **Merge conflict risk at scale.** With hundreds of tenants, every image digest
   update would produce a PR against `zero-ops`, creating a bottleneck on the
   platform team.

3. **Boundary violation.** The platform repository's responsibility is governance
   primitives and infrastructure. Tenant business logic (SQL migrations, env vars,
   image references) does not belong there.

---

## Decision

### Repository responsibilities

| Repository | Responsibility |
|---|---|
| `zero-ops` | Platform governance, ArgoCD, Crossplane, Kyverno, **hardened workload bases** |
| `fleet-registry` | Tenant descriptors (`values.yaml`) + **tenant workload overlays** |
| Tenant app repo | Application source code + CI pipeline |
| GHCR | Signed, immutable OCI artifacts |

### Platform bases (`zero-ops`)

`zero-ops/manifests/tenants/workloads/bases/` contains reusable, hardened
Kustomize bases. Each base encodes the full enterprise ABI:

- `stateless-web/` — Argo Rollout with canary strategy, dedicated ServiceAccount,
  topology spread, pod anti-affinity, non-root securityContext, seccomp, resource
  limits, liveness/readiness probes, zero-trust CiliumNetworkPolicy.
- `migration-job/` — Atlas migration Job with PreSync hook, BeforeHookCreation
  delete policy, activeDeadlineSeconds, dedicated ServiceAccount, hardened
  securityContext, resource limits.

Bases contain **no tenant-specific values** — no image references, no env vars,
no hostnames. They are un-deployable on their own.

### Tenant overlays (`fleet-registry`)

`fleet-registry/tenants/<tenantId>/workloads/` contains the tenant's Kustomize
overlays. Each overlay imports the platform base as a remote resource:

```yaml
resources:
  - github.com/kamarkat/kamarkat/manifests/tenants/workloads/bases/stateless-web?ref=main
```

The overlay provides **only** business-specific configuration:
- Image digest (updated by tenant CI/CD)
- Environment variables
- Port numbers
- Custom SQL migration files
- Tenant-specific FQDN egress additions

### Governance is mathematically enforced

Tenants cannot weaken the platform constraints because:

1. **Kyverno ABI policy** (deployed to every spoke via `spoke-infrastructure`
   ApplicationSet) rejects Pods that violate `runAsNonRoot`, missing digests,
   or missing `tenant-id`/`cost-center` labels — regardless of what the overlay
   patches.

2. **PSA restricted** labels on the namespace (applied by `universal-tenant`
   Helm chart) block privileged containers, hostPath, hostNetwork, and unsafe
   capabilities at the admission controller level.

3. **AppProject `tenant-workloads`** restricts the ApplicationSet to
   namespace-scoped resources only — no CRDs, no ClusterRoles, no
   cluster-scoped escalation.

A tenant patching `runAsNonRoot: false` in their fleet-registry overlay will
have their Pod rejected by Kyverno before it reaches the scheduler. The base
patch is irrelevant — the enforcement plane wins.

### Platform release pinning (production requirement)

In development, overlays reference `?ref=main`. In production, every overlay
**must** pin to an immutable platform release SHA:

```yaml
resources:
  - github.com/kamarkat/kamarkat/manifests/tenants/workloads/bases/stateless-web?ref=8f3c9d1e2a4b5c6d
```

This creates:
- Reproducible deployments — the same SHA always produces the same base
- Platform release channels — tenants opt in to base upgrades explicitly
- Safe rollbacks — reverting a SHA reverts the entire base

The platform team manages base versioning. Tenants consume base upgrades by
updating the `?ref=` value in their fleet-registry overlay.

### Deployment flow

```
Step 1 — Tenant CI builds and signs image
  → pushes to GHCR with immutable sha256 digest

Step 2 — Tenant CI updates fleet-registry
  → updates images[].digest in tenants/<id>/workloads/bff/kustomization.yaml
  → commits to fleet-registry main (or feature branch)

Step 3 — ArgoCD detects change
  → tenant-workload-provisioning ApplicationSet reads values.yaml
  → creates/updates Application pointing at fleet-registry workloads path

Step 4 — Kustomize resolves remote base
  → fetches zero-ops base at pinned ?ref=
  → merges tenant overlay (image, env, ports, FQDN additions)
  → produces final manifests

Step 5 — Admission enforcement
  → Kyverno validates digest, labels, securityContext
  → PSA validates pod security standards
  → Non-compliant resources rejected before scheduling

Step 6 — Workload runs in spoke enclave
  → isolated namespace, isolated DB, restricted network,
    non-root runtime, immutable image, monitored workload
```

---

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Kubernetes Resources | Git | ArgoCD | ArgoCD | Platform, Tenants | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

- **Zero tenant business logic in `zero-ops`.** The platform repository is clean.
- **Independent lifecycles.** Tenant CI/CD updates image digests in fleet-registry
  without touching platform code or requiring platform team review.
- **Governance by inheritance.** Every tenant workload automatically gets the
  full enterprise ABI — security, HA, observability — without copying YAML.
- **Safe base upgrades.** Platform team ships base improvements; tenants adopt
  them by updating a single `?ref=` value.
- **Audit trail separation.** Platform changes and tenant changes are in separate
  Git histories with separate approval workflows.

### Trade-offs

- **Remote base resolution latency.** ArgoCD fetches the remote base from GitHub
  at sync time. This adds a network round-trip per sync. Mitigated by ArgoCD's
  repo cache.
- **Two-repo coordination.** A breaking base change requires tenants to update
  their `?ref=` pin. Mitigated by semantic versioning of base releases and a
  migration guide per breaking change.
- **`?ref=main` in development.** Development overlays use `main` for velocity.
  This must be enforced as a pre-production gate — no `?ref=main` in production
  overlays. A Kyverno policy or CI lint check should enforce this.

---

## File structure

```
zero-ops/
└── manifests/tenants/workloads/bases/
    ├── stateless-web/          ← platform base (hardened, un-deployable)
    │   ├── kustomization.yaml
    │   ├── rollout.yaml
    │   ├── service.yaml
    │   ├── service-account.yaml
    │   └── network-policy.yaml
    └── migration-job/          ← platform base (hardened, un-deployable)
        ├── kustomization.yaml
        ├── job.yaml
        └── service-account.yaml

fleet-registry/
└── tenants/app-creator/
    ├── values.yaml             ← tenant descriptor (workloads.gitPath points here)
    └── workloads/              ← tenant overlay (business logic only)
        ├── kustomization.yaml  ← commonLabels: tenant-id, cost-center
        ├── bff/
        │   └── kustomization.yaml   ← imports stateless-web base, overlays image+env
        ├── frontend/
        │   └── kustomization.yaml   ← imports stateless-web base, overlays image+env
        ├── custom-migrations/
        │   ├── kustomization.yaml   ← imports migration-job base, overlays configmap
        │   └── configmap.yaml       ← tenant SQL + atlas.sum
        └── ingress.yaml             ← tenant-specific hostnames (not in base)
```
