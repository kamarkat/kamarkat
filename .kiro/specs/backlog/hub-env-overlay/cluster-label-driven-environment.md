# Cluster-Label Driven Environment Configuration

## Overview

This document describes the implementation of environment-aware configuration using ArgoCD ApplicationSets and cluster labels. This approach treats the Hub cluster as a first-class citizen in the fleet, enabling dynamic environment resolution without hardcoded paths.

## Architecture Decision

**Pattern**: Cluster-Label Driven ApplicationSets (Approach 5)

**Status**: ✅ **APPROVED** - This is the idiomatic approach for PaaS platforms built on ArgoCD + GitOps

**Why This is the "Right" Pattern**:

This aligns with how mature platforms operate:
- **Environment is a property of the cluster**, not embedded in app manifests
- **Applications are generated**, not handcrafted
- **Git defines desired state**; cluster metadata drives variation

**What Makes It Idiomatic (Not Just "Good")**:

It checks all the boxes that define a real PaaS architecture:

1. **Declarative** → No runtime mutation tricks
2. **Composable** → Same app definition works across all environments
3. **Scalable** → Adding clusters requires zero app changes
4. **Separation of Concerns**:
   - Platform team → Defines ApplicationSet
   - Cluster → Declares its identity (labels)
   - Git → Defines app behavior

**The Key Architectural Win**:

We've eliminated this anti-pattern:
> "App decides which environment it runs in"

And replaced it with:
> "Platform decides what runs on a cluster"

That's a fundamental PaaS principle.

## Why This Approach:
- Unified model for Hub and Spoke clusters
- Multi-hub ready (EU-Hub, US-Hub, etc.)
- Immutable source of truth (Git + Cluster Labels)
- MCP-first compatible (agents can mutate cluster labels)
- No chicken-and-egg bootstrapping problems

## Standardized Label Schema

**CRITICAL**: Labels must be standardized strictly. If labels drift, the entire control plane becomes unpredictable.

### Required Labels for All Clusters

```yaml
metadata:
  labels:
    # Platform Type (REQUIRED)
    platform-type: hub | spoke
    
    # Environment (REQUIRED)
    platform-env: dev | staging | prod
    
    # Region (REQUIRED)
    region: ap-south-1 | us-east-1 | eu-central-1
    
    # ArgoCD Secret Type (REQUIRED for cluster secrets)
    argocd.argoproj.io/secret-type: cluster
```

### Hub-Specific Labels

```yaml
metadata:
  labels:
    platform-type: hub
    platform-env: dev
    region: ap-south-1
    cluster-name: hub-cp  # Optional: Human-readable name
```

### Spoke-Specific Labels

```yaml
metadata:
  labels:
    platform-type: spoke
    platform-env: prod
    region: ap-south-1
    tenant-id: acme-corp  # Required for spokes
    spoke-tier: pool | silo  # Required for spokes
```

### Label Validation Rules

1. **Immutability**: `platform-type` should never change after bootstrap
2. **Controlled Values**: Use enums, not free-form strings
3. **Validation**: Bootstrap CLI must validate labels before creating secrets
4. **Documentation**: All valid label values must be documented

### Label Naming Convention

- Use `platform-*` prefix for Zero-Ops platform labels
- Use `argocd.argoproj.io/*` prefix for ArgoCD-specific labels
- Use kebab-case for label keys
- Use lowercase for label values
- No spaces or special characters

## Implementation Steps

### Phase 1: Bootstrap CLI Enhancement

**Status**: Documentation only - requires Go implementation

#### 1.1 Add `--env` Flag to Bootstrap Command

**File**: `cmd/hub/bootstrap.go`

```go
var (
    // Required flags
    clusterName string
    region      string
    imageID     string
    environment string  // NEW: Environment flag

    // ... existing optional flags
)

func newBootstrapCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "bootstrap",
        Short: "Bootstrap a Hub Cluster on Hetzner Cloud",
        Long: `Bootstrap a self-hosted Hub (Management) Cluster...`,
        PreRunE: validateFlags,
        RunE:    runBootstrap,
    }

    // Required flags
    cmd.Flags().StringVar(&clusterName, "name", "", "Hub Cluster name")
    cmd.Flags().StringVar(&region, "region", "", "Hetzner region (hel1, nbg1, hel1)")
    cmd.Flags().StringVar(&environment, "env", "dev", "Environment (dev, staging, prod)")
    
    // ... existing flags
    
    cmd.MarkFlagRequired("name")
    cmd.MarkFlagRequired("region")
    cmd.MarkFlagRequired("env")  // NEW: Make env required
    
    return cmd
}
```

