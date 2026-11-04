# ADR 005 Architectural Violations - Analysis Summary

## Current State Analysis (100% Clarity Achieved)

### 1. Abstraction Leakage Confirmed

**File:** `manifests/argocd/apps/platform-tenant-applicationset.yaml`

**Current Behavior:**
- **ApplicationSet 1 (`tenant-xr-provisioning`)**: Deploys `AINativeSaaS` XR to Hub (namespace: `tenant-{tenantId}`)
- **ApplicationSet 2 (`tenant-spoke-provisioning`)**: Deploys raw K8s resources to Spoke (namespace: `tenant-{tenantId}`)

**Resources Deployed by ApplicationSet 2 (Spoke):**
1. `Namespace` (tenant-{tenantId})
2. `ServiceAccount` (tenant-{tenantId})
3. `Role` (tenant-{tenantId}-role)
4. `RoleBinding` (tenant-{tenantId}-binding)
5. `ResourceQuota` (tenant-{tenantId}-quota)
6. `ConfigMap` (tenant-{tenantId}-migrations) - Contains SQL files

**Violation:** ArgoCD bypasses Crossplane by deploying infrastructure directly to Spoke. This splits the abstraction.

---

### 2. Scope Bleed Analysis - CORRECTED

**Initial Bug Report Claim:** Cluster-wide certs in `tenantdatabase-spoke.yaml`

**Actual Finding:** ✅ **NO SCOPE BLEED IN TENANT COMPOSITION**

**File:** `manifests/tenants/compositions/tenantdatabase-spoke.yaml`
- Contains ONLY tenant-scoped resources (Database, Role, Grant, DefaultPrivileges, Secrets)
- Does NOT contain cluster-wide cert distribution

**File:** `xrds/compositions/spokepool-hetzner.yaml`
- Contains cluster-wide cert distribution (lines 726-885)
- Certs are correctly scoped to `SpokePool` XR (cluster-level)
- Three certs distributed:
  1. `alloy-client-cert` (observability mTLS)
  2. `nats-leafnode-client-cert` (messaging mTLS)
  3. `argocd-agent-client-cert` (GitOps mTLS)

**Conclusion:** The scope bleed bug described in the initial report **does not exist**. Certs are already correctly placed in the cluster-scoped composition.

---

### 3. Migration Anti-Pattern Confirmed

**File:** `manifests/tenants/charts/universal-tenant/templates/configmap-migrations.yaml`

**Current Behavior:**
```yaml
{{- if not .Values.deployXR }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: tenant-{{ .Values.tenantId }}-migrations
data:
{{- range $path, $bytes := .Files.Glob "files/migrations/*.sql" }}
  {{ base $path }}: |
{{ $bytes | toString | indent 4 }}
{{- end }}
```

**Violation:** Helm globs SQL files into ConfigMap, requiring ArgoCD to push massive ConfigMaps to Spoke.

**File:** `xrds/compositions/ainativesaas-starter-hetzner.yaml` (Resource 6: atlasmigration)
```yaml
spec:
  dir:
    configMapRef:
      name: ""  # Patched from tenantId
```

**Expected:** AtlasMigration should use `spec.dir.url` to pull from Git:
```yaml
spec:
  dir:
    url: "https://github.com/kamarkat/kamarkat.git"
    ref: "main"
    path: "migrations/tenant-baseline"
```

---

### 4. Current XRD Schema

**File:** `xrds/definitions/ainativesaas-v1.yaml`

**Existing Fields:**
- `spec.tenantId` (string, required)
- `spec.tier` (enum: starter|enterprise, required)
- `spec.cellId` (string, required)
- `spec.database` (object, required)
  - `name` (string, required)
  - `migrations.baseline` (string, default: "migrations/tenant-baseline/")
  - `migrations.tenant` (string, default: "fleet-registry/tenants/{{tenantId}}/migrations/")
- `spec.pooler` (object, optional)
  - `maxConnections` (integer, default: 5)
  - `poolMode` (enum: transaction, default: transaction)
- `spec.postgrest` (object, optional)
  - `dbSchema` (string, default: "public")
  - `jwtCacheSize` (integer, default: 10000)

**Missing Fields (Need to Add):**
- `spec.resourceQuota` (object) - CPU, memory, storage, pods limits
- `spec.ownerEmail` (string) - Tenant owner contact
- `spec.argocdAgent` (object) - ArgoCD Agent configuration (if needed)

---

### 5. Current Helm Chart Values

**File:** `manifests/tenants/charts/universal-tenant/values.yaml`

**Existing Values:**
```yaml
tenantId: ""
tier: starter
region: hel1
database:
  schemaName: ""
  migrations:
    gitRepo: "https://github.com/kamarkat/kamarkat"
    gitRevision: "main"
    gitPath: "migrations/tenant-baseline"
resourceQuota:
  cpu: "1000m"
  memory: "2Gi"
  storage: "10Gi"
  pods: "10"
```

