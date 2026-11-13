# ADR-026: Dynamic Permission Escalation via RBAC Aggregation for Platform Controllers

**Status**: Accepted  
**Date**: 2026-05-11 (amended 2026-08-28)  
**Context**: Platform controllers (like Kyverno) occasionally need to manage Custom Resources (like Crossplane `ProviderConfig` objects) that are introduced to the cluster *after* the controller was initially deployed. Manually editing Helm charts or maintaining floating `ClusterRoleBindings` to grant these permissions breaks declarative encapsulation and creates GitOps drift.

---

## Problem Statement

Platform controllers require dynamic permission management to handle new Custom Resources and API extensions. Traditional approaches include:

1. **Manual Helm Chart Updates**: Editing values.yaml files and re-deploying controllers
2. **Floating ClusterRoleBindings**: Creating separate RBAC resources outside of Git version control
3. **Static cluster-admin Grants**: Overly privileged permissions that violate least privilege principle

These approaches break our GitOps principles and create operational drift between declared and actual permissions.

---

## Decision

### Core Principle

> **"A mechanism that distributes authority is not itself the authority."**
> RBAC aggregation is a permission-distribution and composition mechanism, not an authorization or least-privilege boundary. Aggregation does not automatically grant least privilege; it merely automates role composition.

We will use **Kubernetes ClusterRole Aggregation** to extend permissions for foundational platform controllers, subject to explicit security boundaries and admission guardrails.

When a platform policy (e.g., Kyverno) requires permissions to generate or manage a specific resource, we declare a dedicated, tightly scoped `ClusterRole` alongside the policy manifest, tagged with the controller's aggregation label (e.g., `rbac.kyverno.io/aggregate-to-background-controller: "true"`).

### Aggregation Security Boundary & Admission Guardrails

To prevent privilege escalation through unauthorized label tagging:

1. **Admission Protection on Aggregation Labels**: Kyverno admission policies SHALL prohibit tenants, non-platform ServiceAccounts, and unauthorized users from creating or mutating `ClusterRole` resources carrying platform aggregation labels (`rbac.kyverno.io/*`, `rbac.authorization.k8s.io/aggregate-to-*`).
2. **Co-located Policy Ownership**: Every aggregated `ClusterRole` must be co-located with and owned by the specific policy manifest requiring those permissions in Git.
3. **Mandatory Documentation**: Every aggregated `ClusterRole` manifest must include annotations documenting the consuming policy, the reason for each permission, and the owner.
4. **Strict Least-Privilege Verbs**: Permissions must be restricted to the minimal API groups, resources, and verbs required:
   - Destructive verbs (`delete`, `deletecollection`) and broad wildcards (`*`) are **prohibited** without explicit platform architecture approval.
   - `list` and `watch` SHALL only be granted when the controller/policy implementation demonstrably requires collection-level observation or cache synchronization (point operations should use `get`, `create`, `update`, `patch`).

---

## Architecture

> **Note (2026-11-14).** The example below is written against `kyverno.io/v1
> ClusterPolicy`, which Kyverno 1.20 no longer executes. The RBAC aggregation
> decision this ADR records is unchanged; a policy that generates resources is
> now a `policies.kyverno.io/v1 GeneratingPolicy`, and the platform ships none.

```yaml
# Kyverno Policy with Dynamic RBAC
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: crossplane-providerconfig-generator
  annotations:
    policies.kyverno.io/title: "Generate Crossplane ProviderConfig"
    policies.kyverno.io/category: "Crossplane"
spec:
  rules:
    - name: generate-providerconfig
      context:
        - name: request
          variable:
            name: providerName
      generate:
        apiVersion: platform.crossplane.io/v1alpha1
        kind: ProviderConfig
        name: "{{request.object.metadata.name}}-providerconfig"
        namespace: platform-ops
        data:
          spec:
            providerConfigRef:
              name: "{{request.object.metadata.name}}"
---
# Aggregated ClusterRole with Strictly Justified Verbs
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kyverno-policy-crossplane-providerconfig
  labels:
    rbac.kyverno.io/aggregate-to-background-controller: "true"
  annotations:
    platform.soloz.io/consuming-policy: "crossplane-providerconfig-generator"
    platform.soloz.io/verb-justification: "create/update/patch to generate ProviderConfigs; get to verify existing state"
rules:
  - apiGroups: ["platform.crossplane.io"]
    resources: ["providerconfigs"]
    verbs: ["create", "update", "patch", "get"]
```