#### 1.2 Validate Environment Flag

```go
func validateFlags(cmd *cobra.Command, args []string) error {
    // ... existing validations
    
    // Validate environment (STRICT ENUM)
    validEnvs := map[string]bool{
        "dev":     true,
        "staging": true,
        "prod":    true,
    }
    if !validEnvs[environment] {
        return fmt.Errorf("invalid environment: must be one of dev, staging, prod")
    }
    
    // Validate region (STRICT ENUM)
    validRegions := map[string]bool{
        "ap-south-1":   true,
        "us-east-1":    true,
        "eu-central-1": true,
        "hel1":         true,  // Hetzner regions
        "nbg1":         true,
        "hel1":         true,
    }
    if !validRegions[region] {
        return fmt.Errorf("invalid region: must be one of ap-south-1, us-east-1, eu-central-1, hel1, nbg1, hel1")
    }
    
    return nil
}
```

**CRITICAL**: Strict validation prevents label drift and ensures predictable control plane behavior.

#### 1.3 Update Bootstrap State

**File**: `internal/hub/state/state.go`

```go
type BootstrapState struct {
    Version          string          `json:"version"`
    ClusterName      string          `json:"clusterName"`
    Region           string          `json:"region"`
    Environment      string          `json:"environment"`      // NEW
    NetworkCIDR      string          `json:"networkCIDR"`
    CurrentPhase     BootstrapPhase  `json:"currentPhase"`
    CompletedPhases  []BootstrapPhase `json:"completedPhases"`
    BootstrapContext string          `json:"bootstrapContext"`
    MgmtKubeconfig   string          `json:"mgmtKubeconfig"`
    Timestamp        time.Time       `json:"timestamp"`
    Metadata         map[string]string `json:"metadata,omitempty"`
}
```

#### 1.4 Create ArgoCD Cluster Secret During Post-Boot

**File**: `internal/hub/components/argocd.go`

Create new function:

```go
package components

import (
    "context"
    "fmt"
    "os/exec"
    
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CreateHubClusterSecret creates the ArgoCD cluster secret for the Hub itself
// This enables ApplicationSets to treat the Hub as a labeled cluster
// 
// IMPORTANT: ArgoCD automatically registers the local cluster as "in-cluster" with
// server URL "https://kubernetes.default.svc". By creating a secret with the exact
// same server URL, ArgoCD merges our custom labels with its internal record.
func CreateHubClusterSecret(ctx context.Context, kubeconfig, clusterName, environment, region string) error {
    secret := &corev1.Secret{
        TypeMeta: metav1.TypeMeta{
            APIVersion: "v1",
            Kind:       "Secret",
        },
        ObjectMeta: metav1.ObjectMeta{
            Name:      "hub-cluster-secret",
            Namespace: "platform-ops",
            Labels: map[string]string{
                // ArgoCD required label
                "argocd.argoproj.io/secret-type": "cluster",
                
                // Zero-Ops platform labels (STANDARDIZED)
                "platform-type": "hub",
                "platform-env":  environment,  // dev | staging | prod
                "region":        region,       // ap-south-1 | us-east-1 | eu-central-1
                "cluster-name":  clusterName,  // hub-cp, hub-eu, etc.
            },
        },
        Type: corev1.SecretTypeOpaque,
        StringData: map[string]string{
            "name":   "in-cluster",
            "server": "https://kubernetes.default.svc",
            // NOTE: We don't need the config TLS block for the local cluster.
            // ArgoCD handles local auth automatically. But it's safe to include
            // an empty config if needed for compatibility.
        },
    }
    
    // Apply the secret using kubectl
    cmd := exec.CommandContext(ctx, "kubectl",
        "--kubeconfig", kubeconfig,
        "apply", "-f", "-",
    )
    
    // Marshal secret to YAML and pipe to kubectl
    // ... implementation details
    
    return nil
}
```

**Technical Note**: 
- ArgoCD automatically creates an `in-cluster` cluster entry for the local cluster
- By creating a secret with `server: https://kubernetes.default.svc`, ArgoCD merges our labels
- This is standard behavior and won't cause "cluster already exists" errors
- The secret decoration pattern is used by advanced multi-cluster setups

