# ADR 008: Federated API Boundary Pattern - Design

## Overview

This design implements the Federated API Boundary Pattern by introducing a `SpokeTenantEnvironment` XRD on Spoke clusters that acts as the API contract between Hub and Spoke. The Hub pushes high-level intent (tenant specification), and the Spoke implements the details (Namespace, RBAC, Pooler, PostgREST, etc.).

## Architecture

```
Hub Cluster
├── AINativeSaaS XR (tenant intent)
├── AINativeSaaS Composition (simplified)
│   └── SpokeTenantEnvironment Object MR → Spoke (single XR)
└── provider-kubernetes (RBAC: 1 XRD type only)

Spoke Cluster
├── TenantDatabase XRD + Composition (existing)
├── SpokeTenantEnvironment XRD + Composition (Russian Doll Pattern)
│   ├── TenantDatabase XR (Resource 1 - creates database + secrets)
│   ├── Namespace (Resource 2)
│   ├── ServiceAccount (Resource 3)
│   ├── Role (Resource 4)
│   ├── RoleBinding (Resource 5)
│   ├── ResourceQuota (Resource 6)
│   ├── CNPG Pooler (Resource 7 - references TenantDatabase secrets)
│   ├── PostgREST Deployment (Resource 8 - references TenantDatabase secrets)
│   ├── PostgREST Service (Resource 9)
│   └── AtlasMigration (Resource 10)
└── Local Crossplane (reconciles XRs locally)
```

**Russian Doll (Matryoshka) Pattern:**
- Hub pushes **ONE** XR: `SpokeTenantEnvironment`
- Spoke Composition nests `TenantDatabase` XR as Resource 1
- Guarantees ordering: Database → Namespace → RBAC → Pooler → PostgREST
- Eliminates race conditions between Hub-pushed XRs
- Simplifies Hub RBAC (only 1 XRD type needed)

## Component Design

### 1. SpokeTenantEnvironment XRD

**File:** `zero-ops/manifests/spoke/xrds/spoketenantenvironment.yaml`

**Key Change:** Add `databaseName` field to XRD spec (needed for TenantDatabase XR composition)

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: CompositeResourceDefinition
metadata:
  name: spoketenantenvironments.nutgraf.in
spec:
  group: nutgraf.in
  names:
    kind: SpokeTenantEnvironment
    plural: spoketenantenvironments
  claimNames:
    kind: SpokeTenantEnvironmentClaim
    plural: spoketenantenvironmentclaims
  versions:
  - name: v1alpha1
    served: true
    referenceable: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            required:
              - tenantId
              - tier
              - databaseName
              - cellId
            properties:
              tenantId:
                type: string
                description: "Tenant identifier"
                pattern: '^[a-z0-9]([-a-z0-9]*[a-z0-9])?'
                maxLength: 63
              tier:
                type: string
                description: "Tenant tier"
                enum: [starter, enterprise]
              databaseName:
                type: string
                description: "Database name for TenantDatabase XR and pooler connection"
              cellId:
                type: string
                description: "Spoke Pool cell identifier (for TenantDatabase XR)"
              postgrestImage:
                type: string
                description: "PostgREST container image"
                default: "postgrest/postgrest:v12.0.2"
              resourceQuota:
                type: object
                properties:
                  cpu:
                    type: string
                  memory:
                    type: string
                  storage:
                    type: string
                  pods:
                    type: string
          status:
            type: object
            properties:
              ready:
                type: boolean
                description: "True when all resources provisioned"
              databaseReady:
                type: boolean
                description: "True when TenantDatabase XR is ready"
              message:
                type: string
                description: "Human-readable status message"
              conditions:
                type: array
                items:
                  type: object
                  properties:
                    type:
                      type: string
                    status:
                      type: string
                    lastTransitionTime:
                      type: string
                      format: date-time
                    reason:
                      type: string
                    message:
                      type: string
```
  versions:
  - name: v1alpha1
    served: true
    referenceable: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            required:
              - tenantId
              - tier
              - databaseName
            properties:
              tenantId:
                type: string
                description: "Tenant identifier"
                pattern: '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
                maxLength: 63
              tier:
                type: string
                description: "Tenant tier"
                enum: [starter, enterprise]
              databaseName:
                type: string
                description: "Database name for pooler connection"
              postgrestImage:
                type: string
                description: "PostgREST container image"
                default: "postgrest/postgrest:v12.0.2"
              resourceQuota:
                type: object
                properties:
                  cpu:
                    type: string
                  memory:
                    type: string
                  storage:
                    type: string
                  pods:
                    type: string
          status:
            type: object
            properties:
              ready:
                type: boolean
                description: "True when all resources provisioned"
              message:
                type: string
                description: "Human-readable status message"
              conditions:
                type: array
                items:
                  type: object
                  properties:
                    type:
                      type: string
                    status:
                      type: string
                    lastTransitionTime:
                      type: string
                      format: date-time
                    reason:
                      type: string
                    message:
                      type: string
```

