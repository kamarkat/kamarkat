# Zero-Ops Validation Scripts

Read-only assertion scripts for validating cluster state after GitOps changes.

**Rule:** These scripts are **read-only**. They assert state — they never mutate
it. No `kubectl apply`, no `helm install`, no direct resource creation.
All infrastructure changes go through Git → ArgoCD. See `k8-developer-workflow.md`.

---

## Scripts

### `validate-tenant-workloads.sh`

Validates that all fleet-registry changes for a specific tenant are fully applied
and healthy on the spoke cluster. Run this after pushing to fleet-registry and
waiting for ArgoCD to sync.

**Checks (11 sections):**

| # | Section | What it validates |
|---|---------|-------------------|
| 0 | Preflight | Hub + spoke cluster reachability |
| 1 | ArgoCD Applications | `{tenant}-xr`, `{tenant}-spoke`, `{tenant}-workloads` all Synced + Healthy |
| 2 | Namespace | Exists on spoke, PSA `enforce=restricted` label present (ADR-021 Blocker 4) |
| 3 | Data Layer | AINativeSaaS XR Ready+Synced, pooler-app Secret, AtlasMigration Ready, migrations ConfigMap |
| 4 | Service Accounts | `bff-workload-sa`, `frontend-workload-sa`, `migration-sa` present (ADR-021 Blocker 6) |
| 5 | Network Policy | `bff-strict-egress-contract`, `frontend-strict-egress-contract` CiliumNetworkPolicies present (ADR-021 Blocker 3) |
| 6 | Migration Job | Custom migrations ConfigMap present, Job succeeded or cleaned up by TTL |
| 7 | BFF Workload | Argo Rollout phase=Healthy, Service has endpoints, pods Running |
| 8 | Frontend Workload | Argo Rollout phase=Healthy, Service has endpoints, pods Running |
| 9 | Ingress | Ingress has address assigned, TLS Secret present (ADR-021 Blocker 7) |
| 10 | Kyverno ABI | `enforce-tenant-abi` ClusterPolicy Ready, pods carry `tenant-id` label |
| 11 | Events | No Warning events in tenant namespace |

**Usage:**

```bash
# From the zero-ops repo root
./scripts/validate-tenant-workloads.sh <tenant-id>

# Examples
./scripts/validate-tenant-workloads.sh app-creator
./scripts/validate-tenant-workloads.sh waypoint

# Override kubeconfig or spoke name
KUBECONFIG=~/.kamarkat/run/kubeconfig/hub.kubeconfig \
  WORKLOAD_CLUSTER=<your-workload-cluster> \
  ./scripts/validate-tenant-workloads.sh app-creator
```

**Environment variables:**

| Variable | Default | Description |
|----------|---------|-------------|
| `KUBECONFIG` | `~/.kamarkat/run/kubeconfig/hub.kubeconfig` | Hub cluster kubeconfig |
| `WORKLOAD_CLUSTER` | _(required)_ | ArgoCD cluster name of the workload cluster; the tenant chooses it (ADR-082), so there is no default |
| `ARGOCD_NS` | `platform-ops` | Namespace where ArgoCD runs |

**Exit codes:** `0` = all critical checks passed. `1` = one or more failures.

**Log output:** Written to `.state/logs/validate-tenant-workloads.log` (cleared on each run).

**Expected output (healthy tenant):**

```
═══════════════════════════════════════════════════════════
 Tenant Workload Validation — ADR-021 / ADR-022
 Tenant:     app-creator
 Namespace:  tenant-app-creator
═══════════════════════════════════════════════════════════

══════════════════════════════════════════
  1. ARGOCD APPLICATIONS
══════════════════════════════════════════
  ✅ ArgoCD app 'app-creator-xr': Synced + Healthy
  ✅ ArgoCD app 'app-creator-spoke': Synced + Healthy
  ✅ ArgoCD app 'app-creator-workloads': Synced + Healthy
...
╔══════════════════════════════════════════════╗
║   TENANT WORKLOAD VALIDATION SUMMARY         ║
╠══════════════════════════════════════════════╣
║  ✅ PASSED : 28
║  ❌ FAILED : 0
║  ⚠️  WARNED : 0
╠══════════════════════════════════════════════╣
║  🎉 ALL CRITICAL CHECKS PASSED               ║
╚══════════════════════════════════════════════╝
```