**Label Standards**:
- Use `platform-type` not `cluster-type` for consistency
- Use `platform-env` not `environment` to avoid conflicts with other tools
- All label values must be lowercase
- All label values must be from validated enums

#### 1.5 Call CreateHubClusterSecret in Orchestrator

**File**: `internal/hub/bootstrap/orchestrator.go`

Add after Phase 8 (Post-Bootstrap Components):

```go
// Phase 8.5: Create Hub Cluster Secret for ApplicationSets
if !contains(bootstrapState.CompletedPhases, state.PhaseHubClusterSecret) {
    fmt.Println("\n[hub-cluster-secret] Creating ArgoCD cluster secret for Hub...")
    
    compInstaller := &components.Installer{
        Kubeconfig: mgmtKubeconfig,
    }
    
    if err := compInstaller.CreateHubClusterSecret(ctx, o.Environment, o.Region); err != nil {
        return fmt.Errorf("failed to create hub cluster secret: %w", err)
    }
    fmt.Println("[hub-cluster-secret] ✓ Hub cluster secret created")
    
    // Update state
    bootstrapState.CompletedPhases = append(bootstrapState.CompletedPhases, state.PhaseHubClusterSecret)
    if err := stateMgr.Save(bootstrapState); err != nil {
        return fmt.Errorf("failed to save state: %w", err)
    }
} else {
    fmt.Println("[hub-cluster-secret] ✓ Skipped (already completed)")
}
```

### Phase 2: ApplicationSet Migration

**Status**: Ready for implementation - YAML manifests only

#### 2.1 Create ApplicationSet for Hub Environment

**File**: `manifests/argocd/appsets/hub-environment-appset.yaml`

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: hub-environment
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  # Generator: Watch for clusters labeled as Hub
  generators:
  - clusters:
      selector:
        matchLabels:
          platform-type: hub  # STANDARDIZED LABEL
      values:
        # Extract environment from cluster labels
        platformEnv: '{{metadata.labels.platform-env}}'
        region: '{{metadata.labels.region}}'
        clusterName: '{{metadata.labels.cluster-name}}'
  
  # Template: Generate Application for each matching cluster
  template:
    metadata:
      name: 'hub-environment-{{name}}'
      namespace: platform-ops
      labels:
        platform-type: hub
        platform-env: '{{metadata.labels.platform-env}}'
        region: '{{metadata.labels.region}}'
    spec:
      project: default
      source:
        repoURL: https://github.com/kamarkat/kamarkat
        targetRevision: HEAD
        # DYNAMIC PATH: Resolved from cluster label
        # Uses platform-env label (dev | staging | prod)
        path: 'manifests/hub-core-services/hub-environment/overlays/{{metadata.labels.platform-env}}'
      destination:
        server: '{{server}}'
        namespace: platform-ops
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
          - ServerSideApply=true
        retry:
          limit: 15
          backoff:
            duration: 30s
            maxDuration: 10m
```

#### 2.2 Update App-of-Apps with Kustomize

**IMPORTANT**: Use Kustomize instead of multi-source to avoid sync behavior issues.

**File**: `manifests/argocd/app-of-apps.yaml`

Update to point to single Kustomize root:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: platform-core
  namespace: platform-ops
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: default
  source:
    repoURL: https://github.com/kamarkat/kamarkat
    targetRevision: HEAD
    path: manifests/argocd  # Single root directory with kustomization.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: platform-ops
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

**File**: `manifests/argocd/kustomization.yaml` (NEW)

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - apps/
  - appsets/
```