---

## Ownership

| Resource Class | System of Record | Lifecycle Owner | Reconciler | Consumer | Phase |
|---|---|---|---|---|---|
| Admission Policies & Aggregated Roles | Git | Kyverno | Kubernetes API Server (RBAC Aggregation) | Platform Controllers | Day-1+ |

See ADR-039 for the complete ownership matrix.

## Consequences

### Positive

- **Tight Coupling**: Policies and their required permissions are declared together in Git.
- **Auditable Composition**: Permission changes follow the same review and GitOps workflow as policy changes.
- **No Manual RBAC Drift**: Eliminates manual ClusterRoleBinding maintenance; Kubernetes API server aggregates rules dynamically.
- **Bounded Attack Surface**: Admission controls prevent unauthorized tenants from abusing aggregation labels for privilege escalation.

### Negative/Mitigation

- **Aggregation Opacity**: Aggregated permissions are resolved dynamically by the API server, making effective permissions less immediately obvious than explicit static bindings.
  - *Mitigation*: Automated preflight tests and CI validation inspect effective aggregated roles.
- **Label Exploitation Risk**: If aggregation labels are unprotected, any actor with ClusterRole creation rights can escalate privileges to the controller's identity.
  - *Mitigation*: Enforced admission policy blocks unauthorized creation of ClusterRoles with aggregation labels.

---

## Implementation & Testing Guidelines

### When Adding New Policy Permissions

1. **Identify Minimal Required Permissions**: Determine exact API groups, resources, and specific verbs needed.
2. **Justify Every Verb**: Explicitly document why each verb (especially `list`, `watch`, or `delete`) is required.
3. **Create Aggregated ClusterRole**: Declare the role in the same directory/manifest as the consuming policy with standard metadata annotations.
4. **Enforce Admission Guardrails**: Verify that admission webhooks reject attempts to attach aggregation labels by non-platform actors.

### Mandatory Positive & Negative Authorization Testing

Validation must test both **required permissions (positive)** and **prohibited permissions (negative)**:

```text
Positive Verification (Functional):
  ✅ Controller can create intended ProviderConfig in platform-ops
  ✅ Controller can get and patch intended ProviderConfig

Negative Verification (Security Boundary):
  ❌ Controller cannot delete ProviderConfig resources
  ❌ Controller cannot access unrelated Secrets or ConfigMaps
  ❌ Controller cannot manage unrelated CRDs or namespaces
  ❌ Controller cannot escalate its own RBAC bindings or ClusterRoles
  ❌ Non-platform actors cannot create ClusterRoles with aggregation labels
```

---

## Migration Path

### Phase 1: Audit Existing Permissions
- Identify all manually created ClusterRoleBindings and floating roles.
- Map them to responsible controllers/policies.

### Phase 2: Create Aggregated Roles with Justifications
- Convert manual bindings to aggregated ClusterRoles with documented verb justifications.
- Attach appropriate aggregation labels.

### Phase 3: Deploy Admission Guardrails
- Deploy Kyverno/admission policies preventing unauthorized use of aggregation labels.

### Phase 4: Validate Positive & Negative Scopes
- Run automated positive and negative RBAC authorization tests.
- Verify controller functionality and confirm security boundaries hold.

---

## Related ADRs

- **ADR-005**: Unified Abstraction Layers & Domain-Bounded Controllers
- **ADR-015**: Namespace Alignment - Defines deployment boundaries
- **ADR-039**: Platform Ownership Model - Complete platform ownership matrix
- **ADR-041**: Controller Responsibility Matrix - Controller boundaries
- **ADR-043**: Control Plane Authority Model - Authority and delegation invariants
