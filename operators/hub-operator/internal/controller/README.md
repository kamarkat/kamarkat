# HubEnvironment Controller

## Overview

The HubEnvironmentReconciler is the core controller that orchestrates Day-2 operations for the Zero-Ops Hub cluster. It watches the HubEnvironment Custom Resource and executes a multi-phase reconciliation workflow to provision and configure all Hub infrastructure components.

## Architecture

### Reconciliation Phases

The controller executes phases sequentially, updating status conditions after each phase:

1. **Phase 1: Bootstrap secret generation**
   - Generates the Secret Zero credentials infrastructure needs before Infisical exists
   - Idempotent: an existing password is reused, never regenerated
   - Addresses the bootstrap cluster the CR names (`Spec.Database.ClusterRef`)
   - Sets `BootstrapSecretsGenerated`

2. **Phase 2: Application secret readiness**
   - Waits for the credential of every declared role, derived from the role list
     through `database.RoleSecretName` — the same function that applies it
   - Sets `ApplicationSecretsReady`

3. **Phase 3: Database role provisioning**
   - One `RoleManager` per cluster the CR declares (`Spec.DatabaseClusters()`),
     each acting only on `Spec.RolesFor(cluster)` — a role is provisioned on its
     own application's cluster and no other (ADR-112)
   - Grants permissions from the CR spec, detects password drift, issues
     `ALTER ROLE`, prunes roles the CR no longer declares
   - Sets `DatabaseRolesProvisioned`

4. **Phase 4: Infisical secret upload**
   - Waits for Infisical to be Ready
   - Uploads credentials to the Infisical API, tracking them in
     `Status.UploadedSecrets` and skipping what is already there
   - Suspends if `infisical-auth` is missing
   - Sets `SecretsBackedUp`

5. **Phase 5: Identity**
   - Registers the platform's OAuth clients with the box's own issuer
   - Idempotent on client id; prunes clients the CR no longer declares
   - Sets `IdentityReady`

6. **Ready**
   - Set when every phase above has completed
   - Schema is NOT a phase here: each application migrates its own, at its own
     cluster (ADR-074, amended 2026-11-03)

### Dependency Readiness Checks

The controller implements readiness checks for external dependencies:

- `isCNPGReady()`: the bootstrap cluster's Ready condition — the database the
  operator itself reads. An application's own cluster is that application's
  prerequisite, and `RoleManager` fails per cluster rather than holding up the rest
- `isInfisicalReady()`: Infisical's workload status

Reconciliation waits (requeues after 10 seconds) if dependencies are not ready.

## Error Handling

### Error Classification

The controller distinguishes between transient and permanent errors:

**Transient Errors** (requeue with backoff):
- Database connection failures
- API timeouts (the issuer, Infisical)
- Network failures
- 5xx HTTP errors

**Permanent Errors** (no automatic requeue):
- Dirty database state (requires manual intervention)
- Invalid CR configuration

### Retry Strategy

- Transient errors: Requeue after 30 seconds (exponential backoff via controller-runtime)
- Dependency not ready: Requeue after 10 seconds
- Permanent errors: Set condition to False, do NOT requeue

### Manual Recovery

For permanent errors (e.g., dirty database), add the `ops.nutgraf.in/reconcile-trigger` annotation:

```bash
kubectl annotate hubenvironment hub-env \
  ops.nutgraf.in/reconcile-trigger="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

This clears permanent error conditions and resumes reconciliation.

## Status Conditions

The controller maintains 7 status conditions:

| Condition | Type | Reason | Description |
|-----------|------|--------|-------------|
| BootstrapSecretsGenerated | True/False | Generated/Failed | Secret Zero created |
| ApplicationSecretsReady | True/False | Ready/Waiting | Every declared role's credential has been delivered |
| DatabaseRolesProvisioned | True/False | Provisioned/RoleCreationFailed | Roles provisioned, each on its own cluster |
| SecretsBackedUp | True/False | Uploaded/AuthenticationFailed | Secrets uploaded to Infisical |
| IdentityReady | True/False | Registered/Failed | OAuth clients registered with the box's issuer |
| Ready | True/False | AllPhasesComplete/Failed | Hub environment fully operational |

## RBAC Permissions

The controller requires the following permissions:

```yaml
# HubEnvironment CRD
- apiGroups: ["ops.nutgraf.in"]
  resources: ["hubenvironments", "hubenvironments/status", "hubenvironments/finalizers"]
  verbs: ["get", "list", "watch", "update", "patch"]

# Secrets (owned)
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]

# CNPG Cluster (dependency check)
- apiGroups: ["postgresql.cnpg.io"]
  resources: ["clusters"]
  verbs: ["get", "list", "watch"]
```

## Idempotency

All reconciliation operations are idempotent:

- Secret generation reuses existing passwords
- Role creation is idempotent, and a managed role is identified by its PostgreSQL
  comment rather than by assuming the operator made it
- OAuth client registration checks client_id before creating
- Infisical uploads track uploaded secrets in Status.UploadedSecrets

Running reconciliation multiple times produces the same end state.

## Structured Logging

The controller uses controller-runtime structured logging:

```go
logger := log.FromContext(ctx)
logger.Info("Phase 1: Generating Secret Zero")
logger.Error(err, "Failed to create secret", "secret", secretName)
```

All operations log phase transitions, errors, and important events.

## Watch Configuration

The controller watches:

- **Primary Resource**: HubEnvironment CR
- **Owned Resources**: Secrets created by the operator (via ownerReferences)
- **External Dependencies**: CNPG Cluster (for readiness)

Future tasks will add watches for:
- `infisical-db-ca` (certificate rotation)
- Secrets with label `ops.nutgraf.in/db-credentials=true` (password rotation)

## Testing

### Unit Tests (envtest)
- Use controller-runtime fake client
- Test reconciliation logic in isolation
- Mock external API calls

### E2E Tests (KUTTL)
- Deploy real CNPG and Infisical
- Assert database roles exist via SQL queries
- Assert OAuth clients exist via the issuer's API
- Assert secrets exist in Infisical via API

## Development Workflow

### Local Testing

1. Start a local cluster (kind or k3d)
2. Install CRDs: `make install`
3. Run controller: `make run`
4. Apply HubEnvironment CR: `kubectl apply -f config/samples/`

### GitOps Deployment

1. Commit changes to feature branch
2. Push to GitHub
3. ArgoCD syncs operator to dev cluster
4. Verify reconciliation via status conditions

**NEVER use `kubectl apply` for production deployments. All changes must go through GitOps.**

## Troubleshooting

### Reconciliation Stuck

Check status conditions:
```bash
kubectl get hubenvironment hub-env -o jsonpath='{.status.conditions}' | jq
```

### Infisical Upload Suspended

Check if infisical-auth secret exists:
```bash
kubectl get secret infisical-auth -n platform-ops
```

If missing, Phase 3a will suspend until the secret is created.

### Dependency Not Ready

Check dependency status:
```bash
kubectl -n platform-data get clusters.postgresql.cnpg.io \
  -o custom-columns=NAME:.metadata.name,READY:'.status.conditions[?(@.type=="Ready")].status'
kubectl -n platform-security get deployment infisical-standalone-infisical
```

Reconciliation will requeue every 10 seconds until dependencies are ready.

## References

- [Kubebuilder Documentation](https://book.kubebuilder.io/)
- [Controller-Runtime](https://github.com/kubernetes-sigs/controller-runtime)
- [CloudNativePG API](https://cloudnative-pg.io/documentation/current/api_reference/)