**Typical failure patterns and fixes:**

| Failure | Likely cause | Fix |
|---------|-------------|-----|
| ArgoCD app not found | fleet-registry commit not yet detected | Wait for ApplicationSet reconcile or force sync |
| ArgoCD app OutOfSync | Kustomize remote base fetch failed | Check ArgoCD app events; verify `?ref=` is reachable |
| Namespace PSA label missing | `universal-tenant` chart not re-rendered | Force sync `{tenant}-spoke` ArgoCD app |
| AINativeSaaS XR not Ready | Crossplane composition error | `kubectl describe ainativesaas <tenant>` on hub |
| AtlasMigration not Ready | Pooler secret not yet created | Wait for TenantDatabase XR to complete; check `{tenant}-pooler-app` secret |
| Rollout not found | Workload ApplicationSet not synced | Check `{tenant}-workloads` ArgoCD app; verify `workloads.gitPath` in values.yaml |
| Kyverno ClusterPolicy missing | `spoke-infrastructure` app not synced | Check `<workload-cluster>-infrastructure` ArgoCD app |

---

### `post-bootstrap-validate.sh`

Validates the full hub platform after initial bootstrap. Covers all platform
namespaces, ArgoCD, Crossplane, ESO, Infisical, CNPG, OpenMeter,
Ory identity stack, hub-operator, kube-sbt, ingress, spoke pool,
and observability.

SPOKEPOOL_NAME names the SpokePool this box declares. There is no default: one
used to exist (`spoke-pool-eu-prod-01`), so a run that omitted it validated a dev
box against the production pool name and reported the spoke checks as failures of
the box rather than of the invocation.

```bash
SPOKEPOOL_NAME=<your-workload-cluster> ENVIRONMENT=dev \
  KUBECONFIG=~/.kamarkat/run/kubeconfig/<cluster>.kubeconfig \
  ./scripts/post-bootstrap-validate.sh
```

---

### `test-kube-sbt-providers-manual.sh`

Interactive manual test script for kube-sbt OpenMeter providers. Guides through
subject registration, entitlement checking, and subscription creation via
port-forward. Read-only assertions only.

```bash
./scripts/test-kube-sbt-providers-manual.sh
```

---

### `test-openmeter-providers.sh`

Automated assertions for OpenMeter provider endpoints.

```bash
./scripts/test-openmeter-providers.sh
```

### `validate-spokepool-compositions.sh`

Regression guard for SpokePool Composition ClusterResourceSet resource
alignment. `function-patch-and-transform` addresses resources positionally
(`spec.forProvider.manifest.spec.resources[N].name`), so inserting or removing
an element silently shifts downstream patches. This script renders each
composition's cluster-resource-set `resources[]` through its index-targeted
patches (fixed test claim) and asserts every rendered name is non-empty,
unique, static namespace ConfigMaps stay unprefixed, and per-spoke resources
(`{claim}-<suffix>`) resolve exactly once.

```bash
./scripts/validate-spokepool-compositions.sh
```

Runs in CI on every PR touching `manifests/**` and via `make test`.

---

## GitOps Principle

These scripts validate state **after** ArgoCD has synced. They are the
"Day 1 GitOps" validation step:

```
1. Developer commits to fleet-registry or zero-ops
2. ArgoCD detects change and syncs to cluster
3. Developer runs validation script to assert end state
```

Never use these scripts to diagnose a problem and then fix it with `kubectl apply`.
Fix the Git source and let ArgoCD re-sync.
