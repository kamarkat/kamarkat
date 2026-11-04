# Known Issues and Resolutions

## Issue 1: ESO PushSecret Not Implemented for Infisical Provider

### Problem
ESO's Infisical provider returns `SecretStoreReadOnly` capability and explicitly returns `errNotImplemented` for PushSecret operations.

**Evidence from ESO codebase** (`archived/identity-auth/external-secrets/providers/v1/infisical/`):

```go
// provider.go
func (p *Provider) Capabilities() esv1.SecretStoreCapabilities {
	return esv1.SecretStoreReadOnly
}

// client.go
func (p *Provider) PushSecret(_ context.Context, _ *corev1.Secret, _ esv1.PushSecretData) error {
	return errNotImplemented
}
```

### Impact
- Cannot use ESO PushSecret to upload per-spoke crossplane-admin passwords to Infisical
- Spoke clusters cannot pull crossplane-admin credentials via ESO ExternalSecret
- Blocks Phase 1 validation (tasks 4.1.1-4.1.3)

### Root Cause
ESO's Infisical provider is designed as **read-only**. This is an upstream limitation, not a platform bug.

### Solution Options

#### Option 1: Deploy Infisical Native Operator (NOT RECOMMENDED)
- Deploy `infisical/kubernetes-operator` alongside ESO
- Use `InfisicalPushSecret` CRD instead of ESO's `PushSecret`
- **Cons**: Adds operational complexity, two operators for same backend

#### Option 2: Extend hub-operator (RECOMMENDED)
- Add per-spoke secret generation to `hub-operator`
- Upload to Infisical via API during SpokePool reconciliation
- **Pros**: Centralized, follows existing bootstrap pattern, no new operators
- **Implementation**: Extend `operators/hub-operator/internal/infisical/secret_mappings.go`

#### Option 3: Use Infisical API Directly
- Create Job/CronJob to upload secrets via curl
- **Cons**: Not declarative, requires manual intervention

### Recommended Fix: Option 2 - Detailed Implementation Plan

Extend `hub-operator` to handle per-spoke secret generation and upload following existing patterns.

#### Architecture Analysis

**Existing Patterns in hub-operator**:
1. **HubEnvironment Controller** (`internal/controller/hubenvironment_controller.go`):
   - Watches HubEnvironment CR
   - Generates bootstrap secrets in Phase 1
   - Uploads secrets to Infisical in Phase 0 via `SecretUploader`
   - Uses `UncachedClient` for reading secret data

2. **Secret Generation** (`internal/secrets/generator.go`):
   - `GenerateSecurePassword()` - 32-char hex passwords
   - `GenerateBootstrapSecrets()` - orchestrates bootstrap secret creation
   - Implements idempotency via `existingSecrets` map
   - Supports AWS backup/restore for critical secrets

3. **Infisical Upload** (`internal/infisical/secret_uploader.go`):
   - `SecretUploader.UploadCLISecrets()` - uploads from K8s to Infisical
   - Uses `CLISecretMappings` registry pattern
   - Reads secrets via `UncachedClient` (bypasses cache)
   - Calls `InfisicalClient.CreateOrUpdateSecretRaw()`

4. **Infisical Client** (`internal/client/infisical.go`):
   - `CreateOrUpdateSecretRaw()` - creates/updates secrets in Infisical
   - Handles authentication, token refresh, retries
   - Uses Infisical v3 API

#### Implementation Plan

**Step 1: Add SpokePool Controller**

Create `operators/hub-operator/internal/controller/spokepool_controller.go`:

```go
package controller

import (
	"context"
	"fmt"
	
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	
	infisicalclient "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/infisical"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

// SpokePoolReconciler reconciles SpokePool XRs
type SpokePoolReconciler struct {
	client.Client
	UncachedClient client.Client
}

//+kubebuilder:rbac:groups=nutgraf.in,resources=spokepools,verbs=get;list;watch
//+kubebuilder:rbac:groups=nutgraf.in,resources=spokepools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *SpokePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	
	// 1. Fetch SpokePool XR
	spokePool := &unstructured.Unstructured{}
	spokePool.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "SpokePool",
	})
	
	if err := r.Get(ctx, req.NamespacedName, spokePool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	
	spokeName := spokePool.GetName()
	
	// 2. Check if crossplane-admin secret already generated
	conditions, _, _ := unstructured.NestedSlice(spokePool.Object, "status", "conditions")
	if meta.IsStatusConditionTrue(conditions, "CrossplaneAdminSecretGenerated") {
		return ctrl.Result{}, nil
	}
	
	logger.Info("Generating crossplane-admin secret for spoke", "spoke", spokeName)
	
	// 3. Generate password using metadata.uid (36-char UUID)
	password := string(spokePool.GetUID())
	
	// 4. Create K8s secret in platform-ops
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-crossplane-admin", spokeName),
			Namespace: "platform-ops",
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"password": password,
		},
	}
	
	if err := r.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}
	
	// 5. Upload to Infisical
	infisicalClient, err := infisicalclient.NewInfisicalClient(ctx, r.UncachedClient, "")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create Infisical client: %w", err)
	}
	
	projectSlug := "hub-platform"
	environmentSlug := "dev"
	secretPath := "/"
	infisicalKey := fmt.Sprintf("%s-crossplane-admin-password", spokeName)
	
	if err := infisicalClient.CreateOrUpdateSecretRaw(ctx, projectSlug, environmentSlug, secretPath, infisicalKey, password); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to upload to Infisical: %w", err)
	}
	
	logger.Info("Uploaded crossplane-admin password to Infisical", "spoke", spokeName, "key", infisicalKey)
	
	// 6. Update status condition
	meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:    "CrossplaneAdminSecretGenerated",
		Status:  metav1.ConditionTrue,
		Reason:  "Generated",
		Message: fmt.Sprintf("crossplane-admin secret generated and uploaded to Infisical"),
	})
	
	unstructured.SetNestedSlice(spokePool.Object, conditions, "status", "conditions")
	
	if err := r.Status().Update(ctx, spokePool); err != nil {
		return ctrl.Result{}, err
	}
	
	return ctrl.Result{}, nil
}

func (r *SpokePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&unstructured.Unstructured{}).
		Complete(r)
}
```

**Step 2: Register Controller in main.go**

Add to `operators/hub-operator/cmd/main.go`:

```go
if err = (&controller.SpokePoolReconciler{
	Client:         mgr.GetClient(),
	UncachedClient: uncachedClient,
}).SetupWithManager(mgr); err != nil {
	setupLog.Error(err, "unable to create controller", "controller", "SpokePool")
	os.Exit(1)
}
```

**Step 3: Add RBAC Permissions**

Add to `operators/hub-operator/config/rbac/role.yaml`:

```yaml
- apiGroups:
  - nutgraf.in
  resources:
  - spokepools
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - nutgraf.in
  resources:
  - spokepools/status
  verbs:
  - get
  - update
  - patch
```

**Step 4: Remove PushSecret from SpokePool Composition**

Remove the `crossplane-admin-pushsecret` resource from `xrds/compositions/spokepool-hetzner.yaml` (lines 703-768).

**Step 5: Update Spoke ExternalSecret**

Ensure `manifests/spoke-catalog/infra/crossplane-admin-eso.yaml` uses correct Infisical key pattern:

```yaml
data:
  - secretKey: password
    remoteRef:
      key: ${SPOKE_NAME}-crossplane-admin-password  # Injected by ApplicationSet
```

#### Benefits of This Approach

