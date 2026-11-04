# Implementation Plan

## Phase 0: Fix TenantDatabase Composition (ADR 006 Prerequisite)

**Context**: Before implementing ADR 005 fixes, we must resolve the PostgreSQL Role creation failure in the TenantDatabase composition. This is blocking tenant database provisioning.

- [x] 0.1 Delete Resource 3 (Crossplane-generated secret) from TenantDatabase composition
  - Open `manifests/spoke/compositions/tenantdatabase-spoke.yaml`
  - Locate Resource 3 with `name: db-credentials-secret`
  - Delete the entire resource block (Object MR that creates Secret with `metadata.uid` password)
  - **Why**: Violates ADR 006 by generating credentials via Crossplane patches instead of Infisical
  - Commit changes to feature branch
  - _ADR: 006 (Multi-Tenant Database Pattern)_

- [x] 0.2 Update Resource 1 to use creationPolicy: Owner
  - In `manifests/spoke/compositions/tenantdatabase-spoke.yaml`
  - Locate Resource 1 with `name: db-credentials-restore`
  - Change `creationPolicy: Merge` to `creationPolicy: Owner`
  - Add template section with computed fields (database, host, port)
  - Change `dataFrom.extract` to explicit `data` array (username, password properties)
  - Add patch to inject database name dynamically
  - **Why**: ESO should create and own the secret (Infisical → ESO → provider-sql pattern)
  - Commit changes to feature branch
  - _ADR: 006 (Multi-Tenant Database Pattern)_

- [x] 0.3 Manual Validation - TenantDatabase Fix
  - **What to verify**: ESO creates secret, provider-sql Role created, AtlasMigration succeeds
  - **How to verify**:
    - Push changes, wait for ArgoCD sync
    - `kubectl get externalsecret tenant-app-creator-db-credentials-restore -n tenant-app-creator` - verify Ready=True
    - `kubectl get secret tenant-app-creator-db-credentials -n tenant-app-creator` - verify exists with username/password from Infisical
    - `kubectl get role.postgresql.sql.crossplane.io tenant_app-creator_user` - verify Ready=True
    - `kubectl exec -n platform-data shared-cnpg-1 -- psql -U postgres -c "\du tenant_app_creator_user"` - verify user exists
    - `kubectl get atlasmigration -n tenant-app-creator` - verify Ready=True, no "no such user" error
  - **Expected outcome**: PostgreSQL Role created successfully, AtlasMigration works, database provisioning unblocked
  - **Approval gate**: Proceed to Phase 1 only after TenantDatabase fix verified

---

## Phase 1: XRD Schema Updates

- [x] 1.1 Update XRD to add resourceQuota and ownerEmail fields
  - Open `xrds/definitions/ainativesaas-v1.yaml`
  - Add `spec.resourceQuota` field with cpu, memory, storage, pods properties
  - Add `spec.ownerEmail` field with email validation pattern
  - Commit changes to feature branch
  - Push to trigger ArgoCD sync
  - _Requirements: 2.7_

- [x] 1.2 Manual Validation - XRD Schema
  - **What to verify**: XRD accepts resourceQuota and ownerEmail fields
  - **How to verify**: 
    - `kubectl get xrd ainativesaas.platform.zero-ops.io -o yaml` - confirm new fields in schema
    - Create test XR with resourceQuota and ownerEmail fields
    - `kubectl apply -f test-xr.yaml` - should succeed without validation errors
  - **Expected outcome**: XR created successfully, no validation errors
  - **Approval gate**: Proceed to Phase 2 only after approval

---

## Phase 2: Helm Template Updates

- [x] 2.1 Update Helm template to map resourceQuota values
  - Open `manifests/tenants/charts/universal-tenant/templates/ainativesaas.yaml`
  - Add resourceQuota mapping block after postgrest spec
  - Add ownerEmail mapping block after resourceQuota
  - Commit changes to feature branch
  - _Requirements: 2.7_

