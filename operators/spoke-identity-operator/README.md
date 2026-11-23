# Spoke Identity Operator

A Kubernetes Operator that manages Machine Identity lifecycle in Infisical for Spoke clusters, replacing the deprecated cert-operator's identity responsibilities.

## Overview

The spoke-identity-operator watches `SpokeMachineIdentity` CRs in `platform-capi` and reconciles them against the Infisical API. It manages the full identity lifecycle: creation, rotation, revocation, and ClusterResourceSet (CRS) wrapper packaging for bootstrap delivery.

**This operator does NOT perform PKI operations.** Certificate issuance is cert-manager's exclusive domain per ADR-035.

## Responsibilities

- Watch `SpokeMachineIdentity` CRs
- Create/delete Machine Identities in Infisical
- Grant/revoke project-level permissions
- Rotate client secrets per `spec.rotationPolicy`
- Package identity credentials into CRS wrapper Secrets (type `addons.cluster.x-k8s.io/resource-set`)
- Set `ownerReferences` → SpokePool for automatic GC
- Guard CRS wrappers as immutable (bootstrap-only, never regenerated)

## Architecture

```
SpokeMachineIdentity CR (platform-capi)
        │
        ▼
spoke-identity-operator reconciles
        │
        ├── GetOrCreateMachineIdentity (Infisical API)
        ├── GrantProjectAccess
        ├── handleRotation (create/rotate client secret)
        ├── ensureCRSWrapper (identity.yaml → addons.cluster.x-k8s.io/resource-set Secret)
        │
        ▼
Identity CRS Wrapper Secret (immutable, platform-capi)
        │
  ClusterResourceSet → ApplyOnce
        │
        ▼
Spoke ESO pulls rotated credentials from Infisical (Day-1)
```

**Key distinction:** The identity CRS wrapper is a bootstrap-only artifact. Rotation updates the authoritative `smi-{spokeName}-auth` Secret (for Day-1 ESO access), not the wrapper.

## CRD

| Name | Group | Kind | Scope |
|---|---|---|---|
| `SpokeMachineIdentity` | `identity.kamarkat.io/v1alpha1` | `SpokeMachineIdentity` | Namespaced |

### Status Conditions

| Condition | Purpose |
|---|---|
| `Ready` | Overall reconciliation success |
| `IdentityProvisioned` | Machine Identity exists in Infisical |
| `RotationComplete` | Client secret rotation succeeded |

## Development

### Prerequisites

- Go 1.26+
- Access to a Kubernetes cluster with:
  - cert-manager
  - Infisical (with PKI enabled)

### Building

```bash
make build
```

### Running locally

```bash
make run
```

### Generating manifests

```bash
make manifests  # CRD + RBAC
make generate   # DeepCopy
```

## Deployment

Deployed via ArgoCD at sync-wave 3 in the `03-platform-services` ApplicationSet:

```yaml
- appName: spoke-identity-operator
  path: operators/spoke-identity-operator/config/default
```

## Project Structure

```
operators/spoke-identity-operator/
├── api/v1alpha1/               # CRD type definitions
├── cmd/                        # Operator entry point
├── config/
│   ├── crd/bases/              # Generated CRDs
│   ├── default/                # Kustomize overlay for ArgoCD
│   ├── manager/                # Deployment manifest
│   └── rbac/                   # ClusterRole
├── internal/
│   ├── controller/             # Main reconciler
│   └── infisical/              # Infisical REST API client
└── Dockerfile
```

## Relevant ADRs

- ADR-035: Enterprise PKI and Delegated Trust
- ADR-039: Platform Ownership Model
- ADR-041: Controller Responsibility Matrix
- ADR-043: Control Plane Authority Model
- ADR-045: Bootstrap-Generated GitOps Artifacts