### 2. SpokeTenantEnvironment Composition (Russian Doll Pattern)

**File:** `zero-ops/manifests/spoke/compositions/spoketenantenvironment-default.yaml`

**Key Design Decisions:**

1. **Russian Doll (Matryoshka) Pattern:** Compose `TenantDatabase` XR as Resource 1 inside `SpokeTenantEnvironment` Composition (not pushed separately by Hub)

2. **Composition Functions Pipeline:** Use modern `mode: Pipeline` pattern with `function-patch-and-transform` (consistent with existing `spokepool-hetzner` and `tenantdatabase-spoke` compositions)

3. **Guaranteed Ordering:** TenantDatabase XR (Resource 1) → Namespace (Resource 2) → RBAC → Pooler → PostgREST

4. **Native Resources:** Use native Kubernetes resources directly (Namespace, ServiceAccount, etc.) instead of wrapping in Object MRs

5. **Status Aggregation:** Use Crossplane's built-in status aggregation to set `status.ready` and `status.databaseReady` based on all composed resources

6. **Secret References:** Pooler and PostgREST reference secrets created by nested `TenantDatabase` XR (same Composition, guaranteed ordering with initContainers for safety)

**Composition Structure:**

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: spoketenantenvironment-default
spec:
  compositeTypeRef:
    apiVersion: nutgraf.in/v1alpha1
    kind: SpokeTenantEnvironment
  
  mode: Pipeline
  pipeline:
    - step: patch-and-transform
      functionRef:
        name: function-patch-and-transform
      input:
        apiVersion: pt.fn.crossplane.io/v1beta1
        kind: Resources
        resources:
          # Resource 1: TenantDatabase XR (Russian Doll - nested inside SpokeTenantEnvironment)
          - name: tenant-database
            base:
              apiVersion: nutgraf.in/v1alpha1
              kind: TenantDatabase
              metadata:
                name: ""  # Patched from tenantId
              spec:
                tenantId: ""
                cellId: ""
                databaseName: ""
                tier: ""
            patches:
              - type: FromCompositeFieldPath
                fromFieldPath: spec.tenantId
                toFieldPath: metadata.name
              - type: FromCompositeFieldPath
                fromFieldPath: spec.tenantId
                toFieldPath: spec.tenantId
              - type: FromCompositeFieldPath
                fromFieldPath: spec.cellId
                toFieldPath: spec.cellId
              - type: FromCompositeFieldPath
                fromFieldPath: spec.databaseName
                toFieldPath: spec.databaseName
              - type: FromCompositeFieldPath
                fromFieldPath: spec.tier
                toFieldPath: spec.tier
              - type: ToCompositeFieldPath
                fromFieldPath: status.ready
                toFieldPath: status.databaseReady
            readinessChecks:
              - type: MatchCondition
                matchCondition:
                  type: Ready
                  status: "True"
          
          # Resource 2: Namespace (created after TenantDatabase due to ordering)
          - name: namespace
            base:
              apiVersion: v1
              kind: Namespace
              metadata:
                name: ""  # Patched from tenantId
                labels:
                  tenant-id: ""
                  tier: ""
                  managed-by: crossplane
            patches:
              - type: FromCompositeFieldPath
                fromFieldPath: spec.tenantId
                toFieldPath: metadata.name
                transforms:
                  - type: string
                    string:
                      fmt: "tenant-%s"
              - type: FromCompositeFieldPath
                fromFieldPath: spec.tenantId
                toFieldPath: metadata.labels['tenant-id']
              - type: FromCompositeFieldPath
                fromFieldPath: spec.tier
                toFieldPath: metadata.labels['tier']
          
          # Resource 3-10: ServiceAccount, Role, RoleBinding, ResourceQuota, Pooler, PostgREST Deployment, PostgREST Service, AtlasMigration
          # (Full definitions omitted for brevity - see existing design for complete YAML)
