# Hub Operator Troubleshooting

## Issue 1: CNPG Scheme Registration Error

**Date:** 2026-04-03  
**Status:** Fixed

### Problem
Operator pods were running but showing error:
```
no kind is registered for the type v1.Cluster in scheme
```

### Root Cause
CNPG API types were not registered in the operator's scheme, preventing the operator from watching CNPG Cluster resources.

### Fix
Added CNPG scheme registration in `operators/hub-operator/cmd/main.go`:

```go
import (
    cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

func init() {
    utilruntime.Must(cnpgv1.AddToScheme(scheme))
}
```

**Commit:** [hash from git log]

---

## Issue 2: ResourceVersion Error on Secret Creation

**Date:** 2026-04-03  
**Status:** Fixed

### Problem
Operator was failing to create secrets with error:
```
resourceVersion should not be set on objects to be created
```

### Root Cause
The `generateOrReuse*` functions in `operators/hub-operator/internal/secrets/generator.go` were returning existing secret objects (which include `resourceVersion` metadata) when secrets already existed. The controller then tried to `Create()` these objects, which is invalid because `resourceVersion` should only be set on existing resources, not new ones.

### Fix
Modified all `generateOrReuse*` functions to return `nil` when a secret already exists, instead of returning the existing secret object:

```go
func generateOrReuseInfisicalSecrets(..., existing *corev1.Secret) (*corev1.Secret, error) {
    if existing != nil {
        // Return nil to indicate secret already exists (don't try to create)
        return nil, nil
    }
    return GenerateInfisicalSecrets(...)
}
```

Applied to:
- `generateOrReuseInfisicalSecrets`
- `generateOrReusePlatformDBApp`
- `generateOrReuseInfisicalDBCredentials`
- `generateOrReuseOryDBCredentials`

The controller's existing logic already handles `nil` secrets correctly by skipping them in the creation loop.

**Commit:** cb03f59

---

## Issue 3: Nil Pointer Dereference in Secret Generation

**Date:** 2026-04-03  
**Status:** Fixed

### Problem
Operator was panicking with:
```
panic: runtime error: invalid memory address or nil pointer dereference
at generator.go:311
```

### Root Cause
When `infisical-db-credentials` already existed, `generateOrReuseInfisicalDBCredentials` returned `nil`. The code then tried to access `infisicalDBCreds.Data["username"]` causing a nil pointer dereference.

### Fix
Modified `GenerateSecretZero` to handle nil returns by reading credentials from existing secrets:

```go
var username, password string
if infisicalDBCreds != nil {
    username = string(infisicalDBCreds.Data["username"])
    password = string(infisicalDBCreds.Data["password"])
} else if existing, ok := existingSecrets["infisical-db-credentials"]; ok {
    username = string(existing.Data["username"])
    password = string(existing.Data["password"])
} else {
    return nil, fmt.Errorf("infisical-db-credentials not found")
}
```

**Commit:** 700d77b

---

## Issue 4: ResourceVersion Error on platform-db-ca

**Date:** 2026-04-03  
**Status:** Fixed

### Problem
After fixing Issue 3, operator still failing with:
```
Failed to create secret platform-db-ca: resourceVersion should not be set on objects to be created
```

### Root Cause
When `platform-db-ca` already existed, `GenerateSecretZero` was setting `result.PlatformDBCA = existing`, which included the existing secret's `resourceVersion`. The controller then tried to `Create()` this object.

### Fix
Modified `GenerateSecretZero` to return `nil` for `PlatformDBCA` when it already exists:

```go
if existing, ok := existingSecrets["platform-db-ca"]; ok {
    // Reuse existing CA certificate - return nil to skip creation
    result.PlatformDBCA = nil
    caCert := existing.Data["ca.crt"]
    // ... continue with existing CA
}
```

**Commit:** ef55738

---

## Issue 5: Database Connection with CNPG Wildcard

**Date:** 2026-04-04  
**Status:** Fixed

### Problem
Operator failing to connect to database for migrations:
```
failed to ping database: pq: database "*" does not exist (3D000)
```

### Root Cause
CNPG creates `platform-db-superuser` secret with `dbname: *` (wildcard meaning "all databases"). The migrator and role manager read this value and try to connect to a database literally named `*`, which doesn't exist.

### Fix
Added fallback logic in both database connection functions to use `postgres` database when encountering the wildcard:

