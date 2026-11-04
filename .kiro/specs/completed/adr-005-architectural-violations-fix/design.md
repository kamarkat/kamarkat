# ADR 005 Architectural Violations Bugfix Design

## Overview

This bugfix eliminates architectural violations of ADR 005 (Hub-Spoke Crossplane Composition Pattern) by consolidating tenant abstraction into a single Crossplane Composition. Currently, tenant provisioning splits across ArgoCD (deploying raw K8s resources to Spoke) and Crossplane (deploying XRs to Hub), causing operational failures including duplicate resource provisioning, garbage collection breaking shared infrastructure, and massive ConfigMap pushes.

The fix moves ALL Spoke resource provisioning into the `ainativesaas-starter-hetzner` Composition by adding 5 new Object MRs (Namespace, ServiceAccount, Role, RoleBinding, ResourceQuota) and updating AtlasMigration to pull from Git URL instead of ConfigMap. This ensures ArgoCD deploys ONLY the `AINativeSaaS` XR to Hub, and Crossplane handles all Spoke resource delivery.

## Glossary

- **Bug_Condition (C)**: The condition that triggers architectural violations - when ArgoCD deploys both XR to Hub AND raw K8s resources to Spoke via dual ApplicationSets
- **Property (P)**: The desired ADR 005 compliant behavior - ArgoCD deploys ONLY XR to Hub, Crossplane provisions ALL Spoke resources via Composition
- **Preservation**: Existing tenant functionality (database provisioning, pooler, PostgREST, migrations) that must remain unchanged
- **Object MR**: `kubernetes.crossplane.io/v1alpha2/Object` Managed Resource that wraps any K8s resource for Hub→Spoke delivery
- **AINativeSaaS XR**: Composite Resource representing complete tenant environment (database, pooler, PostgREST, namespace, RBAC)
- **ApplicationSet**: ArgoCD resource that generates Applications from Git repository patterns
- **Spoke Pool**: Shared Kubernetes cluster hosting multiple Starter tier tenants (namespace isolation)
- **Hub Crossplane**: Crossplane instance running on Hub cluster that reconciles XRs and provisions Spoke resources

## Bug Details

### Bug Condition

The bug manifests when a tenant is provisioned via the `universal-tenant` Helm chart. The current implementation violates ADR 005 by splitting tenant abstraction across two delivery mechanisms: ArgoCD deploys raw Kubernetes primitives directly to Spoke, while Crossplane deploys the `AINativeSaaS` XR to Hub. This causes duplicate resource provisioning attempts, garbage collection breaking shared infrastructure, and ConfigMap bloat.

**Formal Specification:**
```
FUNCTION isBugCondition(input)
  INPUT: input of type TenantProvisioningRequest
  OUTPUT: boolean
  
  RETURN input.hasSecondApplicationSet = true
         AND input.secondApplicationSetDeploysRawK8sToSpoke = true
         AND input.migrationsDeliveredViaConfigMap = true
         AND NOT input.allSpokeResourcesInComposition = true
END FUNCTION
```

### Examples

- **Abstraction Leakage**: Tenant `acme-corp` provisioning triggers ApplicationSet 1 (deploys `AINativeSaaS` XR to Hub namespace `tenant-acme-corp`) AND ApplicationSet 2 (deploys Namespace, RBAC, ResourceQuota, ConfigMap to Spoke namespace `tenant-acme-corp`). Expected: ONLY ApplicationSet 1 deploys XR, Composition provisions Spoke resources.

- **ConfigMap Bloat**: Tenant `widgets-inc` with 50 SQL migration files causes Helm to glob all files into ConfigMap `tenant-widgets-inc-migrations` (500KB+), requiring ArgoCD to push massive ConfigMap to Spoke. Expected: AtlasMigration pulls from Git URL `https://github.com/kamarkat/kamarkat.git` path `migrations/tenant-baseline`.

- **Missing XRD Fields**: Tenant `beta-saas` requires ResourceQuota (cpu: 2000m, memory: 4Gi) but XRD lacks `spec.resourceQuota` field, forcing values into Helm template instead of XR spec. Expected: XRD contains `spec.resourceQuota` mapped from `values.yaml`.

- **Edge Case - Namespace Collision**: If Composition adds Namespace Object MR but ApplicationSet 2 still exists, both attempt to create `tenant-acme-corp` namespace on Spoke, causing conflict. Expected: ApplicationSet 2 deleted, ONLY Composition creates namespace.