```

**Safe Eventual Consistency Pattern:**

Since `TenantDatabase` XR is now composed **inside** `SpokeTenantEnvironment` (Russian Doll pattern), Crossplane guarantees ordering within the Composition. However, there's still a timing gap between when TenantDatabase reports Ready and when secrets are fully propagated to the namespace. To handle this safely:

1. **TenantDatabase XR - readinessChecks:**
   ```yaml
   - name: tenant-database
     readinessChecks:
       - type: MatchCondition
         matchCondition:
           type: Ready
           status: "True"
   ```
   This blocks subsequent resources until TenantDatabase is fully ready.

2. **PostgREST Deployment - Startup Probe Configuration:**

1. **PostgREST Deployment - Startup Probe Configuration:**
   ```yaml
   spec:
     template:
       spec:
         initContainers:
           - name: wait-for-secret
             image: busybox:1.36
             command:
               - sh
               - -c
               - |
                 echo "Waiting for database credentials secret..."
                 until [ -f /secrets/username ]; do
                   echo "Secret not found, waiting 5s..."
                   sleep 5
                 done
                 echo "Secret found, proceeding with startup"
             volumeMounts:
               - name: db-credentials
                 mountPath: /secrets
         containers:
           - name: postgrest
             startupProbe:
               httpGet:
                 path: /
                 port: 3000
               initialDelaySeconds: 30
               periodSeconds: 10
               failureThreshold: 30  # 5 minutes total before marking as failed
             readinessProbe:
               httpGet:
                 path: /
                 port: 3000
               periodSeconds: 10
               failureThreshold: 3
   ```

2. **CNPG Pooler - Startup Probe Configuration:**
   ```yaml
   spec:
     template:
       spec:
         initContainers:
           - name: wait-for-secret
             image: busybox:1.36
             command:
               - sh
               - -c
               - |
                 echo "Waiting for database credentials secret..."
                 until [ -f /secrets/password ]; do
                   echo "Secret not found, waiting 5s..."
                   sleep 5
                 done
                 echo "Secret found, proceeding with startup"
             volumeMounts:
               - name: auth-secret
                 mountPath: /secrets
         containers:
           - name: pgbouncer
             startupProbe:
               tcpSocket:
                 port: 5432
               initialDelaySeconds: 30
               periodSeconds: 10
               failureThreshold: 30  # 5 minutes total
   ```

3. **Monitoring Alert Tuning:**
   - Suppress `PodCrashLooping` alerts for tenant namespaces during first 5 minutes
   - Suppress `PodNotReady` alerts for tenant namespaces during first 5 minutes
   - VictoriaMetrics alert rule example:
     ```yaml
     - alert: PodCrashLooping
       expr: rate(kube_pod_container_status_restarts_total{namespace=~"tenant-.*"}[15m]) > 0
       for: 5m  # Only alert after 5 minutes
       annotations:
         summary: "Pod {{ $labels.namespace }}/{{ $labels.pod }} is crash looping"
     ```

**Benefits of This Approach:**
- ✅ **Russian Doll Pattern**: TenantDatabase nested inside SpokeTenantEnvironment guarantees ordering
- ✅ **No Race Conditions**: Database created before Namespace, secrets exist before Pooler/PostgREST
- ✅ **readinessChecks**: TenantDatabase must be Ready before subsequent resources are created
- ✅ **initContainers**: Additional safety layer for secret propagation timing
- ✅ **No CrashLoopBackOff**: initContainer blocks pod startup until secret exists
- ✅ **No noisy logs or false alerts** during normal provisioning
- ✅ **Kubernetes-native pattern** (initContainers are standard practice)
- ✅ **Self-healing** once secret appears
- ✅ **5-minute timeout** prevents infinite waiting if secret never appears
- ✅ **Simpler Hub RBAC**: Hub only needs permissions for 1 XRD type (SpokeTenantEnvironment)

### 3. Hub Composition Refactoring (Simplified to 1 XR)

**File:** `zero-ops/manifests/hub-core-services/crossplane/tenant-platform/compositions/ainativesaas-starter-hetzner.yaml`

**Changes:**

1. **Remove 10 Object MRs:**
   - tenant-database-remote (moved to Spoke Composition - Russian Doll)
   - namespace (moved to Spoke Composition)
   - service-account (moved to Spoke Composition)
   - role (moved to Spoke Composition)
   - role-binding (moved to Spoke Composition)
   - resource-quota (moved to Spoke Composition)
   - pooler (moved to Spoke Composition)
   - postgrest-deployment (moved to Spoke Composition)
   - postgrest-service (moved to Spoke Composition)
   - atlasmigration (moved to Spoke Composition)

2. **Keep ONLY 1 Object MR: SpokeTenantEnvironment (Russian Doll Pattern)**

**Simplified Hub Composition:**

```yaml
# Resource 1: SpokeTenantEnvironment XR - Remote Provider Pattern (Russian Doll)
- name: spoke-tenant-environment
  base:
    apiVersion: kubernetes.crossplane.io/v1alpha2
    kind: Object
    spec:
      managementPolicies: ["*"]
      forProvider:
        manifest:
          apiVersion: nutgraf.in/v1alpha1
          kind: SpokeTenantEnvironment
          metadata:
            name: ""  # Patched from tenantId
          spec:
            tenantId: ""
            tier: ""
            databaseName: ""
            cellId: ""  # NEW: Required for TenantDatabase XR nested inside
            postgrestImage: ""
            resourceQuota:
              cpu: ""
              memory: ""
              storage: ""
              pods: ""
      readinessChecks:
        - type: MatchCondition
          matchCondition:
            type: Ready
            status: "True"
      providerConfigRef:
        name: ""  # Patched from cellId
  patches:
    - type: FromCompositeFieldPath
      fromFieldPath: spec.cellId
      toFieldPath: spec.providerConfigRef.name
    - type: FromCompositeFieldPath
      fromFieldPath: spec.tenantId
      toFieldPath: metadata.name
      transforms:
        - type: string
          string:
            fmt: "%s-spoke-tenant-environment"
    - type: FromCompositeFieldPath
      fromFieldPath: spec.tenantId
      toFieldPath: spec.forProvider.manifest.metadata.name
    - type: FromCompositeFieldPath
      fromFieldPath: spec.tenantId
      toFieldPath: spec.forProvider.manifest.spec.tenantId
    - type: FromCompositeFieldPath
      fromFieldPath: spec.cellId
      toFieldPath: spec.forProvider.manifest.spec.cellId  # NEW: Pass cellId to Spoke
    - type: FromCompositeFieldPath
      fromFieldPath: spec.tier
      toFieldPath: spec.forProvider.manifest.spec.tier
    - type: FromCompositeFieldPath
      fromFieldPath: spec.database.name
      toFieldPath: spec.forProvider.manifest.spec.databaseName
    - type: FromCompositeFieldPath
      fromFieldPath: spec.postgrest.image
      toFieldPath: spec.forProvider.manifest.spec.postgrestImage
    - type: FromCompositeFieldPath
      fromFieldPath: spec.resourceQuota.cpu
      toFieldPath: spec.forProvider.manifest.spec.resourceQuota.cpu
    - type: FromCompositeFieldPath
      fromFieldPath: spec.resourceQuota.memory
      toFieldPath: spec.forProvider.manifest.spec.resourceQuota.memory
    - type: FromCompositeFieldPath
      fromFieldPath: spec.resourceQuota.storage
      toFieldPath: spec.forProvider.manifest.spec.resourceQuota.storage
    - type: FromCompositeFieldPath
      fromFieldPath: spec.resourceQuota.pods
      toFieldPath: spec.forProvider.manifest.spec.resourceQuota.pods