1. **Follows Existing Patterns**: Mirrors HubEnvironment controller structure
2. **Reuses Infrastructure**: Uses existing InfisicalClient, no new dependencies
3. **Declarative**: Controller watches XRs, no manual intervention
4. **Idempotent**: Checks status condition before generating
5. **Centralized**: All secret management in hub-operator
6. **No New Operators**: Avoids deploying Infisical native operator

#### Testing Plan

1. **Unit Tests**: Test password generation, Infisical upload logic
2. **E2E Tests**: Create SpokePool XR, verify secret in K8s and Infisical
3. **Validation**: Verify Spoke can pull secret via ESO ExternalSecret

### Temporary Workaround
Manual upload via Job until hub-operator is extended:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: upload-crossplane-admin-password
  namespace: platform-ops
spec:
  template:
    spec:
      containers:
      - name: upload
        image: curlimages/curl:8.5.0
        env:
        - name: INFISICAL_TOKEN
          valueFrom:
            secretKeyRef:
              name: infisical-auth
              key: serviceToken
        - name: PASSWORD
          valueFrom:
            secretKeyRef:
              name: spoke-pool-eu-prod-01-crossplane-admin
              key: password
        command:
        - /bin/sh
        - -c
        - |
          curl -X POST "https://app.infisical.com/api/v3/secrets/spoke-pool-eu-prod-01-crossplane-admin-password" \
            -H "Authorization: Bearer $INFISICAL_TOKEN" \
            -H "Content-Type: application/json" \
            -d "{
              \"workspaceId\": \"hub-platform\",
              \"environment\": \"dev\",
              \"secretValue\": \"$PASSWORD\",
              \"type\": \"shared\"
            }"
```

### Status
- **Current**: Using temporary Job workaround
- **Next**: Implement Option 2 (hub-operator extension)
- **Blocked**: Phase 1 validation until proper fix is implemented

### References
- ESO Infisical Provider: `archived/identity-auth/external-secrets/providers/v1/infisical/`
- Infisical Native Operator: `archived/identity-auth/kubernetes-operator/`
- Hub Operator: `operators/hub-operator/`

---

## Issue 2: Bootstrap Job Secret Reference Error

### Problem
Bootstrap Job failed with `CreateContainerConfigError: secret "shared-cnpg-superuser" not found`

### Root Cause
CNPG doesn't create superuser secret by default for existing clusters. Only `shared-cnpg-app` secret exists.

### Solution
- Updated bootstrap Job to use `shared-cnpg-app` secret (app user with CREATEROLE privilege)
- Changed connection from `postgres` superuser to `app` user
- Removed unused `superuserSecret` configuration from CNPG Cluster

### Status
- **Fixed**: 2026-04-24
- **Files Modified**: 
  - `manifests/spoke-catalog/infra/crossplane-admin-bootstrap.yaml`
  - `manifests/spoke-catalog/infra/cnpg-cluster.yaml`

---

## Issue 2: Bootstrap Job Fails - CNPG Superuser Secret Not Created

### Problem
Bootstrap Job (`crossplane-admin-bootstrap`) fails with `CreateContainerConfigError: secret "shared-cnpg-superuser" not found`.

**Evidence from spoke cluster events**:
```
Warning   Failed             pod/crossplane-admin-bootstrap-sj7gt   
Error: secret "shared-cnpg-superuser" not found
```

### Impact
- Bootstrap Job cannot create `crossplane_admin` role
- provider-sql ProviderConfig cannot connect to database
- Blocks Phase 1 completion (Application stuck at OutOfSync/Missing)

### Root Cause
1. CNPG Cluster was created without `superuserSecret` configuration initially
2. Adding `superuserSecret` to existing cluster doesn't retroactively create the secret
3. CNPG only creates `shared-cnpg-app` secret (for app user, not postgres superuser)
4. Bootstrap Job was referencing non-existent `shared-cnpg-superuser` secret

**Available secrets**:
```bash
$ kubectl get secrets -n platform-data | grep cnpg
shared-cnpg-app           kubernetes.io/basic-auth   11     29h
shared-cnpg-ca            Opaque                     2      29h
shared-cnpg-replication   kubernetes.io/tls          2      29h
shared-cnpg-server        kubernetes.io/tls          2      29h
```

### Solution
Use `app` user instead of `postgres` superuser for bootstrap operations.

**Rationale**:
- CNPG creates `shared-cnpg-app` secret by default
- `app` user can be granted `CREATEROLE` privilege via `postInitSQL`
- `CREATEROLE` privilege allows creating other roles (including `crossplane_admin`)
- No need for superuser access for this operation

### Implementation

**Step 1: Update CNPG Cluster postInitSQL**

File: `manifests/spoke-catalog/infra/cnpg-cluster.yaml`

```yaml
bootstrap:
  initdb:
    database: app
    owner: app
    postInitSQL:
      - CREATE EXTENSION IF NOT EXISTS vector;
      - CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
      - ALTER ROLE app WITH CREATEROLE;  # Grant role creation privilege