## Expected Behavior

### Preservation Requirements

**Unchanged Behaviors:**
- Existing `TenantDatabase` XR provisioning (PostgreSQL cluster, role, grants, secrets) must continue working
- CNPG Pooler connection pooling with tenant-specific credentials must remain functional
- PostgREST deployment with JWT authentication must continue serving REST APIs
- Spoke Controller status sync (Crossplane claim conditions → Hub Centralised DB via PostgREST) must remain operational
- Grafana Alloy metrics collection (remote_write → VictoriaMetrics) must continue
- NATS Leaf Node event forwarding (billing/lifecycle → Hub JetStream) must remain functional

**Scope:**
All inputs that do NOT involve tenant provisioning should be completely unaffected by this fix. This includes:
- Spoke Pool cluster provisioning (SpokePool XR with cluster-wide certs)
- Hub platform services (VictoriaMetrics, Loki, ArgoCD, Crossplane)
- Existing tenant workloads already deployed

## Hypothesized Root Cause

Based on the bug analysis, the root causes are:

1. **Dual ApplicationSet Anti-Pattern**: The `platform-tenant-applicationset.yaml` contains TWO ApplicationSets:
   - `tenant-xr-provisioning` (deploys XR to Hub) - CORRECT
   - `tenant-spoke-provisioning` (deploys raw K8s to Spoke) - VIOLATES ADR 005
   
   The second ApplicationSet was likely added as a workaround when the Composition lacked Namespace/RBAC provisioning.

2. **Incomplete Composition**: The `ainativesaas-starter-hetzner.yaml` Composition contains only 6 resources (TenantDatabase XR, Pooler, Pooler Secret, PostgREST Deployment, PostgREST Service, AtlasMigration). It lacks Namespace, ServiceAccount, Role, RoleBinding, ResourceQuota Object MRs.

3. **ConfigMap Migration Delivery**: The `configmap-migrations.yaml` template globs SQL files into ConfigMap, and AtlasMigration uses `spec.dir.configMapRef` instead of `spec.dir.url`. This was likely implemented before Git-native migration support was available.

4. **Missing XRD Schema Fields**: The `ainativesaas-v1.yaml` XRD lacks `spec.resourceQuota` and `spec.ownerEmail` fields, preventing Helm values from being mapped to XR spec.

## Correctness Properties

Property 1: Bug Condition - ADR 005 Compliance

_For any_ tenant provisioning request where the bug condition holds (dual ApplicationSets exist, ConfigMap migrations used, Composition lacks Spoke resources), the fixed system SHALL deploy ONLY the `AINativeSaaS` XR to Hub via ArgoCD, and Crossplane SHALL provision ALL Spoke resources (Namespace, RBAC, ResourceQuota, Database, Pooler, PostgREST, AtlasMigration with Git URL) via the Composition, eliminating abstraction leakage and ConfigMap bloat.

**Validates: Requirements 2.1, 2.2, 2.5, 2.6, 2.7**

Property 2: Preservation - Existing Tenant Functionality

_For any_ existing tenant workload or platform service that does NOT involve new tenant provisioning, the fixed system SHALL produce exactly the same behavior as the original system, preserving database provisioning, connection pooling, REST API serving, status sync, observability, and event forwarding.

**Validates: Requirements 3.1, 3.2, 3.3, 3.4, 3.5, 3.6, 3.7**

## Observability & Status Aggregation Strategy

### XR Status Derivation

The `AINativeSaaS` XR status is derived from a mix of Native MRs (via TenantDatabase XR) and Object MRs (Namespace, RBAC, Pooler, PostgREST, AtlasMigration). The status aggregation strategy is:

**Status Sources:**
1. **TenantDatabase XR (Native MR)**: Provides rich, strongly-typed status (databaseReady, roleReady, grantsReady)
2. **Object MRs**: Provide generic Kubernetes status (Ready condition based on manifest applied)

**CRITICAL - Object MR Readiness Caveat:**
The `Object` MR `Ready=True` condition means "Crossplane successfully applied YAML to Spoke API server", NOT "the underlying resource finished reconciling". For example, `atlasmigration` Object MR reports `Ready=True` immediately after creating the `AtlasMigration` CR on Spoke, even if Atlas migrations are still running or failed.

**Solution - ToCompositeFieldPath Status Mapping:**
To ensure XR status reflects TRUE readiness of Spoke resources, use `ToCompositeFieldPath` patches to extract status from the nested manifest back to the XR:

