# Hub Operator

A Kubernetes Operator that manages Day-2 operations for the Kamarkat Hub cluster and orchestrates Spoke bootstrap PKI.

## Overview

The hub-operator reconciles two Custom Resources:

### HubEnvironment (3-phase Day-2 orchestration)
 
| Phase | Responsibility |
|---|---|
| **Phase 1** | Secret Zero generation — bootstrap secrets required before ArgoCD syncs dependent services |
| **Phase 2** | Database setup — executes CNPG migrations and creates database roles |
| **Phase 3** | External services — configures Infisical (secret backup) and Ory Hydra (OAuth clients) |

### SpokePool (bootstrap PKI + identity)

| Responsibility | Artifact | Namespace |
|---|---|---|
| Ensure Infisical credentials | Machine Identity credentials in Infisical `/spoke-pool/{name}/shared/infisical-credentials` | — |
| Create Certificate CR | `argocd-agent-{spokeName}` (72h TTL, ClusterIssuer `infisical-fleet-issuer`) | `platform-capi` |
| Create SpokeMachineIdentity CR | `{spokeName}` (declares desired identity state) | `platform-capi` |
| Package bootstrap cert CRS wrapper | `{spokeName}-bootstrap-cert` (type `addons.cluster.x-k8s.io/resource-set`) | `platform-capi` |
| Set ownerReferences → SpokePool | All bootstrap artifacts for automatic GC | — |

The hub-operator **does not** perform PKI operations. It declares `Certificate` CRs — cert-manager fulfills them per ADR-035. It **does not** call the Infisical PKI API directly.

## Architecture

### Watches

- `SpokePool` (primary — `nutgraf.in/v1alpha1`)
- `Certificate` (secondary — maps via `argocd-agent-{spokeName}` name convention)
- `SpokeMachineIdentity` (secondary — maps via name = spoke name)

Uses explicit `Watches()` with `EnqueueRequestsFromMapFunc` (not `Owns()`) to avoid untested cluster-scoped → namespaced ownerReference watch semantics.

### Bootstrap Cert Flow

```
SpokePool
   │
   ├── ensureBootstrapCertificate() → Certificate CR (72h, platform-capi)
   │       │
   │       ▼  cert-manager + infisical-fleet-issuer
   │   TLS Secret
   │
   ├── ensureBootstrapCertCRSWrapper() → CRS wrapper Secret (immutable)
   │       │
   │       ├── reads TLS Secret
   │       ├── packages as addons.cluster.x-k8s.io/resource-set
   │       ├── sets ownerReferences → SpokePool
   │       ↓
   │   ClusterResourceSet → ApplyOnce → Spoke ArgoCD Agent
   │
   └── ensureSpokeMachineIdentity() → SpokeMachineIdentity CR
           │
           ▼  spoke-identity-operator reconciles
       Identity CRS wrapper Secret → ApplyOnce
```

## Prerequisites

- Go 1.26+
- Access to a Kubernetes cluster with:
  - CloudNativePG operator
  - cert-manager
  - ArgoCD
  - Ory Hydra
  - Infisical (with PKI enabled)

## Dependencies

| Dependency | Purpose |
|---|---|
| `sigs.k8s.io/controller-runtime` | Operator framework |
| `github.com/golang-migrate/migrate/v4` | Database migrations |
| `github.com/cloudnative-pg/cloudnative-pg` | CNPG API types |

## Development

### Building

```bash
make build
```

### Running locally

```bash
make install  # Install CRDs
make run      # Run operator locally
```

### Generating manifests

```bash
make manifests  # CRD + RBAC
make generate   # DeepCopy
```

## Deployment

Deployed via ArgoCD at sync-wave 1. **Never apply manifests directly** — all changes go through Git → ArgoCD.

## Project Structure

```
operators/hub-operator/
├── api/v1alpha1/               # CRD type definitions
├── cmd/                        # Operator entry point
├── config/                     # Kubebuilder generated manifests
├── internal/
│   ├── controller/             # HubEnvironment + SpokePool reconcilers
│   ├── client/                 # External service clients (Infisical, Hydra)
│   ├── secrets/                # Secret generation + Infisical credential logic
│   ├── database/               # Migration and role management
│   └── embed/                  # Embedded SQL migration files
└── tests/e2e/                  # KUTTL E2E tests
```

## Relevant ADRs

- ADR-015: Namespace Alignment (bootstrap artifacts → `platform-capi`)
- ADR-031: Cell-Based Identity Topology (Infisical credential paths)
- ADR-035: Enterprise PKI and Delegated Trust
- ADR-039: Platform Ownership Model
- ADR-040: Day-0 vs Day-1 Lifecycle Boundary
- ADR-041: Controller Responsibility Matrix
- ADR-042: Bootstrap State Machine
- ADR-045: Bootstrap-Generated GitOps Artifacts