- [x] 2.2 Delete obsolete Helm templates
  - Delete `manifests/tenants/charts/universal-tenant/templates/namespace.yaml`
  - Delete `manifests/tenants/charts/universal-tenant/templates/resourcequota.yaml`
  - Delete `manifests/tenants/charts/universal-tenant/templates/rbac.yaml`
  - Delete `manifests/tenants/charts/universal-tenant/templates/configmap-migrations.yaml`
  - Commit deletions to feature branch
  - _Requirements: 2.5, 2.6_

- [x] 2.3 Manual Validation - Helm Template Rendering
  - **What to verify**: Helm template renders XR with resourceQuota mapped correctly
  - **How to verify**:
    - `helm template test-tenant manifests/tenants/charts/universal-tenant --set tenantId=test-001 --set resourceQuota.cpu=2000m --set resourceQuota.memory=4Gi`
    - Inspect rendered YAML for `spec.resourceQuota` block
    - Verify deleted templates no longer render (namespace.yaml, resourcequota.yaml, rbac.yaml, configmap-migrations.yaml)
  - **Expected outcome**: XR contains resourceQuota values, deleted templates absent
  - **Approval gate**: Proceed to Phase 3 only after approval

---

## Phase 3: Composition Updates

- [x] 3.1 Add Namespace Object MR to Composition
  - Open `xrds/compositions/ainativesaas-starter-hetzner.yaml`
  - Add namespace Object MR as resource #7 (after atlasmigration)
  - Configure patches for tenantId → namespace name transformation
  - Configure providerConfigRef patch from cellId
  - Commit changes to feature branch
  - _Requirements: 2.2_

- [x] 3.2 Add ServiceAccount Object MR to Composition
  - Add service-account Object MR as resource #8
  - Configure patches for tenantId → ServiceAccount name/namespace
  - Configure providerConfigRef patch from cellId
  - Commit changes to feature branch
  - _Requirements: 2.2_

- [x] 3.3 Add Role Object MR to Composition
  - Add role Object MR as resource #9
  - Define RBAC rules (pods, services, configmaps, secrets, deployments, statefulsets)
  - Configure patches for tenantId → Role name/namespace
  - Configure providerConfigRef patch from cellId
  - Commit changes to feature branch
  - _Requirements: 2.2_

- [x] 3.4 Add RoleBinding Object MR to Composition
  - Add role-binding Object MR as resource #10
  - Configure roleRef and subjects patches from tenantId
  - Configure providerConfigRef patch from cellId
  - Commit changes to feature branch
  - _Requirements: 2.2_

- [x] 3.5 Add ResourceQuota Object MR to Composition
  - Add resource-quota Object MR as resource #11
  - Configure patches from spec.resourceQuota fields to manifest.spec.hard
  - Configure providerConfigRef patch from cellId
  - Commit changes to feature branch
  - _Requirements: 2.2, 2.7_

- [x] 3.6 Create GitHub PAT ExternalSecret for Atlas migrations
  - **CRITICAL**: zero-ops repository is PRIVATE. Atlas needs GitHub PAT to pull migrations from Git
  - **Why this is needed**: Currently works because ArgoCD globs SQL files into ConfigMap. After switching to Git-native, Atlas needs credentials
  - Add github-migrations-pat ExternalSecret as resource #12 in Composition
  - Configure ESO to pull PAT from Infisical secret store
  - Secret will be created in each tenant namespace for AtlasMigration to reference
  - Commit changes to feature branch
  - _Design: Feedback GAP 2 - Atlas Git Authentication_