```yaml
# Example: AtlasMigration Object MR
patches:
  - type: ToCompositeFieldPath
    fromFieldPath: status.atProvider.manifest.status.conditions[?(@.type=="Ready")].status
    toFieldPath: status.migrationsApplied
    transforms:
      - type: map
        map:
          "True": "true"
          "False": "false"
```

This extracts the ACTUAL `AtlasMigration` CR status (from Atlas Operator on Spoke) and propagates it to the XR, ensuring the Hub XR only reports Ready when Spoke migrations actually complete.

**Aggregation Logic:**
```yaml
status:
  conditions:
    - type: Ready
      status: "True"  # When ALL resources report Ready
      reason: "AllResourcesReady"
  databaseReady: <from TenantDatabase XR status via FromCompositeFieldPath>
  poolerReady: <from pooler Object MR via ToCompositeFieldPath from Spoke Pooler CR status>
  postgrestReady: <from postgrest-deployment Object MR via ToCompositeFieldPath from Spoke Deployment status>
  migrationsApplied: <from atlasmigration Object MR via ToCompositeFieldPath from Spoke AtlasMigration CR status>
```

**Readiness Criteria:**
- XR is Ready when: `databaseReady AND poolerReady AND postgrestReady AND migrationsApplied`
- If any Object MR fails to apply (e.g., namespace conflict), XR status shows `Ready=False` with reason from failed resource
- TenantDatabase XR failure propagates detailed error (e.g., "PostgreSQL role creation failed: permission denied")
- **If Spoke resource fails** (e.g., Atlas migration fails), ToCompositeFieldPath patch propagates failure to XR status

**Debugging Strategy:**
- Check XR status: `kubectl get ainativesaas {tenant} -o yaml | yq .status`
- Drill into TenantDatabase: `kubectl get tenantdatabase {tenant} -o yaml | yq .status` (rich status)
- Drill into Object MRs: `kubectl get object {tenant}-namespace -o yaml | yq .status` (generic status)

### Dependency Handling Between Native MR ↔ Object

**Dependency Flow:**
1. **TenantDatabase XR** (Native MR) provisions PostgreSQL database, role, grants → produces connection secret `tenant-{id}-db-credentials`
2. **Pooler Object MR** consumes connection secret via `spec.pgbouncer.authQueryUser.secretRef`
3. **PostgREST Object MR** consumes pooler connection secret via `env.PGRST_DB_URI.secretKeyRef`
4. **AtlasMigration Object MR** consumes pooler connection secret via `spec.urlFrom.secretKeyRef`

**Ordering Guarantees:**
- Crossplane reconciles resources in declaration order (TenantDatabase first, then Pooler, then PostgREST, then AtlasMigration)
- Object MRs with `secretKeyRef` dependencies will fail with `SecretNotFound` error if TenantDatabase hasn't created secret yet
- Crossplane retries failed Object MRs automatically (exponential backoff, max 5 minutes)

**Retry Behavior:**
- TenantDatabase XR failure: Crossplane retries every 1 minute until database provisioned
- Object MR failure (missing secret): Crossplane retries every 30 seconds until secret exists
- Network failure (Hub → Spoke): Crossplane retries with exponential backoff (1s, 2s, 4s, 8s, 16s, 32s, 60s max)

**Failure Modes:**
1. **TenantDatabase provisioning fails**: XR status shows `databaseReady=false`, downstream Object MRs remain in `Pending` state
2. **Pooler Object MR fails (missing secret)**: XR status shows `poolerReady=false`, PostgREST and AtlasMigration remain `Pending`
3. **Spoke cluster unreachable**: All Object MRs show `SyncFailed` status, Crossplane retries until connectivity restored

**Explicit Dependency Declaration:**
- Use `readinessChecks` in Object MRs to wait for specific conditions:
  ```yaml
  readinessChecks:
    - type: MatchCondition
      matchCondition:
        type: Ready
        status: "True"
  ```
- TenantDatabase XR already has readiness checks in composition (line 30 in design spec)

### When NOT to Use Object MRs