```

**Key Benefits:**
- Hub Composition reduced from 11 Object MRs to **1 Object MR** (90% reduction)
- Hub ETCD footprint reduced by ~90%
- Hub RBAC simplified to **1 XRD type** (SpokeTenantEnvironment only, not TenantDatabase)
- No race conditions between Hub-pushed XRs
- All implementation details encapsulated in Spoke Composition (Russian Doll)

### 4. ArgoCD Distribution

**File:** `zero-ops/manifests/argocd/apps/platform-spoke-catalog-appsets.yaml`

Add new ApplicationSets for `SpokeTenantEnvironment` XRD and Composition with proper sync-wave ordering:

```yaml
---
# SpokeTenantEnvironment XRD (Spoke) - Must sync before Composition
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: platform-spoketenantenvironment-xrds
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "11"
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - clusters:
        selector:
          matchLabels:
            spoke-type: pool
  template:
    metadata:
      name: '{{.name}}-spoketenantenvironment-xrds'
      namespace: platform-ops
      labels:
        cell-id: '{{.name}}'
      annotations:
        argocd.argoproj.io/sync-wave: "1"  # XRDs must sync first
    spec:
      project: platform-infrastructure
      source:
        repoURL: https://github.com/kamarkat/kamarkat.git
        targetRevision: HEAD
        path: manifests/spoke/xrds
        directory:
          include: 'spoketenantenvironment.yaml'
      destination:
        name: '{{.name}}'
        namespace: platform-ops
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
          - ServerSideApply=true
        retry:
          limit: 5
          backoff:
            duration: 5s
            factor: 2
            maxDuration: 3m