**File**: `manifests/argocd/apps/kustomization.yaml` (NEW)

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - api-gateway.yaml
  - argocd-cm.yaml
  - cert-manager-webhook-hetzner.yaml
  - external-dns.yaml
  - hcloud-ccm.yaml
  # NOTE: hub-environment.yaml will be DELETED (replaced by ApplicationSet)
  - hub-operator.yaml
  - ingress-config.yaml
  - ingress-nginx.yaml
  - network-policies.yaml
  - platform-argocd-fleet-registry-auth.yaml
  - platform-argocd-github-auth.yaml
  - platform-capi2argo.yaml
  - platform-cloudnative-pg.yaml
  - platform-cluster-bios-templates.yaml
  - platform-cluster-secret-store.yaml
  - platform-crossplane-config.yaml
  - platform-crossplane-providers.yaml
  - platform-crossplane.yaml
  - platform-database.yaml
  - platform-external-dns-secrets.yaml
  - platform-external-secrets-crds.yaml
  - platform-external-secrets.yaml
  - platform-grafana-alloy.yaml
  - platform-identity.yaml
  - platform-infisical.yaml
  - platform-kyverno.yaml
  - platform-nats.yaml
  - platform-prometheus-operator.yaml
  - platform-security-certificates.yaml
  - platform-spire.yaml
  - platform-spoke-bootstrap-templates.yaml
  - platform-spoke-catalog-appsets.yaml
  - platform-spoke-pool-manifests.yaml
  - platform-spoke-pool-xrds.yaml
  - platform-spoke-pools.yaml
  - platform-tenant-applicationset.yaml
  - platform-victoriametrics-alerts.yaml
  - platform-victoriametrics-infisical.yaml
  - platform-victoriametrics-ingress.yaml
  - platform-victoriametrics.yaml
```

**File**: `manifests/argocd/appsets/kustomization.yaml` (NEW)

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - hub-environment-appset.yaml
```

**Why Kustomize over multi-source?**
- Multi-source is designed for combining Helm charts with Git values
- Using it for multiple raw manifest directories can cause unexpected sync behavior
- Kustomize is natively supported and cleaner for this use case
- Single source of truth with explicit resource listing

#### 2.3 Delete Old Application

**Action**: Delete `manifests/argocd/apps/hub-environment.yaml`

The ApplicationSet will replace it with a dynamically generated Application.

### Phase 3: Validation & Testing

**Status**: Manual testing steps after Phase 2 implementation

#### 3.1 Verify Cluster Secret

```bash
kubectl get secret hub-cluster-secret -n platform-ops -o yaml
```

Expected output:
```yaml
metadata:
  labels:
    argocd.argoproj.io/secret-type: cluster
    platform-type: hub
    platform-env: dev
    region: ap-south-1
    cluster-name: hub-cp
```

#### 3.2 Verify ApplicationSet Generates Application

```bash
kubectl get applicationset hub-environment -n platform-ops
kubectl get application -n platform-ops | grep hub-environment
```

Expected: `hub-environment-in-cluster` Application created

#### 3.3 Verify Correct Overlay Path

```bash
kubectl get application hub-environment-in-cluster -n platform-ops -o yaml | grep path
```

Expected: `path: manifests/hub-core-services/hub-environment/overlays/dev`

#### 3.4 Test Environment Change

```bash
# Update cluster label (use standardized label name)
kubectl label secret hub-cluster-secret -n platform-ops platform-env=staging --overwrite

# Wait for ApplicationSet to reconcile (30s default)
sleep 30

# Verify new path
kubectl get application hub-environment-in-cluster -n platform-ops -o yaml | grep path
```

Expected: `path: manifests/hub-core-services/hub-environment/overlays/staging`

**IMPORTANT**: Only change `platform-env` label. Never change `platform-type` after bootstrap.

## Migration Path for Existing Clusters

For clusters already bootstrapped without the `--env` flag:

### Option 1: Manual Secret Creation

```bash
export PLATFORM_ENV=dev
export REGION=ap-south-1
export CLUSTER_NAME=hub-cp

kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: hub-cluster-secret
  namespace: platform-ops
  labels:
    argocd.argoproj.io/secret-type: cluster
    platform-type: hub
    platform-env: ${PLATFORM_ENV}
    region: ${REGION}
    cluster-name: ${CLUSTER_NAME}
type: Opaque
stringData:
  name: in-cluster
  server: https://kubernetes.default.svc
EOF
```

**CRITICAL**: Use standardized label names (`platform-type`, `platform-env`) not old names (`cluster-type`, `environment`).

### Option 2: Re-bootstrap with --env

```bash
./bin/hub bootstrap --name hub-cp --region hel1 --env dev --upgrade
```

## Technical Refinements (Pro-Tips)

### 1. Kustomize for App-of-Apps Structure

**Why not multi-source?**
- ArgoCD's `sources:` array is designed for combining Helm charts with Git values
- Using it for multiple raw manifest directories can cause unexpected sync behavior
- Kustomize is natively supported and cleaner for this use case

**Implementation**:
- Single `source.path: manifests/argocd` pointing to Kustomize root
- `manifests/argocd/kustomization.yaml` lists `apps/` and `appsets/` as resources
- Each subdirectory has its own `kustomization.yaml` listing YAML files