**File:** `operators/hub-operator/internal/database/migrator.go`
```go
dbname := string(secret.Data["dbname"])

// Handle CNPG wildcard database name
// CNPG uses "*" to denote superuser access to all databases
if dbname == "*" || dbname == "" {
    dbname = "postgres"
}
```

**File:** `operators/hub-operator/internal/database/roles.go`
```go
dbname := string(secret.Data["dbname"])

// Handle CNPG wildcard database name
// CNPG uses "*" to denote superuser access to all databases
if dbname == "*" || dbname == "" {
    dbname = "postgres"
}
```

**Commit:** b7ca8f3

---

## Issue 6: Missing ArgoCD Sync-Wave Annotations

**Date:** 2026-04-04  
**Status:** Fixed

### Problem
Several ArgoCD applications missing `sync-wave` annotations, causing them to deploy in wave 0 before dependencies are ready:
- `mcp-server` (no annotation)
- `kratos-selfservice-ui` (no annotation)

### Fix
Added sync-wave annotations to ensure proper deployment order:

**File:** `manifests/hub-core-services/platform-identity/argocd/mcp-server.yaml`
```yaml
metadata:
  name: mcp-server
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "4"
```

**File:** `manifests/hub-core-services/platform-identity/argocd/kratos-ui.yaml`
```yaml
metadata:
  name: kratos-selfservice-ui
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "4"
```

**Commit:** b7ca8f3

---

## Issue 7: ArgoCD OutOfSync Applications

**Date:** 2026-04-04  
**Status:** Fixed

### Problem
Applications stuck OutOfSync/Degraded due to field ownership conflicts with CRDs and ESO-managed secrets.

### Fix
Added `ServerSideApply=true` and `RespectIgnoreDifferences=true` to sync options:

**File:** `manifests/argocd/apps/platform-external-secrets.yaml`
```yaml
syncPolicy:
  automated:
    prune: true
    selfHeal: true
  syncOptions:
    - CreateNamespace=true
    - ServerSideApply=true
    - RespectIgnoreDifferences=true
```

**Commit:** b7ca8f3

---
## Issue 8: Missing infisical-redis-credentials Secret

**Date:** 2026-04-04  
**Status:** Fixed

### Problem
Redis pod stuck in `CreateContainerConfigError`:
```
Error: secret "infisical-redis-credentials" not found
```

Infisical deployment blocked because Redis couldn't start.

### Root Cause
The operator's `GenerateSecretZero()` function only generated `infisical-secrets` (which contains `REDIS_URL` with embedded password) but never created the separate `infisical-redis-credentials` secret that the Redis StatefulSet expects.

The old CLI explicitly created both secrets:
1. `infisical-secrets` - Contains `REDIS_URL` for Infisical to connect to Redis
2. `infisical-redis-credentials` - Contains `password` key for Redis pod itself

### Fix
Modified secret generation to create both secrets:

**File:** `operators/hub-operator/internal/secrets/generator.go`

1. Changed `GenerateInfisicalSecrets()` to return both the secret and the Redis password:
```go
type GenerateInfisicalSecretsResult struct {
    InfisicalSecrets *corev1.Secret
    RedisPassword    string
}

func GenerateInfisicalSecrets(...) (*GenerateInfisicalSecretsResult, error) {
    // ... generate secrets ...
    return &GenerateInfisicalSecretsResult{
        InfisicalSecrets: secret,
        RedisPassword:    redisPassword,
    }, nil
}
```

2. Added new function to generate Redis credentials:
```go
func GenerateInfisicalRedisCredentials(namespace, redisPassword string, owner metav1.OwnerReference) (*corev1.Secret, error) {
    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "infisical-redis-credentials",
            Namespace: namespace,
            OwnerReferences: []metav1.OwnerReference{owner},
            Labels: map[string]string{
                "app.kubernetes.io/managed-by": "zero-ops-hub-operator",
                "app.kubernetes.io/component":  "secret-zero",
            },
        },
        Type: corev1.SecretTypeOpaque,
        StringData: map[string]string{
            "password": redisPassword,
        },
    }
    return secret, nil
}
```

3. Updated `GenerateSecretZero()` to call both functions and added `InfisicalRedisCredentials` to result struct.

4. Updated controller Phase 1 to create the Redis credentials secret.

**Commit:** [current]

---

## Issue 9: Secret Naming Convention Mismatch (CRITICAL)