**Avoid Object MRs for:**
1. **Core infrastructure where status matters**: Use Native MRs (e.g., AWS RDS, GCP CloudSQL) for rich status propagation
2. **Resources requiring lifecycle hooks**: Use Native MRs if you need `PreDelete` hooks or complex deletion logic
3. **Resources with complex dependencies**: Use Native MRs if resource depends on multiple other resources with ordering constraints
4. **Hub-local resources**: Never wrap Hub resources in Object MRs (e.g., Hub Crossplane Providers, Hub ArgoCD Applications)

**Use Object MRs for:**
1. **Remote Spoke delivery**: Deploying any resource from Hub to Spoke cluster (Namespace, RBAC, CNPG Cluster, ESO ExternalSecret)
2. **Operator-managed resources**: Resources where Spoke operators handle reconciliation (CNPG, ESO, Kyverno, Atlas)
3. **Simple Kubernetes primitives**: Namespace, ConfigMap, Secret, Service, Deployment (where generic status is sufficient)

**Guardrails:**
- **Rule 1**: If resource is on Hub cluster, use Native MR (never Object)
- **Rule 2**: If resource requires rich status (e.g., connection details, endpoint URLs), use Native MR
- **Rule 3**: If resource is on Spoke cluster AND operator handles reconciliation, use Object MR
- **Rule 4**: If unsure, default to Native MR (can always refactor to Object later)

## Fix Implementation

### Changes Required

Assuming our root cause analysis is correct:

**File 1**: `xrds/definitions/ainativesaas-v1.yaml`

**Function**: Add missing XRD schema fields

**Specific Changes**:
1. **Add `spec.resourceQuota` field**: Insert after `spec.postgrest` field
   ```yaml
   resourceQuota:
     type: object
     description: "Namespace resource quota limits"
     properties:
       cpu:
         type: string
         description: "CPU limit (e.g., '1000m', '2')"
         pattern: '^[0-9]+m?$'
         default: "1000m"
       memory:
         type: string
         description: "Memory limit (e.g., '2Gi', '512Mi')"
         pattern: '^[0-9]+[EPTGMK]i?$'
         default: "2Gi"
       storage:
         type: string
         description: "Storage limit (e.g., '10Gi')"
         pattern: '^[0-9]+[EPTGMK]i?$'
         default: "10Gi"
       pods:
         type: string
         description: "Max pods (e.g., '10')"
         pattern: '^[0-9]+$'
         default: "10"
   ```

2. **Add `spec.ownerEmail` field**: Insert after `spec.resourceQuota` field
   ```yaml
   ownerEmail:
     type: string
     description: "Tenant owner email for notifications"
     pattern: '^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$'
   ```

**File 2**: `manifests/tenants/charts/universal-tenant/templates/ainativesaas.yaml`

**Function**: Map Helm values to XR spec fields

**Specific Changes**:
1. **Add resourceQuota mapping**: Insert after `postgrest` spec block
   ```yaml
   {{- if .Values.resourceQuota }}
   resourceQuota:
     cpu: {{ .Values.resourceQuota.cpu | default "1000m" }}
     memory: {{ .Values.resourceQuota.memory | default "2Gi" }}
     storage: {{ .Values.resourceQuota.storage | default "10Gi" }}
     pods: {{ .Values.resourceQuota.pods | default "10" | quote }}
   {{- end }}
   ```

2. **Add ownerEmail mapping**: Insert after `resourceQuota` block
   ```yaml
   {{- if .Values.ownerEmail }}
   ownerEmail: {{ .Values.ownerEmail }}
   {{- end }}
   ```

**File 3**: `xrds/compositions/ainativesaas-starter-hetzner.yaml`

**Function**: Add 5 new Object MRs for Spoke resource provisioning, update AtlasMigration

**Specific Changes**:

1. **Add Namespace Object MR**: Insert as resource #7 (after atlasmigration)
   ```yaml
   - name: namespace
     base:
       apiVersion: kubernetes.crossplane.io/v1alpha2
       kind: Object
       spec:
         managementPolicies:
           - "*"
         forProvider:
           manifest:
             apiVersion: v1
             kind: Namespace
             metadata:
               name: ""  # Patched from tenantId
               labels:
                 tenant-id: ""
                 tier: ""
                 managed-by: crossplane
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
           fmt: "%s-namespace"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.name
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.labels['tenant-id']
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tier
       toFieldPath: spec.forProvider.manifest.metadata.labels['tier']
   ```