### 2. The `in-cluster` Secret Decoration Pattern

**How it works**:
- ArgoCD automatically registers the local cluster as `in-cluster` with server `https://kubernetes.default.svc`
- By creating a secret with the **exact same server URL**, ArgoCD merges our custom labels with its internal record
- This is standard behavior in advanced multi-cluster setups
- No "cluster already exists" errors because we're decorating, not duplicating

**Key points**:
- Must use exact server URL: `https://kubernetes.default.svc`
- Must use exact name: `in-cluster`
- Labels are merged with ArgoCD's internal cluster record
- No TLS config needed for local cluster (ArgoCD handles auth automatically)

## Benefits of This Approach

### 1. Unified Fleet Model
Hub and Spoke clusters use the same pattern:
- Spokes: Labeled with `platform-type: spoke`, `platform-env: prod`, `tenant-id: acme`
- Hub: Labeled with `platform-type: hub`, `platform-env: dev`, `region: ap-south-1`

### 2. Multi-Hub Ready
Scale to multiple hubs without code changes:
```yaml
# EU Hub
labels:
  platform-type: hub
  platform-env: prod-eu
  region: eu-central-1
  cluster-name: hub-eu

# US Hub
labels:
  platform-type: hub
  platform-env: prod-us
  region: us-east-1
  cluster-name: hub-us
```

ApplicationSet automatically deploys correct overlays to each.

### 3. MCP-First Compatible
AI agents can change environment by mutating cluster labels:
```go
// MCP tool: change_hub_environment
func ChangeHubEnvironment(ctx context.Context, newEnv string) error {
    // Validate newEnv is in allowed enum (dev | staging | prod)
    // Update cluster secret label: platform-env
    // ApplicationSet automatically reconciles
}
```

**CRITICAL**: MCP tools must validate label values against strict enums to prevent drift.

### 4. GitOps Native
- Git remains 100% source of truth for manifests
- Cluster labels are the "identity card"
- No runtime ConfigMaps or environment variables
- Deterministic and auditable

## Troubleshooting

### ApplicationSet not generating Application
- Check cluster secret exists: `kubectl get secret hub-cluster-secret -n platform-ops`
- Verify labels use standardized names: `kubectl get secret hub-cluster-secret -n platform-ops -o yaml | grep labels -A 10`
- Confirm `platform-type: hub` label exists (not `cluster-type`)
- Confirm `platform-env` label exists (not `environment`)
- Check ApplicationSet status: `kubectl describe applicationset hub-environment -n platform-ops`

### Wrong overlay path
- Verify environment label: `kubectl get secret hub-cluster-secret -n platform-ops -o jsonpath='{.metadata.labels.platform-env}'`
- Check ApplicationSet template: `kubectl get applicationset hub-environment -n platform-ops -o yaml | grep path`
- Ensure overlay directory exists: `ls -la manifests/hub-core-services/hub-environment/overlays/`

### Label drift detected
- Audit all cluster secrets: `kubectl get secrets -n platform-ops -l argocd.argoproj.io/secret-type=cluster -o yaml`
- Verify all labels use standardized names
- Check for typos in label values (must be lowercase, no spaces)
- Validate against allowed enums (dev | staging | prod)

### Application not syncing
- Check Application status: `kubectl get application hub-environment-in-cluster -n platform-ops`
- View sync errors: `kubectl describe application hub-environment-in-cluster -n platform-ops`

## Implementation Checklist

### Phase 1: Bootstrap CLI (Requires Go Development)
- [ ] Add `--env` flag to `cmd/hub/bootstrap.go`
- [ ] Add environment validation (dev/staging/prod)
- [ ] Update `internal/hub/state/state.go` with Environment field
- [ ] Create `internal/hub/components/argocd.go::CreateHubClusterSecret()`
- [ ] Call CreateHubClusterSecret in orchestrator post-boot phase
- [ ] Add new bootstrap phase: `PhaseHubClusterSecret`
- [ ] Update bootstrap state management
- [ ] Test: `./bin/hub bootstrap --name test --region hel1 --env dev`