**Date:** 2026-04-04  
**Status:** Fixed

### Problem
Phase 2b (Database Roles) failing with:
```
failed to get secret mcp_server-db-credentials: secrets "mcp_server-db-credentials" not found
```

### Root Cause Analysis

**The Three-Way Naming Conflict:**

1. **HubEnvironment CR** defines roles with underscores (valid PostgreSQL identifiers):
   ```yaml
   roles:
   - name: mcp_server
   - name: spoke_controller
   ```

2. **Kubernetes Secret Names** cannot contain underscores (RFC 1123 DNS subdomain):
   - Invalid: `mcp_server-db-credentials`
   - Valid: `control-plane-db-credentials`

3. **ExternalSecrets** expect specific hyphenated names:
   ```yaml
   name: control-plane-db-credentials  # for mcp_server role
   name: hub-db-credentials            # for spoke_controller role
   ```

4. **PostgreSQL Roles** need underscores (SQL identifiers):
   ```sql
   CREATE ROLE mcp_server WITH LOGIN PASSWORD '...';
   ```

**The Operator's Bug:**
The operator was dynamically constructing secret names like this:
```go
secretName := fmt.Sprintf("%s-db-credentials", roleSpec.Name)
// Result: "mcp_server-db-credentials" (INVALID K8s name)
```