2. **Add ServiceAccount Object MR**: Insert as resource #8
   ```yaml
   - name: service-account
     base:
       apiVersion: kubernetes.crossplane.io/v1alpha2
       kind: Object
       spec:
         managementPolicies:
           - "*"
         forProvider:
           manifest:
             apiVersion: v1
             kind: ServiceAccount
             metadata:
               name: ""  # Patched from tenantId
               namespace: ""  # Patched from tenantId
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
           fmt: "%s-serviceaccount"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.name
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.namespace
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
   ```

3. **Add Role Object MR**: Insert as resource #9
   ```yaml
   - name: role
     base:
       apiVersion: kubernetes.crossplane.io/v1alpha2
       kind: Object
       spec:
         managementPolicies:
           - "*"
         forProvider:
           manifest:
             apiVersion: rbac.authorization.k8s.io/v1
             kind: Role
             metadata:
               name: ""  # Patched from tenantId
               namespace: ""  # Patched from tenantId
             rules:
             - apiGroups: [""]
               resources: ["pods", "services", "configmaps", "secrets"]
               verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
             - apiGroups: ["apps"]
               resources: ["deployments", "statefulsets"]
               verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
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
           fmt: "%s-role"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.name
       transforms:
       - type: string
         string:
           fmt: "tenant-%s-role"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.namespace
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
   ```

4. **Add RoleBinding Object MR**: Insert as resource #10
   ```yaml
   - name: role-binding
     base:
       apiVersion: kubernetes.crossplane.io/v1alpha2
       kind: Object
       spec:
         managementPolicies:
           - "*"
         forProvider:
           manifest:
             apiVersion: rbac.authorization.k8s.io/v1
             kind: RoleBinding
             metadata:
               name: ""  # Patched from tenantId
               namespace: ""  # Patched from tenantId
             roleRef:
               apiGroup: rbac.authorization.k8s.io
               kind: Role
               name: ""  # Patched from tenantId
             subjects:
             - kind: ServiceAccount
               name: ""  # Patched from tenantId
               namespace: ""  # Patched from tenantId
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
           fmt: "%s-rolebinding"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.name
       transforms:
       - type: string
         string:
           fmt: "tenant-%s-binding"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.namespace
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.roleRef.name
       transforms:
       - type: string
         string:
           fmt: "tenant-%s-role"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.subjects[0].name
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.subjects[0].namespace
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
   ```

5. **Add ResourceQuota Object MR**: Insert as resource #11
   ```yaml
   - name: resource-quota
     base:
       apiVersion: kubernetes.crossplane.io/v1alpha2
       kind: Object
       spec:
         managementPolicies:
           - "*"
         forProvider:
           manifest:
             apiVersion: v1
             kind: ResourceQuota
             metadata:
               name: ""  # Patched from tenantId
               namespace: ""  # Patched from tenantId
             spec:
               hard:
                 requests.cpu: ""     # Patched from resourceQuota.cpu
                 requests.memory: ""  # Patched from resourceQuota.memory
                 requests.storage: "" # Patched from resourceQuota.storage
                 pods: ""             # Patched from resourceQuota.pods
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
           fmt: "%s-resourcequota"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.name
       transforms:
       - type: string
         string:
           fmt: "tenant-%s-quota"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.namespace
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.resourceQuota.cpu
       toFieldPath: spec.forProvider.manifest.spec.hard['requests.cpu']
     - type: FromCompositeFieldPath
       fromFieldPath: spec.resourceQuota.memory
       toFieldPath: spec.forProvider.manifest.spec.hard['requests.memory']
     - type: FromCompositeFieldPath
       fromFieldPath: spec.resourceQuota.storage
       toFieldPath: spec.forProvider.manifest.spec.hard['requests.storage']
     - type: FromCompositeFieldPath
       fromFieldPath: spec.resourceQuota.pods
       toFieldPath: spec.forProvider.manifest.spec.hard['pods']
   ```