### Phase 2: ApplicationSet Migration (YAML Only - Can Start Now)
- [ ] Create `manifests/argocd/kustomization.yaml`
- [ ] Create `manifests/argocd/apps/kustomization.yaml` (list all apps)
- [ ] Create `manifests/argocd/appsets/` directory
- [ ] Create `manifests/argocd/appsets/kustomization.yaml`
- [ ] Create `manifests/argocd/appsets/hub-environment-appset.yaml`
- [ ] Update `manifests/argocd/app-of-apps.yaml` to point to kustomize root
- [ ] Delete `manifests/argocd/apps/hub-environment.yaml`
- [ ] Test: `kustomize build manifests/argocd`

### Phase 3: Manual Migration for Existing Cluster
- [ ] Create hub-cluster-secret manually with environment label
- [ ] Apply updated app-of-apps
- [ ] Verify ApplicationSet generates Application
- [ ] Verify correct overlay path resolution
- [ ] Test environment label change
- [ ] Monitor ArgoCD sync status

### Phase 4: Documentation & Validation
- [ ] Update README with new bootstrap command
- [ ] Document migration path for existing clusters
- [ ] Add troubleshooting guide
- [ ] Test multi-environment scenarios
- [ ] Validate MCP agent compatibility

## Execution Order

**For Existing Cluster (Current State)**:
1. Start with Phase 2 (YAML manifests) - can be done immediately
2. Manually create cluster secret (Phase 3)
3. Test and validate
4. Later: Implement Phase 1 (CLI) for future clusters

**For New Clusters (Future State)**:
1. Implement Phase 1 (CLI enhancement)
2. Bootstrap with `--env` flag
3. ApplicationSet automatically works
4. No manual steps needed

## Future Enhancements

### 1. Multi-Region Support
Add region-specific overlays:
```
overlays/
├── dev/
├── staging/
├── prod-eu/
└── prod-us/
```

### 2. Feature Flags via Labels
Add feature labels to enable/disable capabilities:
```yaml
labels:
  platform-type: hub
  platform-env: dev
  feature-ai-agents: enabled
  feature-multi-tenancy: enabled
```

**IMPORTANT**: Feature flag labels must also use strict enums (enabled | disabled) to prevent drift.

### 3. Automated Environment Promotion
MCP tool to promote environments:
```bash
# Promote staging → prod
mcp-tool promote_environment --from staging --to prod
# Updates cluster label, ApplicationSet reconciles
```

**Validation Required**: MCP tool must validate:
- Source environment exists
- Target environment is valid enum value
- Promotion path is allowed (e.g., dev → staging → prod, not dev → prod directly)

## Label Governance

### Preventing Label Drift

**Problem**: If labels drift (typos, wrong values, inconsistent naming), the control plane becomes unpredictable.

**Solution**: Implement strict governance:

1. **Bootstrap-Time Validation**: CLI validates all labels before creating secrets
2. **Admission Controller**: Use Kyverno/OPA to enforce label schemas
3. **Audit Logging**: Track all label changes
4. **MCP Tool Validation**: All MCP tools must validate labels against enums
5. **Documentation**: Maintain single source of truth for valid label values

### Label Change Policy

**Allowed Changes**:
- `platform-env`: Can change (dev → staging → prod)
- `region`: Can change (for migration scenarios)
- `cluster-name`: Can change (for renaming)

**Forbidden Changes**:
- `platform-type`: NEVER change after bootstrap (hub cannot become spoke)
- `argocd.argoproj.io/secret-type`: NEVER change (breaks ArgoCD)

### Label Audit Checklist

Run this audit regularly:

```bash
# 1. Check all cluster secrets use standardized labels
kubectl get secrets -n platform-ops \
  -l argocd.argoproj.io/secret-type=cluster \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.labels}{"\n"}{end}'

# 2. Verify platform-type values
kubectl get secrets -n platform-ops \
  -l argocd.argoproj.io/secret-type=cluster \
  -o jsonpath='{range .items[*]}{.metadata.labels.platform-type}{"\n"}{end}' | sort | uniq

# 3. Verify platform-env values
kubectl get secrets -n platform-ops \
  -l argocd.argoproj.io/secret-type=cluster \
  -o jsonpath='{range .items[*]}{.metadata.labels.platform-env}{"\n"}{end}' | sort | uniq

# 4. Check for old label names (should return empty)
kubectl get secrets -n platform-ops \
  -l cluster-type \
  -o name

kubectl get secrets -n platform-ops \
  -l environment \
  -o name
```

Expected results:
- `platform-type`: Only `hub` or `spoke`
- `platform-env`: Only `dev`, `staging`, or `prod`
- Old labels: Empty (no results)