```

**Step 2: Update Bootstrap Job**

File: `manifests/spoke-catalog/infra/crossplane-admin-bootstrap.yaml`

Changes:
- User: `postgres` → `app`
- Database: `postgres` → `app`
- Secret: `shared-cnpg-superuser` → `shared-cnpg-app`
- Env var: `POSTGRES_PASSWORD` → `APP_PASSWORD`

```yaml
command:
  - /bin/sh
  - -c
  - |
    set -e
    echo "Waiting for CNPG cluster to be ready..."
    until pg_isready -h shared-cnpg-rw.platform-data.svc.cluster.local -p 5432 -U app; do
      echo "Waiting for PostgreSQL..."
      sleep 5
    done
    
    echo "Creating crossplane_admin role..."
    PGPASSWORD="${APP_PASSWORD}" psql \
      -h shared-cnpg-rw.platform-data.svc.cluster.local \
      -p 5432 \
      -U app \
      -d app \
      -c "DO \$\$ BEGIN
            IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'crossplane_admin') THEN
              CREATE ROLE crossplane_admin WITH LOGIN CREATEDB CREATEROLE PASSWORD '${CROSSPLANE_ADMIN_PASSWORD}';
              RAISE NOTICE 'Role crossplane_admin created successfully';
            ELSE
              RAISE NOTICE 'Role crossplane_admin already exists, skipping creation';
            END IF;
          END \$\$;"
    
    echo "Bootstrap complete"
env:
  - name: APP_PASSWORD
    valueFrom:
      secretKeyRef:
        name: shared-cnpg-app
        key: password
  - name: CROSSPLANE_ADMIN_PASSWORD
    valueFrom:
      secretKeyRef:
        name: crossplane-admin-credentials
        key: password
```

**Step 3: Remove superuserSecret from CNPG Cluster**

Remove the following lines from `manifests/spoke-catalog/infra/cnpg-cluster.yaml`:

```yaml
superuserSecret:
  name: shared-cnpg-superuser
```

### Recovery Steps

1. Commit changes to Git
2. Delete existing failed Job: `kubectl delete job crossplane-admin-bootstrap -n platform-data --kubeconfig k8-secrets/kubeconfig/spoke-pool-eu-prod-01.kubeconfig`
3. Trigger ArgoCD sync to recreate Job with correct configuration
4. Verify Job completes successfully
5. Verify Application reaches `Synced/Healthy` status

### Status
- **Current**: Fixed in manifests, ready to commit
- **Next**: GitOps deployment and validation
- **Completion Criteria**: `status.sync.status=Synced` AND `status.health.status=Healthy`

### References
- CNPG Cluster: `manifests/spoke-catalog/infra/cnpg-cluster.yaml`
- Bootstrap Job: `manifests/spoke-catalog/infra/crossplane-admin-bootstrap.yaml`
- CNPG Documentation: https://cloudnative-pg.io/documentation/
