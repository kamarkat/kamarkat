# ADR-039: Platform Ownership Model

*Amended by: ADR-117 (Self-Hosted Secret Management on OpenBao), 2026-11-24 — Ownership rows for secret storage, database credentials, PKI and escrow custody follow ADR-117's Ownership table.*

**Date:** 2026-06-08
**Status:** Accepted

## Context

The platform operates across multiple resource domains — secrets, certificates, infrastructure, database schemas, identity, and Kubernetes state — each governed by different controllers. Existing ADRs describe implementation flows (who generates, who uploads, who syncs) but do not define a single authoritative ownership model. Ownership claims are scattered across ADRs 003, 004, 006, 008, 011, 014, 020, 023, 024, and 030, with no reference point for resolving cross-domain conflicts.

Every resource must have exactly one authoritative record of its desired state (System of Record), exactly one component responsible for changes (Lifecycle Owner), and exactly one component responsible for ensuring the current state matches the desired state (Reconciler).

## Decision

The following matrix is the single authoritative source for ownership boundaries across the platform. Every other ADR that touches resource lifecycle must reference this matrix. Where another ADR appears to conflict, this matrix controls.

### Ownership Matrix

| Resource Class | Generator | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|---|
| Bootstrap Secrets | CLI | Infisical | CLI | None | Operators, CNPG | Day-0 |
| Application Secrets | Human / Infisical UI | Infisical | Infisical | ESO | Workloads, Crossplane | Day-1+ |
| Tenant Passwords | Kube-SBT | Infisical | Tenant Identity Service | ESO | Tenant Apps, provider-sql | Day-1+ |
| Spoke Infrastructure Credentials | Hub Operator | Infisical | Hub Operator | ESO | Crossplane provider-sql | Day-1+ |
| Certificates | cert-manager | Kubernetes API | cert-manager | cert-manager | Workloads, ArgoCD Agent | Day-1+ |
| Kubernetes Resources | ArgoCD | Git | ArgoCD | ArgoCD | Platform, Tenants | Day-1+ |
| Infrastructure XRs | Crossplane | Kubernetes API | Crossplane | Crossplane | Spokes, Tenants | Day-1+ |
| Database Schemas | Atlas Operator | Git | Atlas Operator | Atlas Operator | provider-sql | Day-1+ |
| Database Clusters (physical) | ArgoCD | Kubernetes API | CNPG | CNPG | Crossplane, Applications | Day-1+ |
| Database Roles / Grants | Crossplane provider-sql | PostgreSQL | Crossplane | Crossplane provider-sql | Tenant Apps | Day-1+ |
| PKI Trust Anchors | CLI | Offline Storage | CLI | None | cert-manager, Services | Day-0 |
| Machine Identities | CLI | Infisical | Hub Operator | Infisical API | Spokes, Operators | Day-0 |
| Spoke Machine Identities | Hub Operator | Infisical | Spoke Identity Operator | Infisical API | Spokes | Day-1+ |
| Observability State | Alloy / OTel Collector | VictoriaMetrics / Loki | Observability Stack | Alloy / OTel Collector | Operators, SRE | Day-1+ |
| Billing Catalog | Catalog reconciler (kube-sbt) | Git | Platform | Catalog reconciler → Lago API | Lago invoice engine | Day-1+ |
| Workload Identity | SPIRE | SPIRE Server | SPIRE | SPIRE | Istio, Tenant Apps | Day-1+ |
| Admission Policies | Kyverno | Git | Kyverno | Kyverno | Kubernetes API | Day-1+ |
| Network Policies | ArgoCD | Git | ArgoCD | CNI (Cilium) | Spoke Clusters | Day-1+ |

### Column Definitions

- **Generator:** The component that creates the initial value. Generation is a one-time event. It is distinct from lifecycle ownership.
- **System of Record:** The authoritative storage location for the desired state. All reconcilers must read from the System of Record. All mutations must flow through the Lifecycle Owner to the System of Record.
- **Lifecycle Owner:** The single component authorized to modify the resource's desired state after creation. Responsible for rotation, renewal, updates, and eventual deletion.
- **Reconciler:** The component that continuously ensures the current state matches the desired state stored in the System of Record. Writes are to the managed system, never to the System of Record.
- **Consumer:** Components that read and use the resource but must not modify it.
- **Phase:** Whether the resource is created during Day-0 (bootstrap) or Day-1+ (continuous operation). See ADR-040.

### Ownership Constraints

1. Every resource has exactly one Generator, one System of Record, one Lifecycle Owner, and one Reconciler.
2. The Generator and Lifecycle Owner MAY be the same component, but the separation in this matrix is deliberate — generating a password is not the same as owning its lifecycle.
3. The Lifecycle Owner MUST NOT also be the Reconciler, unless the resource class explicitly requires co-location (e.g., cert-manager is both Lifecycle Owner and Reconciler for certificates because certificate issuance and lifecycle are inseparable).
4. No component may read a resource from anywhere other than its System of Record for the purpose of acting on that resource.
5. No component may mutate a resource's desired state by writing directly to the System of Record unless it is the Lifecycle Owner.

## Consequences

### Positive

- Single reference point eliminates ownership ambiguity across the platform.
- Future ADRs can assert "X owns Y" by citing this matrix, rather than re-deriving ownership boundaries.
- Cross-domain conflicts (e.g., Hub Operator vs Kube-SBT for tenant passwords) have a clear resolution path.
- Ownership assignments are auditable — every state change can be attributed to exactly one component.

### Negative

- Any change to ownership boundaries requires updating this matrix and reviewing all referencing ADRs.
- The matrix must be maintained as new resource classes are introduced (new ADRs must register their resource classes here).