**Note:** `resourceQuota` exists in values.yaml but is NOT mapped to XR spec. It's used directly by the Helm template.

---

### 6. Current Composition Resources

**File:** `xrds/compositions/ainativesaas-starter-hetzner.yaml`

**Current Resources (6 total):**
1. `tenant-database-remote` - TenantDatabase XR (Object MR)
2. `pooler` - CNPG Pooler (Object MR)
3. `pooler-secret` - Pooler connection secret (Object MR)
4. `postgrest-deployment` - PostgREST Deployment (Object MR)
5. `postgrest-service` - PostgREST Service (Object MR)
6. `atlasmigration` - Atlas Migration CR (Object MR)

**Missing Resources (Need to Add):**
1. `namespace` - Namespace (Object MR)
2. `service-account` - ServiceAccount (Object MR)
3. `role` - Role (Object MR)
4. `role-binding` - RoleBinding (Object MR)
5. `resource-quota` - ResourceQuota (Object MR)

---

### 7. Provider-SQL Implementation Details

**Source:** `archived/cloud-native/provider-sql/`

**Key Findings:**
- Provider-SQL supports PostgreSQL, MySQL, MSSQL
- Managed Resources: `Database`, `Grant`, `DefaultPrivileges`, `Extension`, `Role`
- Connection via K8s Secret with fields: `username`, `password`, `endpoint`, `port`
- Reconciliation interval: 10 minutes (to reduce DB load)
- Used in `tenantdatabase-spoke.yaml` for:
  - `Role` (tenant user)
  - `Grant` (CONNECT privilege)
  - `DefaultPrivileges` (tables and sequences)

**Pattern:** Provider-SQL MRs are NOT wrapped in Object MRs because they are native Crossplane MRs running on Spoke Crossplane.

---

### 8. Crossplane v2.0 Pattern Validation

**Source:** `xrds/compositions/spokepool-hetzner.yaml`

**Confirmed Patterns:**
1. ✅ Native CAPI Cluster wrapped in Object MR (Hub → Spoke delivery)
2. ✅ Cert-manager Certificates on Hub (generate certs)
3. ✅ Object MRs with `references.patchesFrom` to copy Hub secrets to Spoke
4. ✅ ClusterResourceSet for Day-1 bootstrap (CNI, CCM, ArgoCD Agent)
5. ✅ ExternalSecret for credential management (Infisical)
6. ✅ PushSecret for credential backup (Hub → Infisical)

**Conclusion:** The codebase already follows Crossplane v2.0+ patterns correctly for cluster-scoped resources.

---

## Impacted Files Summary

### Files to DELETE (7 files):
1. ❌ `manifests/tenants/charts/universal-tenant/templates/namespace.yaml`
2. ❌ `manifests/tenants/charts/universal-tenant/templates/resourcequota.yaml`
3. ❌ `manifests/tenants/charts/universal-tenant/templates/rbac.yaml`
4. ❌ `manifests/tenants/charts/universal-tenant/templates/configmap-migrations.yaml`
5. ❌ Second ApplicationSet in `manifests/argocd/apps/platform-tenant-applicationset.yaml` (lines 60-120)

### Files to MODIFY (3 files):
1. ✏️ `xrds/definitions/ainativesaas-v1.yaml` - Add resourceQuota, ownerEmail fields
2. ✏️ `manifests/tenants/charts/universal-tenant/templates/ainativesaas.yaml` - Map resourceQuota to XR spec
3. ✏️ `xrds/compositions/ainativesaas-starter-hetzner.yaml` - Add 5 new Object MRs (Namespace, RBAC, ResourceQuota), update AtlasMigration to use Git URL

### Files UNCHANGED (2 files):
1. ✅ `manifests/tenants/compositions/tenantdatabase-spoke.yaml` - Already correct (no cluster-wide certs)
2. ✅ `xrds/compositions/spokepool-hetzner.yaml` - Already correct (cluster-wide certs in right place)

---

## Corrected Bug Condition

**Original Bug Report:**
```pascal
RETURN (
  X.hasSecondApplicationSet = true OR
  X.tenantCompositionContainsClusterScopedResources = true OR  // FALSE - This doesn't exist
  X.migrationsDeliveredViaConfigMap = true
)
```

**Corrected Bug Condition:**
```pascal
RETURN (
  X.hasSecondApplicationSet = true OR
  X.migrationsDeliveredViaConfigMap = true
)
```

**Removed:** `tenantCompositionContainsClusterScopedResources` - This violation does not exist in the codebase.

---

## Design Phase Readiness

✅ **100% Clarity Achieved**

**Ready to proceed with design spec:**
1. Understand all impacted files
2. Validated Crossplane v2.0 patterns in codebase
3. Confirmed provider-sql implementation details
4. Identified exact resources to add/delete/modify
5. Corrected bug condition based on actual codebase state

**Next Step:** Create design.md with detailed implementation plan.