- [x] 3.7 Update AtlasMigration to use Git URL with credentials AND add status mapping
  - Replace existing atlasmigration resource (resource #6)
  - Change spec.dir from configMapRef to url-based Git pull
  - Set url: "https://github.com/kamarkat/kamarkat.git"
  - Set ref: "main"
  - **Add credentials block**: Reference github-migrations-pat secret created in 3.6
  - Configure path patch from database.migrations.baseline
  - **CRITICAL - Add ToCompositeFieldPath status mapping**: Extract ACTUAL AtlasMigration CR status from Spoke back to XR
    - Object MR `Ready=True` only means "YAML applied to Spoke", NOT "migrations finished"
    - Add patch: `fromFieldPath: status.atProvider.manifest.status.conditions[?(@.type=="Ready")].status` → `toFieldPath: status.migrationsApplied`
    - This ensures XR only reports Ready when Spoke Atlas Operator actually completes migrations
  - Commit changes to feature branch
  - _Requirements: 2.6, Design: Feedback GAP 2, Design: Status Aggregation Strategy_

- [x] 3.8 Add readinessChecks and ToCompositeFieldPath status mappings to Object MRs
  - **CRITICAL - Object MR Readiness Caveat**: Object MR `Ready=True` means "Crossplane applied YAML to Spoke", NOT "underlying resource finished reconciling"
  - **Solution**: Add `ToCompositeFieldPath` patches to extract ACTUAL resource status from Spoke manifests back to XR
  - Add readinessChecks to github-migrations-pat ExternalSecret (wait for secret sync from Infisical)
  - Add readinessChecks to pooler Object MR (depends on TenantDatabase secret)
  - Add readinessChecks to postgrest-deployment Object MR (depends on pooler secret)
  - Add readinessChecks to atlasmigration Object MR (depends on pooler secret AND github secret)
  - **Add ToCompositeFieldPath patches**:
    - Pooler: Extract `status.atProvider.manifest.status.conditions[?(@.type=="Ready")].status` → `status.poolerReady`
    - PostgREST: Extract `status.atProvider.manifest.status.conditions[?(@.type=="Available")].status` → `status.postgrestReady`
    - AtlasMigration: Already added in 3.7
  - Configure `type: MatchCondition` with `matchCondition.type: Ready`
  - Commit changes to feature branch
  - _Design: Dependency Handling section, Design: Status Aggregation Strategy_

- [ ] 3.9 Manual Validation - Composition Resources
  - **What to verify**: Composition provisions all 12 Spoke resources correctly with proper status aggregation and GitHub PAT credentials
  - **How to verify**:
    - **PREREQUISITE**: Ensure GitHub PAT exists in Infisical secret store with key 
    - Deploy test tenant XR to Hub cluster
    - Wait for Crossplane reconciliation (check `kubectl get ainativesaas test-tenant-001 -o yaml` for Ready condition)
    - **Verify XR Status Aggregation**:
      - `kubectl get ainativesaas test-tenant-001 -o yaml | yq .status`
      - Check `status.databaseReady` (from TenantDatabase XR - rich status)
      - Check `status.poolerReady` (from pooler Object MR via ToCompositeFieldPath - ACTUAL Pooler CR status from Spoke)
      - Check `status.postgrestReady` (from postgrest Object MR via ToCompositeFieldPath - ACTUAL Deployment status from Spoke)
      - Check `status.migrationsApplied` (from atlasmigration Object MR via ToCompositeFieldPath - ACTUAL AtlasMigration CR status from Spoke)
      - **CRITICAL**: Verify status reflects TRUE readiness, not just "YAML applied"
        - If migrations still running, `status.migrationsApplied` should be `false` or missing
        - Only when Atlas Operator completes migrations should `status.migrationsApplied` become `true`
      - Verify `status.conditions[?(@.type=="Ready")].status == "True"` when all sub-resources ready
    - **Verify Spoke Resources**:
      - `kubectl get namespace tenant-test-tenant-001 -o yaml` - check label `managed-by: crossplane`
      - `kubectl get serviceaccount -n tenant-test-tenant-001` - verify tenant-test-tenant-001 exists
      - `kubectl get role -n tenant-test-tenant-001` - verify tenant-test-tenant-001-role exists
      - `kubectl get rolebinding -n tenant-test-tenant-001` - verify tenant-test-tenant-001-binding exists
      - `kubectl get resourcequota -n tenant-test-tenant-001` - verify tenant-test-tenant-001-quota exists with correct limits
      - **Verify GitHub PAT secret**: `kubectl get secret github-migrations-pat -n tenant-test-tenant-001 -o yaml` - should exist with `token` key
      - **Verify AtlasMigration Git config**: `kubectl get atlasmigration -n tenant-test-tenant-001 -o yaml`
        - Check `spec.dir.url == "https://github.com/kamarkat/kamarkat.git"`
        - Check `spec.dir.credentials.secretRef.name == "github-migrations-pat"`
        - Check `spec.dir.credentials.secretRef.key == "token"`
    - **Verify Dependency Ordering**:
      - Check TenantDatabase XR created secret: `kubectl get secret tenant-test-tenant-001-db-credentials -n tenant-test-tenant-001`
      - Check GitHub PAT ExternalSecret synced: `kubectl get externalsecret github-migrations-pat -n tenant-test-tenant-001 -o yaml | yq .status.conditions`
      - Check Pooler references secret: `kubectl get pooler -n platform-data test-tenant-001-pooler -o yaml | yq .spec.pgbouncer.authQueryUser.secretRef.name`
      - Check PostgREST references pooler secret: `kubectl get deployment -n tenant-test-tenant-001 postgrest-test-tenant-001 -o yaml | yq '.spec.template.spec.containers[0].env[] | select(.name=="PGRST_DB_URI").valueFrom.secretKeyRef.name'`
      - **Check AtlasMigration references both secrets**: Database connection secret AND GitHub PAT
    - **Verify Retry Behavior** (if TenantDatabase or ESO slow):
      - Check Object MR status shows `Pending` or `SecretNotFound` while waiting for secrets
      - After secrets created, verify Object MRs transition to `Ready`
      - **Verify ToCompositeFieldPath status propagation**:
        - While migrations running: `kubectl get ainativesaas test-tenant-001 -o yaml | yq .status.migrationsApplied` should be `false` or empty
        - After migrations complete: Should become `true`
        - If migrations fail: Should remain `false` with error in AtlasMigration CR status
  - **Expected outcome**: All 12 resources created by Crossplane, XR status aggregates correctly with TRUE readiness (not just "YAML applied"), dependencies resolve in order, AtlasMigration uses Git URL with GitHub PAT credentials, status mapping works correctly
  - **Approval gate**: Proceed to Phase 4 only after approval

---

## Phase 4: ArgoCD Migration Safety & ApplicationSet Cleanup

- [ ] 4.0 Orphan existing ArgoCD Applications to prevent data loss
  - **CRITICAL SAFETY STEP**: Before deleting ApplicationSet, orphan existing Applications to prevent cascading deletion of Spoke resources
  - **Why**: ApplicationSet has `prune: true` configured. Deleting it triggers ArgoCD to delete all child Applications, which cascades to delete ALL managed Spoke resources including Namespaces (causing immediate PVC/database deletion and data loss)
  - **How to orphan (MUST use Option B - deterministic, no race conditions)**:
    - **Option B (REQUIRED)**: Manually patch each existing Application to remove finalizer
      ```bash
      kubectl get applications -n argocd -l app.kubernetes.io/instance=tenant-spoke-provisioning \
        -o name | xargs -I {} kubectl patch {} -n argocd --type=json \
        -p='[{"op": "remove", "path": "/metadata/finalizers"}]'
      ```
    - **Why Option B over Option A**: Patching Applications directly is 100% deterministic. If you patch the ApplicationSet template (Option A), ArgoCD must reconcile the AppSet and propagate finalizer removal to child Apps. If you delete the AppSet before ArgoCD finishes that loop, finalizers remain and Spoke resources get deleted. Option B eliminates race conditions.
  - **Verify orphaning**:
    - `kubectl get applications -n argocd -l app.kubernetes.io/instance=tenant-spoke-provisioning -o yaml`
    - Check `metadata.finalizers` is empty or does NOT contain `resources-finalizer.argocd.argoproj.io`
  - **Expected outcome**: Existing Applications have no finalizers, safe to delete ApplicationSet without cascading deletes
  - **Approval gate**: Proceed to 4.1 only after verifying orphaning successful
  - _Design: Feedback GAP 1 - ArgoCD Pruning Risk_

- [ ] 4.1 Remove second ApplicationSet from platform-tenant-applicationset.yaml
  - Open `manifests/argocd/apps/platform-tenant-applicationset.yaml`
  - Delete lines 60-120 (entire `tenant-spoke-provisioning` ApplicationSet)
  - Keep `tenant-xr-provisioning` ApplicationSet unchanged
  - Commit changes to feature branch
  - Push to trigger ArgoCD sync
  - _Requirements: 2.1_

- [ ] 4.2 Manual Validation - ApplicationSet Cleanup
  - **What to verify**: Only ONE ArgoCD Application exists per tenant, orphaned resources remain running
  - **How to verify**:
    - `kubectl get applications -n argocd -l tenant-id=test-tenant-001`
    - Count Applications returned (should be exactly 1)
    - Verify Application name matches pattern `{tenantId}-xr` (not `{tenantId}-spoke`)
    - **Verify orphaned resources still exist**:
      - `kubectl get namespace tenant-test-tenant-001` - should STILL exist (orphaned from ArgoCD)
      - `kubectl get pods -n tenant-test-tenant-001` - should STILL be running
      - Database connection test - should STILL work
    - Query Spoke cluster: `kubectl get namespace tenant-test-tenant-001 -o yaml`
    - Check label `app.kubernetes.io/managed-by` should be `argocd` (will transition to `crossplane` after Crossplane adopts resources)
  - **Expected outcome**: Single Application exists, orphaned Spoke resources remain running (no data loss), ready for Crossplane adoption
  - **Approval gate**: Proceed to Phase 5 only after approval

---

## Phase 5: End-to-End Validation

- [ ] 5.1 Deploy test tenant and verify ADR 005 compliance
  - **What to verify**: Complete tenant provisioning flow follows ADR 005 pattern
  - **How to verify**:
    - Create tenant values.yaml with tenantId, tier, resourceQuota, ownerEmail
    - Commit to Git → ArgoCD syncs XR to Hub
    - Wait for Crossplane reconciliation
    - Verify Hub: `kubectl get ainativesaas {tenantId} -n tenant-{tenantId}` - Ready=True
    - Verify Spoke: All 12 resources exist (Namespace, ServiceAccount, Role, RoleBinding, ResourceQuota, Database, Pooler, Pooler Secret, PostgREST Deployment, PostgREST Service, GitHub PAT Secret, AtlasMigration)
    - Verify AtlasMigration: `kubectl get atlasmigration -n tenant-{tenantId} -o yaml` - spec.dir.url contains Git URL with credentials block
    - Verify NO ConfigMap: `kubectl get configmap tenant-{tenantId}-migrations -n tenant-{tenantId}` - should NOT exist
  - **Expected outcome**: Tenant fully provisioned via Crossplane, no ArgoCD-managed Spoke resources, no ConfigMap migrations, GitHub PAT credentials working

- [ ] 5.2 Verify preservation of existing tenant functionality
  - **What to verify**: Existing tenant operations unchanged
  - **How to verify**:
    - Database connection: `kubectl exec -n tenant-{tenantId} {pooler-pod} -- psql -c "SELECT 1"` - should succeed
    - PostgREST API: `curl https://postgrest.{tenantId}.example.com/health` - should return 200
    - Metrics collection: Query VictoriaMetrics for `cnpg_*` metrics from tenant namespace - should exist
    - Status sync: Query Hub Centralised DB for tenant status record - should show "ready"
  - **Expected outcome**: All existing functionality works, no regressions

- [ ] 5.3 Verify garbage collection does NOT affect shared infrastructure
  - **What to verify**: Deleting tenant does NOT break other tenants on same Spoke Pool
  - **How to verify**:
    - Deploy second test tenant (test-tenant-002) to same Spoke Pool
    - Verify both tenants operational
    - Delete first tenant: `kubectl delete ainativesaas test-tenant-001 -n tenant-test-tenant-001`
    - Wait for Crossplane garbage collection
    - Verify Spoke: `kubectl get namespace tenant-test-tenant-001` - should NOT exist
    - Verify second tenant UNAFFECTED:
      - `kubectl get namespace tenant-test-tenant-002` - should exist
      - Database connection for tenant-002 - should succeed
      - PostgREST API for tenant-002 - should return 200
      - Metrics collection for tenant-002 - should continue
  - **Expected outcome**: First tenant fully deleted, second tenant unaffected, no cluster-wide resource deletion

- [ ] 5.4 Verify status aggregation and debugging workflow
  - **What to verify**: XR status correctly aggregates Native MR (rich) + Object MR (generic) status
  - **How to verify**:
    - Check XR status: `kubectl get ainativesaas {tenantId} -o yaml | yq .status`
    - Verify status fields populated:
      - `databaseReady: true` (from TenantDatabase XR - rich status with detailed error if false)
      - `poolerReady: true` (from pooler Object MR - generic Ready condition)
      - `postgrestReady: true` (from postgrest Object MR - generic Ready condition)
      - `migrationsApplied: true` (from atlasmigration Object MR - generic Ready condition)
    - Verify overall Ready condition: `status.conditions[?(@.type=="Ready")].status == "True"`
    - **Test debugging workflow**:
      - Drill into TenantDatabase: `kubectl get tenantdatabase {tenant} -o yaml | yq .status` - should show rich status (roleReady, grantsReady, etc.)
      - Drill into Object MR: `kubectl get object {tenant}-namespace -o yaml | yq .status` - should show generic status (Ready condition only)
    - **Test failure scenario** (optional):
      - Temporarily break TenantDatabase (e.g., invalid database name)
      - Verify XR shows `databaseReady: false` with detailed error message
      - Verify downstream Object MRs remain `Pending` (waiting for secret)
  - **Expected outcome**: Status aggregation works correctly with TRUE readiness (ToCompositeFieldPath extracts actual Spoke resource status), debugging workflow clear, failure modes understood

- [ ] 5.5 Manual Validation - End-to-End Compliance
  - **What to verify**: Complete ADR 005 compliance across all phases
  - **How to verify**:
    - Review all previous validation checkpoints
    - Confirm XRD schema includes resourceQuota and ownerEmail
    - Confirm Helm template maps values correctly
    - Confirm Composition provisions all 11 resources
    - Confirm single ApplicationSet exists
    - Confirm tenant provisioning works end-to-end
    - Confirm preservation of existing functionality
    - Confirm garbage collection isolation
    - **Confirm status aggregation strategy**: XR status correctly combines Native MR (rich) + Object MR (generic) status
    - **Confirm dependency handling**: TenantDatabase → Pooler → PostgREST → AtlasMigration ordering works with retries
    - **Confirm Object MR guardrails**: Team understands when to use Native MR vs Object MR (see design spec "When NOT to Use Object MRs")
  - **Expected outcome**: All validations pass, ADR 005 fully compliant, observability strategy clear, dependency handling robust
  - **Approval gate**: Mark bugfix complete after approval

---

## Notes

- **NO automated tests**: All validation is manual as per requirements
- **Phase ordering**: Each phase must complete manual validation before proceeding to next phase
- **GitOps compliance**: All changes via Git commits, ArgoCD reconciles (no `kubectl apply` for infrastructure)
- **Approval gates**: Explicit approval required at end of each phase before picking next phase tasks