6. **Update AtlasMigration to use Git URL with credentials AND add status mapping**: Replace existing `atlasmigration` resource (resource #6)
   
   **CRITICAL**: The zero-ops repository is PRIVATE. Currently, migrations work because ArgoCD globs SQL files into a ConfigMap and pushes it to Spoke. After switching to Git-native pulls, AtlasMigration needs GitHub PAT credentials to access the private repo.
   
   **CRITICAL - Status Mapping**: Object MR `Ready=True` only means "YAML applied to Spoke", NOT "migrations finished". Must add `ToCompositeFieldPath` patch to extract ACTUAL AtlasMigration CR status from Spoke.
   
   ```yaml
   - name: atlasmigration
     base:
       apiVersion: kubernetes.crossplane.io/v1alpha2
       kind: Object
       spec:
         managementPolicies:
           - "*"
         forProvider:
           manifest:
             apiVersion: db.atlasgo.io/v1alpha1
             kind: AtlasMigration
             metadata:
               namespace: ""  # Patched
             spec:
               urlFrom:
                 secretKeyRef:
                   name: ""  # Patched: <tenantId>-pooler-app
                   key: url
               dir:
                 url: "https://github.com/kamarkat/kamarkat.git"
                 ref: "main"
                 path: ""  # Patched from database.migrations.baseline
                 credentials:
                   secretRef:
                     name: github-migrations-pat  # ExternalSecret pulls from Infisical
                     key: token
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
           fmt: "%s-atlasmigration"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.name
       transforms:
       - type: string
         string:
           fmt: "%s-migrations"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.namespace
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.spec.urlFrom.secretKeyRef.name
       transforms:
       - type: string
         string:
           fmt: "%s-pooler-app"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.database.migrations.baseline
       toFieldPath: spec.forProvider.manifest.spec.dir.path
     # CRITICAL: Extract ACTUAL AtlasMigration status from Spoke back to XR
     - type: ToCompositeFieldPath
       fromFieldPath: status.atProvider.manifest.status.conditions[?(@.type=="Ready")].status
       toFieldPath: status.migrationsApplied
       transforms:
         - type: map
           map:
             "True": "true"
             "False": "false"
   ```

7. **Add GitHub PAT ExternalSecret for Atlas migrations**: Insert as resource #12
   ```yaml
   - name: github-migrations-pat
     base:
       apiVersion: kubernetes.crossplane.io/v1alpha2
       kind: Object
       spec:
         managementPolicies:
           - "*"
         forProvider:
           manifest:
             apiVersion: external-secrets.io/v1
             kind: ExternalSecret
             metadata:
               name: github-migrations-pat
               namespace: ""  # Patched from tenantId
             spec:
               refreshInterval: 1h
               secretStoreRef:
                 name: infisical-backend
                 kind: ClusterSecretStore
               target:
                 name: github-migrations-pat
                 creationPolicy: Owner
               data:
                 - secretKey: token
                   remoteRef:
                     key: github-migrations-pat
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
           fmt: "%s-github-pat"
     - type: FromCompositeFieldPath
       fromFieldPath: spec.tenantId
       toFieldPath: spec.forProvider.manifest.metadata.namespace
       transforms:
       - type: string
         string:
           fmt: "tenant-%s"
   ```

**File 4**: `manifests/argocd/apps/platform-tenant-applicationset.yaml`

**Function**: Remove second ApplicationSet that deploys raw K8s to Spoke

**Specific Changes**:
1. **Delete ApplicationSet 2**: Remove lines 60-120 (entire `tenant-spoke-provisioning` ApplicationSet)
2. **Keep ApplicationSet 1**: Retain `tenant-xr-provisioning` ApplicationSet unchanged

**File 5**: `manifests/tenants/charts/universal-tenant/templates/namespace.yaml`

**Function**: DELETE - Namespace now provisioned by Composition

**File 6**: `manifests/tenants/charts/universal-tenant/templates/resourcequota.yaml`

**Function**: DELETE - ResourceQuota now provisioned by Composition

**File 7**: `manifests/tenants/charts/universal-tenant/templates/rbac.yaml`

**Function**: DELETE - RBAC now provisioned by Composition

**File 8**: `manifests/tenants/charts/universal-tenant/templates/configmap-migrations.yaml`

**Function**: DELETE - Migrations now pulled from Git URL

## Testing Strategy

### Validation Approach

The testing strategy follows a two-phase approach: first, surface counterexamples that demonstrate the bug on unfixed code, then verify the fix works correctly and preserves existing behavior.

### Exploratory Bug Condition Checking

**Goal**: Surface counterexamples that demonstrate the bug BEFORE implementing the fix. Confirm or refute the root cause analysis. If we refute, we will need to re-hypothesize.

**Test Plan**: Write tests that provision a test tenant using the UNFIXED code and assert that dual ApplicationSets exist, ConfigMap migrations are created, and Composition lacks Namespace/RBAC resources. Run these tests on the UNFIXED code to observe failures and understand the root cause.

**Test Cases**:
1. **Dual ApplicationSet Test**: Query ArgoCD for Applications with label `tenant-id=test-tenant-001`, assert TWO Applications exist (`test-tenant-001-xr` and `test-tenant-001-spoke`) (will fail on unfixed code - confirms dual ApplicationSet bug)
2. **ConfigMap Migration Test**: Query Spoke cluster for ConfigMap `tenant-test-tenant-001-migrations`, assert it exists and contains SQL files (will fail on unfixed code - confirms ConfigMap bug)
3. **Missing Composition Resources Test**: Query Spoke cluster for Namespace `tenant-test-tenant-001`, assert it was created by ArgoCD (label `app.kubernetes.io/managed-by: argocd`) NOT Crossplane (will fail on unfixed code - confirms Composition gap)
4. **XRD Schema Test**: Attempt to create `AINativeSaaS` XR with `spec.resourceQuota` field, assert validation error (will fail on unfixed code - confirms missing XRD field)

**Expected Counterexamples**:
- Two ArgoCD Applications exist for single tenant (abstraction leakage)
- ConfigMap contains SQL files instead of AtlasMigration using Git URL
- Namespace/RBAC created by ArgoCD instead of Crossplane Composition
- XRD rejects `resourceQuota` field as unknown

### Fix Checking

**Goal**: Verify that for all inputs where the bug condition holds, the fixed function produces the expected behavior.

**Pseudocode:**
```
FOR ALL input WHERE isBugCondition(input) DO
  result := provisionTenant_fixed(input)
  ASSERT (
    result.argocdApplicationCount = 1 AND
    result.argocdApplication.name = "{tenantId}-xr" AND
    result.spokeNamespaceCreatedBy = "crossplane" AND
    result.spokeRBACCreatedBy = "crossplane" AND
    result.spokeResourceQuotaCreatedBy = "crossplane" AND
    result.atlasMigrationUsesGitURL = true AND
    NOT result.configMapMigrationsExists
  )
END FOR
```

### Preservation Checking

**Goal**: Verify that for all inputs where the bug condition does NOT hold, the fixed function produces the same result as the original function.

**Pseudocode:**
```
FOR ALL input WHERE NOT isBugCondition(input) DO
  ASSERT provisionTenant_original(input) = provisionTenant_fixed(input)
END FOR
```

**Testing Approach**: Property-based testing is recommended for preservation checking because:
- It generates many test cases automatically across the input domain
- It catches edge cases that manual unit tests might miss
- It provides strong guarantees that behavior is unchanged for all non-buggy inputs

**Test Plan**: Observe behavior on UNFIXED code first for existing tenant operations (database queries, pooler connections, PostgREST API calls), then write property-based tests capturing that behavior.

**Test Cases**:
1. **Database Provisioning Preservation**: Observe that existing tenant `acme-corp` can connect to PostgreSQL via pooler on unfixed code, then write test to verify this continues after fix
2. **PostgREST API Preservation**: Observe that existing tenant `widgets-inc` can query PostgREST API on unfixed code, then write test to verify this continues after fix
3. **Status Sync Preservation**: Observe that Spoke Controller writes status to Hub Centralised DB on unfixed code, then write test to verify this continues after fix
4. **Observability Preservation**: Observe that Grafana Alloy scrapes metrics and remote_writes to VictoriaMetrics on unfixed code, then write test to verify this continues after fix

### Unit Tests

- Test XRD schema validation (resourceQuota field accepts valid values, rejects invalid patterns)
- Test Helm template rendering (resourceQuota values mapped to XR spec correctly)
- Test Composition patch transforms (tenantId → namespace name, cellId → providerConfigRef)
- Test ApplicationSet generator (only ONE Application created per tenant)

### Property-Based Tests

- Generate random tenant configurations (tenantId, tier, resourceQuota values) and verify Composition provisions all 11 resources correctly
- Generate random existing tenant workloads and verify database connections, API calls, status sync continue working
- Test that all non-provisioning operations (queries, updates, deletes) produce identical results before/after fix

### Integration Tests

- Test full tenant provisioning flow: Commit tenant values.yaml → ArgoCD syncs XR → Crossplane reconciles Composition → All 11 Spoke resources created → AtlasMigration pulls from Git → Database ready
- Test tenant deletion flow: Delete XR → Crossplane garbage-collects all 11 Spoke resources → Namespace deleted
- Test that cluster-wide resources (SpokePool certs) remain intact when tenant deleted (preservation of shared infrastructure)