---
# SpokeTenantEnvironment Composition (Spoke) - Syncs after XRD
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: platform-spoketenantenvironment-compositions
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "11"
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - clusters:
        selector:
          matchLabels:
            spoke-type: pool
  template:
    metadata:
      name: '{{.name}}-spoketenantenvironment-compositions'
      namespace: platform-ops
      labels:
        cell-id: '{{.name}}'
      annotations:
        argocd.argoproj.io/sync-wave: "2"  # Compositions sync after XRDs
    spec:
      project: platform-infrastructure
      source:
        repoURL: https://github.com/kamarkat/kamarkat.git
        targetRevision: HEAD
        path: manifests/spoke/compositions
        directory:
          include: 'spoketenantenvironment-*.yaml'
      destination:
        name: '{{.name}}'
        namespace: platform-ops
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
        retry:
          limit: 5
          backoff:
            duration: 5s
            factor: 2
            maxDuration: 3m
```

**Sync Wave Ordering:**
- Wave "1": XRDs (SpokeTenantEnvironment XRD must be established first)
- Wave "2": Compositions (Compositions reference XRDs, so they sync second)
- This prevents ArgoCD errors when Compositions try to reference non-existent XRDs

### 5. RBAC Configuration (Simplified to 1 XRD Type)

**File:** `zero-ops/manifests/hub-core-services/crossplane/provider-kubernetes-rbac.yaml`

Update Hub's `provider-kubernetes` RBAC to only allow **1 XRD type** (Russian Doll pattern):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: crossplane-provider-kubernetes-spoke
rules:
  # SpokeTenantEnvironment XR (Russian Doll - contains TenantDatabase)
  - apiGroups: ["nutgraf.in"]
    resources: ["spoketenantenvironments"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  
  # Status subresource for SpokeTenantEnvironment
  - apiGroups: ["nutgraf.in"]
    resources: ["spoketenantenvironments/status"]
    verbs: ["get", "update", "patch"]
```

**Key Changes:**
- Removed `tenantdatabases` and `tenantdatabases/status` rules
- Hub no longer needs RBAC for TenantDatabase XRD (it's nested inside SpokeTenantEnvironment on Spoke)
- Hub RBAC reduced from 2 XRD types to **1 XRD type**

## Progressive Rollout Strategy

**Scenario:** Update PostgREST image version from v12.0.2 to v12.0.3

**Before (Current):**
1. Update Hub Composition `postgrest-deployment` resource
2. Commit to Git
3. ArgoCD syncs to Hub
4. Crossplane reconciles ALL tenants globally
5. ALL tenants across ALL Spokes get new image simultaneously

**After (Federated API Boundary):**
1. Update Spoke Composition `postgrest-deployment` resource
2. Commit to Git
3. Update ArgoCD ApplicationSet to target only `spoke-pool-eu-prod-01`
4. ArgoCD syncs Composition to Cell A only
5. Crossplane on Cell A reconciles tenants on Cell A only
6. Validate Cell A tenants
7. Update ApplicationSet to include Cell B
8. Repeat for remaining cells

**Rollback:** Revert Composition on affected cell only, other cells unaffected

## Status Aggregation

**Spoke XR Status:**
```yaml
status:
  ready: true
  message: "All resources provisioned successfully"
  conditions:
    - type: Ready
      status: "True"
      reason: "AllResourcesReady"
      lastTransitionTime: "2026-04-27T10:00:00Z"
```

**Hub XR Status:**
```yaml
status:
  databaseReady: true  # From TenantDatabase XR
  environmentReady: true  # From SpokeTenantEnvironment XR
  conditions:
    - type: Ready
      status: "True"
      reason: "AllXRsReady"
      lastTransitionTime: "2026-04-27T10:00:30Z"
```

## Migration Path

**Phase 1:** Deploy `SpokeTenantEnvironment` XRD and Composition to Spokes (no impact on existing tenants)

**Phase 2:** Update Hub Composition to add `SpokeTenantEnvironment` Object MR (existing resources still provisioned)

**Phase 3:** Test new tenant with both patterns active (validate Spoke Composition works)

**Phase 4:** Remove 9 Object MRs from Hub Composition (new tenants use Spoke Composition only)

**Phase 5:** Existing tenants continue with old pattern until next update (gradual migration)

## Validation Strategy

**Unit Testing:** Manual validation at each phase (no automated tests per requirements)

**Integration Testing:** Deploy test tenant, verify all resources created, verify status propagation

**Rollout Testing:** Update Composition on one Spoke, verify isolation from other Spokes

**Rollback Testing:** Revert Composition, verify tenants recover

**Performance Testing:** Measure Hub ETCD size before/after, verify >80% reduction