This created secrets with invalid names that:
- Kubernetes accepted (but shouldn't have)
- ExternalSecrets couldn't find (looking for `control-plane-db-credentials`)
- Phase 2b couldn't read (looking for the wrong name)

### Fix

Added `mapRoleToSecretName()` function to handle the mapping:

```go
func mapRoleToSecretName(roleName string) string {
    switch roleName {
    case "mcp_server":
        return "control-plane-db-credentials"
    case "spoke_controller":
        return "hub-db-credentials"
    default:
        // Replace underscores with hyphens for valid K8s names
        return strings.ReplaceAll(roleName, "_", "-") + "-db-credentials"
    }
}
```

**Applied in three locations:**

1. **Secret Generation** (`operators/hub-operator/internal/secrets/generator.go`):
   ```go
   func GenerateRoleDBCredentials(roleName, namespace string, owner metav1.OwnerReference) (*corev1.Secret, error) {
       secretName := mapRoleToSecretName(roleName)
       secret := &corev1.Secret{
           ObjectMeta: metav1.ObjectMeta{
               Name: secretName,  // Uses mapped name
               // ...
           },
           StringData: map[string]string{
               "username": roleName,  // PostgreSQL role keeps underscores
               "password": password,
           },
       }
       return secret, nil
   }
   ```

2. **Database Role Creation** (`operators/hub-operator/internal/database/roles.go`):
   ```go
   func (rm *RoleManager) CreateOrUpdateRoles(...) error {
       for _, roleSpec := range hubEnv.Spec.Database.Roles {
           secretName := mapRoleToSecretName(roleSpec.Name)
           secret := &corev1.Secret{}
           if err := rm.client.Get(ctx, client.ObjectKey{
               Name:      secretName,  // Reads correct K8s secret
               Namespace: namespace,
           }, secret); err != nil {
               return err
           }
           
           username := string(secret.Data["username"])  // Still "mcp_server"
           // CREATE ROLE mcp_server WITH LOGIN PASSWORD '...';
       }
   }
   ```

3. **Controller Phase 1** (`operators/hub-operator/internal/controller/hubenvironment_controller.go`):
   ```go
   for _, role := range hubEnv.Spec.Database.Roles {
       roleSecret, err := secrets.GenerateRoleDBCredentials(role.Name, namespace, owner)
       // roleSecret.Name is now "control-plane-db-credentials"
       
       if _, exists := existingSecrets[roleSecret.Name]; exists {
           continue
       }
       
       if err := r.Create(ctx, roleSecret); err != nil {
           return err
       }
   }
   ```

**The Complete Flow:**
```
CR: mcp_server
  ↓
Secret Generation: control-plane-db-credentials (K8s valid)
  ↓
Secret Data: username=mcp_server, password=xxx
  ↓
PostgreSQL: CREATE ROLE mcp_server (SQL valid)
  ↓
ExternalSecret: Syncs control-plane-db-credentials from Infisical
```

**Commit:** [current]

---

## Issue 10: platform-db-app Not Uploaded to Infisical

**Date:** 2026-04-04  
**Status:** Fixed

### Problem
ExternalSecret `platform-db-app-credentials-externalsecret.yaml` would fail to sync because Infisical keys `platform-db-app-username` and `platform-db-app-password` don't exist.

### Root Cause
The operator's `uploadSecretsToInfisical()` function only looped through `hubEnv.Spec.Database.Roles`. Since `platform-db-app` is a bootstrap secret (not a role in the CR), it was never uploaded to Infisical.

The old CLI explicitly uploaded it:
```go
// Upload platform-db-app (Layer 1 bootstrap secret)
secrets := map[string]string{
    "platform-db-app-username": "app",
    "platform-db-app-password": password,
}
for key, value := range secrets {
    infisicalClient.CreateOrUpdateSecret(ctx, key, value)
}
```

### Fix

Modified `uploadSecretsToInfisical()` to explicitly upload `platform-db-app` before processing role secrets:

**File:** `operators/hub-operator/internal/controller/hubenvironment_controller.go`

```go
func (r *HubEnvironmentReconciler) uploadSecretsToInfisical(ctx context.Context, hubEnv *opsv1alpha1.HubEnvironment) error {
    // ... setup ...
    
    // GAP 3 FIX: Upload platform-db-app (Layer 1 bootstrap secret)
    if !uploadedSecrets["platform-db-app"] {
        appSecret := &corev1.Secret{}
        if err := r.Get(ctx, client.ObjectKey{
            Name:      "platform-db-app",
            Namespace: namespace,
        }, appSecret); err == nil {
            username := string(appSecret.Data["username"])
            password := string(appSecret.Data["password"])

            if err := infisicalClient.CreateOrUpdateSecret(ctx, hubEnv, "platform-db-app-username", username); err != nil {
                return fmt.Errorf("failed to upload platform-db-app-username: %w", err)
            }

            if err := infisicalClient.CreateOrUpdateSecret(ctx, hubEnv, "platform-db-app-password", password); err != nil {
                return fmt.Errorf("failed to upload platform-db-app-password: %w", err)
            }

            hubEnv.Status.UploadedSecrets = append(hubEnv.Status.UploadedSecrets, "platform-db-app")
            logger.Info("Uploaded platform-db-app to Infisical")
        }
    }
    
    // Continue with role secrets...
}
```

Also fixed the Infisical key naming for role secrets to match ExternalSecret expectations:

```go
// Determine Infisical key prefix based on secret name
// control-plane-db-credentials → control-plane-db-username/password
// hub-db-credentials → hub-db-username/password
infisicalPrefix := strings.TrimSuffix(secretName, "-credentials")

if err := infisicalClient.CreateOrUpdateSecret(ctx, hubEnv, infisicalPrefix+"-username", username); err != nil {
    return err
}

if err := infisicalClient.CreateOrUpdateSecret(ctx, hubEnv, infisicalPrefix+"-password", password); err != nil {
    return err
}
```

**Commit:** [current]

---

## Migration Summary: CLI to Operator Gaps

### Context
During migration from CLI-based bootstrap (`internal/hub/components/installer-bkp.md`) to operator-based management, three critical gaps were discovered by comparing the old CLI logic against the new operator codebase.

### Gap Analysis

| Gap | CLI Behavior | Operator Behavior (Before Fix) | Impact |
|-----|--------------|-------------------------------|--------|
| **Secret Naming** | Mapped `mcp_server` → `control-plane-db-credentials` | Generated `mcp_server-db-credentials` (invalid) | Phase 2b failures, ExternalSecrets can't sync |
| **Redis Credentials** | Created both `infisical-secrets` and `infisical-redis-credentials` | Only created `infisical-secrets` | Redis pod stuck, Infisical blocked |
| **platform-db-app Upload** | Explicitly uploaded to Infisical | Skipped (not in roles list) | ExternalSecret sync would fail |

### Lessons Learned

1. **Kubernetes Naming Rules**: Secret names must follow RFC 1123 DNS subdomain rules (no underscores)
2. **PostgreSQL Naming Rules**: Role names can contain underscores (valid SQL identifiers)
3. **Mapping Layer Required**: Need explicit mapping between CR role names and K8s secret names
4. **Bootstrap Secrets**: Special-case secrets outside the roles list must be handled explicitly
5. **CLI Parity**: When migrating from CLI to operator, audit ALL secret generation and upload logic

### Verification Commands

```bash
# Check secret names are valid
kubectl get secrets -n platform-data | grep -E "(control-plane|hub-db|spire-server)"

# Verify Redis is running
kubectl get pods -n platform-data -l app.kubernetes.io/name=redis

# Check Infisical keys exist
kubectl exec -n platform-data infisical-0 -- \
  infisical secrets list --env prod --path /

# Verify ExternalSecrets are synced
kubectl get externalsecrets -n platform-data
```

---

# Hub Operator Troubleshooting Guide

## Fix Status Summary (Latest)

### ✅ Database Connection Fix (CNPG Wildcard)
**Status:** ALREADY APPLIED

Both files already handle the CNPG wildcard `*` dbname:
- `operators/hub-operator/internal/database/migrator.go` (lines 56-60)
- `operators/hub-operator/internal/database/roles.go` (lines 40-44)

Code correctly defaults to `postgres` database when encountering `*` or empty string.

### ✅ ArgoCD Sync-Wave Annotations
**Status:** ALREADY APPLIED

Both applications already have sync-wave "4":
- `manifests/hub-core-services/platform-identity/argocd/mcp-server.yaml` (line 6)
- `manifests/hub-core-services/platform-identity/argocd/kratos-ui.yaml` (line 6)

### ✅ OutOfSync Fix (ServerSideApply)
**Status:** ALREADY APPLIED

`manifests/argocd/apps/platform-external-secrets.yaml` already has:
- `ServerSideApply=true` (line 68)
- `RespectIgnoreDifferences=true` (line 69)

## Current Issue: Role Name Mismatch

**Problem:** HubEnvironment CR uses underscores, secrets use hyphens.

**CR:** `mcp_server`, `spoke_controller`, `spire_server`
**Secrets:** `mcp-server-db-credentials`, `spoke-controller-db-credentials`

**Fix:** Secret generation must match CR naming (underscores).

# Hub Operator Troubleshooting Guide

## Fix Status Summary

### ✅ Database Connection Fix (CNPG Wildcard)
**Status:** ALREADY APPLIED

Both files handle CNPG wildcard `*` dbname correctly.

### ✅ ArgoCD Sync-Wave Annotations
**Status:** ALREADY APPLIED

Both mcp-server and kratos-ui have sync-wave "4".

### ✅ OutOfSync Fix (ServerSideApply)
**Status:** ALREADY APPLIED

platform-external-secrets has ServerSideApply=true.

### ✅ Role Name Mismatch Fix
**Status:** FIXED

**Problem:** Operator wasn't generating credential secrets for CR-defined roles.

**Fix:** Added dynamic role credential generation in Phase 1.

**Next Steps:**
```bash
# 1. Rebuild operator
make docker-build docker-push IMG=ghcr.io/kamarkat/kamarkat/hub-operator:latest

# 2. Restart operator
kubectl rollout restart deployment hub-operator -n platform-ops

# 3. Trigger reconciliation
kubectl annotate hubenvironment hub-production -n platform-ops \
  ops.nutgraf.in/reconcile-trigger="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

# Hub Operator Troubleshooting Guide

## Issue 8: Missing `infisical-redis-credentials` Secret

**Symptom:** Redis StatefulSet stuck in `CreateContainerConfigError` with event:
```
Error: secret "infisical-redis-credentials" not found
```

**Root Cause:** Operator's `GenerateSecretZero()` only created `infisical-secrets` but not `infisical-redis-credentials` that Redis StatefulSet expects.

**Fix:** Added `GenerateInfisicalRedisCredentials()` function to create the missing secret.

**Files Changed:**
- `operators/hub-operator/internal/secrets/generator.go`
- `operators/hub-operator/internal/controller/hubenvironment_controller.go`

**Commit:** a94a00dea72c03860c175125e8997a31450ecb84

---

## Issue 9: Secret Naming Convention Mismatch (Phase 2b Blocker)

**Symptom:** Phase 2b fails with:
```
failed to get secret control-plane-db-mcp_server: secrets "control-plane-db-mcp_server" not found
```

**Root Cause:** HubEnvironment CR uses underscores in role names (`mcp_server`, `spoke_controller`) but K8s secrets need hyphens. Operator was directly using role names without mapping.

**Fix:** Added `mapRoleToSecretName()` function to map:
- `mcp_server` → `control-plane-db-credentials`
- `spoke_controller` → `hub-db-credentials`
- Other roles → `{role}-db-credentials` (with underscores replaced by hyphens)

**Files Changed:**
- `operators/hub-operator/internal/database/roles.go`

**Commit:** a94a00dea72c03860c175125e8997a31450ecb84

---

## Issue 10: `platform-db-app` Not Uploaded to Infisical

**Symptom:** ExternalSecret `platform-db-app-credentials` stuck in `SecretSyncedError`:
```
could not find secret platform-db-app-username
```

**Root Cause:** Operator only uploaded role-specific secrets (mcp_server, spoke_controller) but not the base `platform-db-app` credentials that CNPG bootstrap created.

**Fix:** Modified `uploadSecretsToInfisical()` to explicitly upload `platform-db-app-username` and `platform-db-app-password` from the `platform-db-app` K8s secret.

**Files Changed:**
- `operators/hub-operator/internal/secrets/uploader.go`

**Commit:** a94a00dea72c03860c175125e8997a31450ecb84

---

## Issue 11: SQL Syntax Error in CREATE ROLE

**Symptom:** Phase 2b fails with:
```
pq: syntax error at or near "$1"
```

**Root Cause:** Operator was using parameterized query `PASSWORD $1` but PostgreSQL CREATE ROLE doesn't support parameterized passwords (only bind parameters in WHERE clauses work).

**Fix:** Changed to string escaping: `PASSWORD '%s'` with proper escaping of single quotes using `strings.ReplaceAll(password, "'", "''")`.

**Files Changed:**
- `operators/hub-operator/internal/database/roles.go`

**Commit:** a94a00dea72c03860c175125e8997a31450ecb84

---

## Issue 12: Schema Does Not Exist Error

**Symptom:** Phase 2b fails with:
```
pq: schema "control_plane" does not exist
```

**Root Cause:** Operator was using `roleSpec.Database` as schema name in GRANT statements (`GRANT SELECT ON ALL TABLES IN SCHEMA control_plane`), but migrations create tables in the `public` schema, not in schemas named after databases.

**Analysis:**
- Migrations create tables in `public` schema within each database
- `control_plane` is a database name, not a schema name
- Operator needs to connect to target database and grant on `public` schema

**Fix:** Updated `grantPermissions()` to:
1. Connect to target database (not just postgres)
2. Grant CONNECT on database
3. Grant USAGE on public schema
4. Grant permissions on existing tables in public schema
5. Grant default privileges for future tables

**Files Changed:**
- `operators/hub-operator/internal/database/roles.go`

**Commit:** [pending]

---

## Migration Summary: CLI vs Operator Behavior

### CLI Approach (installer-bkp.md)
1. Generated passwords using `crypto/rand`
2. Injected secrets directly via client-go (Secret Zero)
3. Uploaded secrets to Infisical via API
4. Did NOT create database roles (relied on CNPG bootstrap)

### Operator Approach (Current)
1. Reads passwords from existing K8s secrets
2. Creates database roles with CREATE ROLE
3. Grants permissions on tables
4. Uploads secrets to Infisical
5. Manages role lifecycle (create/update/delete)

### Key Differences
- **CLI:** Bootstrap-only, one-time execution
- **Operator:** Continuous reconciliation, Day-2 operations
- **Role Management:** Operator adds full role lifecycle management
- **Schema Handling:** Fixed to use `public` schema instead of database names

---

## Validation Commands

### Check Role Creation
```bash
kubectl exec -n platform-data platform-db-1 -- psql -U postgres -d control_plane -c "\du mcp_server"
```

### Check Permission Grants
```bash
kubectl exec -n platform-data platform-db-1 -- psql -U postgres -d control_plane -c "\dp"
```

### Check Infisical Secrets
```bash
kubectl exec -n zero-ops-system deployment/platform-infisical-infisical-standalone-infisical -- \
  curl -s http://localhost:8080/api/v1/secrets -H "Authorization: Bearer <token>"
```

### Check ExternalSecret Status
```bash
kubectl get externalsecrets -n platform-data -o wide
```

---

## Next Steps

1. Build and deploy operator with schema fix
2. Verify role creation succeeds
3. Verify permission grants succeed
4. Verify `infisical-secrets` gets created (Phase 2b completion)
5. Verify Infisical pods become healthy
6. Test https://infisical.nutgraf.in/ accessibility
