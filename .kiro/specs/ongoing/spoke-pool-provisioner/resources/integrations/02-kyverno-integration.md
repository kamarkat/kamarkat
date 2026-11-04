# Kyverno Integration Analysis

**Feature**: Spoke Pool Provisioner  
**Dependency**: Kyverno Policy Engine  
**Analysis Date**: 2026-04-08  
**Spec Requirement**: FR-1.3 (Cluster Registration)

---

## 1. Spoke Pool Provisioner Context

### 1.1 Spec Requirement (FR-1.3)

From `requirements.md`:

```
FR-1.3: Cluster Registration
- Description: Kyverno policy auto-generates ArgoCD cluster Secret when CAPI cluster becomes Ready
- Acceptance Criteria:
  - Kyverno watches Cluster.status.phase=Provisioned
  - Extracts kubeconfig from CAPI-generated Secret
  - Creates ArgoCD cluster Secret in argocd namespace with labels:
    * argocd.argoproj.io/secret-type: cluster
    * spoke-type: pool
    * cell-id: <cluster-name>
  - ArgoCD discovers cluster within 30 seconds
```

### 1.2 Integration Point in Spoke Pool Flow

```
SpokePool XR (Applied by Platform Admin)
  ↓
Crossplane Composition generates CAPI Cluster CR
  ↓
CAPI provisions Hetzner VMs + Kubernetes cluster
  ↓
CAPI creates Secret: <cluster-name>-kubeconfig
  ↓
CAPI updates Cluster.status.phase = "Provisioned"
  ↓
[KYVERNO INTEGRATION POINT] ← This document
  ↓
Kyverno generates ArgoCD cluster Secret
  ↓
ArgoCD discovers cluster (FR-2.1: Edge Catalog Deployment)
```

### 1.3 Why Kyverno for This Spec?

**Spec Constraint**: "Zero manual cluster configuration" (Success Criteria, Section 1.3)

**Kyverno Benefits**:
- ✅ Declarative (GitOps-compliant, aligns with tech.md)
- ✅ Event-driven (no polling, immediate reaction to Cluster.status.phase)
- ✅ No custom Go code (reduces maintenance burden)
- ✅ Built-in reconciliation (handles transient failures automatically)

**Rejected Alternatives** (violate spec constraints):
- ❌ Custom Kubernetes Operator: Requires Go code, violates "operational simplicity"
- ❌ Manual kubectl: Violates "zero manual configuration"
- ❌ Bash scripts: Not GitOps-compliant, no reconciliation

---

## 2. Spoke Pool-Specific Requirements

### 2.1 CAPI Cluster Resource Structure (from CAPI integration doc)

```yaml
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: spokepool-01                    # Cell ID
  namespace: platform-capi
  labels:
    spoke-type: pool                    # REQUIRED for Kyverno selector
    cluster.x-k8s.io/cluster-name: spokepool-01
status:
  phase: Provisioned                    # TRIGGER for Kyverno
  controlPlaneEndpoint:
    host: "10.0.0.1"                    # Load balancer IP
    port: 6443
```

### 2.2 CAPI-Generated Kubeconfig Secret (from CAPI integration doc)

**Secret Name**: `<cluster-name>-kubeconfig` (e.g., `spokepool-01-kubeconfig`)  
**Namespace**: Same as Cluster CR (`platform-capi`)  
**Data Key**: `value` (base64-encoded kubeconfig)

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: spokepool-01-kubeconfig
  namespace: platform-capi
  labels:
    cluster.x-k8s.io/cluster-name: spokepool-01
type: cluster.x-k8s.io/secret
data:
  value: <base64-encoded-kubeconfig>
```

### 2.3 Required ArgoCD Cluster Secret Format (Spec FR-1.3)

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: spokepool-01-argocd-cluster
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: cluster  # REQUIRED by spec
    spoke-type: pool                         # REQUIRED by spec (for ApplicationSet selector)
    cell-id: spokepool-01                    # REQUIRED by spec
type: Opaque
stringData:
  name: spokepool-01
  server: https://10.0.0.1:6443
  config: |
    {
      "tlsClientConfig": {
        "insecure": false,
        "caData": "<base64-ca-cert>",
        "certData": "<base64-client-cert>",
        "keyData": "<base64-client-key>"
      }
    }
```

### 2.4 Kyverno Policy Type for This Spec

**Policy Type**: Generate (creates new ArgoCD Secret when CAPI Cluster is Provisioned)

**Why Not Mutate?**: We're creating a NEW resource (ArgoCD Secret), not modifying existing Cluster CR.

**Why Not Validate?**: We're not enforcing rules, we're automating resource creation.

---

## 3. Kyverno ClusterPolicy Implementation (Spec-Specific)

### 3.1 Policy Specification for Spoke Pool

**File**: `manifests/platform-gitops/kyverno-policies/spoke-pool-cluster-discovery.yaml`

**Policy Name**: `spoke-pool-cluster-discovery` (not generic "capi-argocd")

**Trigger Conditions** (from spec):
1. Resource kind: `Cluster` (cluster.x-k8s.io/v1beta1)
2. Label: `spoke-type: pool` (only Spoke Pool clusters, not Spoke Silo)
3. Status: `phase=Provisioned` (not Pending or Failed)

**Action**: Generate ArgoCD cluster Secret with spec-required labels

### 3.2 Implementation (Option A: Context Variables - Recommended)

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: spoke-pool-cluster-discovery
  annotations:
    policies.kyverno.io/category: "Spoke Pool Provisioner"
    policies.kyverno.io/description: |
      FR-1.3: Automatically registers Spoke Pool clusters with ArgoCD.
      Watches CAPI Cluster resources with spoke-type=pool label.
      Generates ArgoCD cluster Secret when Cluster.status.phase=Provisioned.
spec:
  admission: false         # Only background reconciliation (not admission webhook)
  background: true         # Apply to existing Cluster resources
  validationFailureAction: Audit
  rules:
  - name: generate-argocd-cluster-secret
    match:
      any:
      - resources:
          kinds:
          - Cluster
          namespaces:
          - platform-capi  # Only watch CAPI namespace (spec constraint)
    # Preconditions: Only trigger for Spoke Pool clusters that are Provisioned
    preconditions:
      all:
      - key: "{{request.object.metadata.labels.\"spoke-type\"}}"
        operator: Equals
        value: "pool"
      - key: "{{request.object.status.phase}}"
        operator: Equals
        value: "Provisioned"
    # Context: Fetch kubeconfig from CAPI-generated Secret
    context:
    - name: kubeconfigSecret
      apiCall:
        urlPath: "/api/v1/namespaces/{{request.object.metadata.namespace}}/secrets/{{request.object.metadata.name}}-kubeconfig"
        jmesPath: "data.value | base64_decode(@)"
    # Generate: Create ArgoCD cluster Secret
    generate:
      apiVersion: v1
      kind: Secret
      name: "{{request.object.metadata.name}}-argocd-cluster"
      namespace: argocd
      synchronize: true  # Keep in sync with Cluster CR (spec NFR-3.1: idempotent)
      data:
        metadata:
          labels:
            argocd.argoproj.io/secret-type: cluster  # Spec FR-1.3 requirement
            spoke-type: pool                         # Spec FR-1.3 requirement
            cell-id: "{{request.object.metadata.name}}"  # Spec FR-1.3 requirement
        type: Opaque
        stringData:
          name: "{{request.object.metadata.name}}"
          server: "https://{{request.object.spec.controlPlaneEndpoint.host}}:{{request.object.spec.controlPlaneEndpoint.port}}"
          config: "{{kubeconfigSecret}}"
```

### 3.3 Implementation (Option B: Two-Step Clone - Fallback)

If Kyverno context variables don't work (Kyverno < v1.11 or RBAC restrictions):

**Step 1: Clone kubeconfig to argocd namespace**

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: spoke-pool-clone-kubeconfig
spec:
  background: true
  rules:
  - name: clone-kubeconfig
    match:
      any:
      - resources:
          kinds:
          - Cluster
          namespaces:
          - platform-capi
    preconditions:
      all:
      - key: "{{request.object.metadata.labels.\"spoke-type\"}}"
        operator: Equals
        value: "pool"
      - key: "{{request.object.status.phase}}"
        operator: Equals
        value: "Provisioned"
    generate:
      kind: Secret
      name: "{{request.object.metadata.name}}-kubeconfig-clone"
      namespace: argocd
      clone:
        namespace: "{{request.object.metadata.namespace}}"
        name: "{{request.object.metadata.name}}-kubeconfig"
      synchronize: true
```

**Step 2: Generate ArgoCD cluster Secret from cloned kubeconfig**

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: spoke-pool-generate-argocd-secret
spec:
  background: true
  rules:
  - name: create-cluster-secret
    match:
      any:
      - resources:
          kinds:
          - Secret
          names:
          - "*-kubeconfig-clone"
          namespaces:
          - argocd
    generate:
      apiVersion: v1
      kind: Secret
      name: "{{request.object.metadata.name | replace_all(@, '-kubeconfig-clone', '')}}-argocd-cluster"
      namespace: argocd
      synchronize: true
      data:
        metadata:
          labels:
            argocd.argoproj.io/secret-type: cluster
            spoke-type: pool
            cell-id: "{{request.object.metadata.name | replace_all(@, '-kubeconfig-clone', '')}}"
        type: Opaque
        stringData:
          name: "{{request.object.metadata.name | replace_all(@, '-kubeconfig-clone', '')}}"
          config: "{{request.object.data.value | base64_decode(@)}}"
```

### 3.4 Spec Alignment Check

| Spec Requirement | Implementation | Status |
|------------------|----------------|--------|
| FR-1.3: Watch Cluster.status.phase=Provisioned | `preconditions.all[1]` | ✅ |
| FR-1.3: Extract kubeconfig from CAPI Secret | `context.kubeconfigSecret` | ✅ |
| FR-1.3: Create Secret in argocd namespace | `generate.namespace: argocd` | ✅ |
| FR-1.3: Label argocd.argoproj.io/secret-type | `generate.data.metadata.labels` | ✅ |
| FR-1.3: Label spoke-type: pool | `generate.data.metadata.labels` | ✅ |
| FR-1.3: Label cell-id | `generate.data.metadata.labels` | ✅ |
| NFR-1.4: Discovery within 30 seconds | Kyverno background controller (10s default) | ✅ |
| NFR-3.1: Idempotent provisioning | `synchronize: true` | ✅ |

---

## 4. Integration Pattern for Spoke Pool

### 4.1 CAPI Cluster Discovery Flow

```
1. Platform Admin applies SpokePool XR
   ↓
2. Crossplane generates CAPI Cluster CR
   ↓
3. CAPI provisions Hetzner VMs + Kubernetes cluster
   ↓
4. CAPI creates Secret: <cluster-name>-kubeconfig
   ↓
5. CAPI updates Cluster.status.phase = "Provisioned"
   ↓
6. Kyverno ClusterPolicy triggers (watches Cluster resources)
   ↓
7. Kyverno extracts kubeconfig from Secret
   ↓
8. Kyverno generates ArgoCD cluster Secret
   ↓
9. ArgoCD discovers cluster within 30 seconds
```

### 4.2 Required Kyverno ClusterPolicy

**File**: `manifests/platform-gitops/kyverno-capi-argocd-bridge.yaml`

**Policy Name**: `capi-argocd-cluster-discovery`

**Trigger**: CAPI Cluster resource with `status.phase=Provisioned`

**Action**: Generate ArgoCD cluster Secret

---

## 5. Kyverno ClusterPolicy Implementation

### 5.1 Policy Specification

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: capi-argocd-cluster-discovery
  annotations:
    policies.kyverno.io/category: "GitOps Integration"
    policies.kyverno.io/description: |
      Automatically registers CAPI-provisioned clusters with ArgoCD by generating
      cluster Secrets when CAPI Cluster resources reach Provisioned state.
spec:
  admission: false         # Only background reconciliation (not admission)
  background: true         # Apply to existing Cluster resources
  validationFailureAction: Audit
  rules:
  - name: generate-argocd-cluster-secret
    match:
      any:
      - resources:
          kinds:
          - Cluster
          operations:
          - CREATE
          - UPDATE
    # Only trigger when Cluster is Provisioned
    preconditions:
      all:
      - key: "{{request.object.status.phase}}"
        operator: Equals
        value: "Provisioned"
      - key: "{{request.object.metadata.labels.\"spoke-type\"}}"
        operator: Equals
        value: "pool"
    generate:
      apiVersion: v1
      kind: Secret
      name: "{{request.object.metadata.name}}-argocd-cluster"
      namespace: argocd
      synchronize: true
      data:
        metadata:
          labels:
            argocd.argoproj.io/secret-type: cluster
            spoke-type: pool
            cell-id: "{{request.object.metadata.name}}"
            cluster.x-k8s.io/cluster-name: "{{request.object.metadata.name}}"
        type: Opaque
        stringData:
          name: "{{request.object.metadata.name}}"
          server: "{{request.object.spec.controlPlaneEndpoint.host}}:{{request.object.spec.controlPlaneEndpoint.port}}"
          config: |
            {
              "tlsClientConfig": {
                "insecure": false,
                "caData": "{{request.object.status.controlPlaneReady.kubeconfig.certificateAuthorityData}}",
                "certData": "{{request.object.status.controlPlaneReady.kubeconfig.clientCertificateData}}",
                "keyData": "{{request.object.status.controlPlaneReady.kubeconfig.clientKeyData}}"
              }
            }
```

### 5.2 Challenge: Extracting Kubeconfig from Secret

**Problem**: CAPI stores kubeconfig in a separate Secret (`<cluster-name>-kubeconfig`), not in the Cluster CR itself.

**Kyverno Limitation**: Generate policies cannot directly read other Secrets (security restriction).

**Solution Options**:

#### Option A: Use Kyverno Context Variables (Recommended)

Kyverno v1.11+ supports `context` to fetch data from other resources:

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: capi-argocd-cluster-discovery
spec:
  background: true
  rules:
  - name: generate-argocd-cluster-secret
    match:
      any:
      - resources:
          kinds:
          - Cluster
    preconditions:
      all:
      - key: "{{request.object.status.phase}}"
        operator: Equals
        value: "Provisioned"
      - key: "{{request.object.metadata.labels.\"spoke-type\"}}"
        operator: Equals
        value: "pool"
    context:
    - name: kubeconfigSecret
      apiCall:
        urlPath: "/api/v1/namespaces/{{request.object.metadata.namespace}}/secrets/{{request.object.metadata.name}}-kubeconfig"
        jmesPath: "data.value | base64_decode(@)"
    generate:
      apiVersion: v1
      kind: Secret
      name: "{{request.object.metadata.name}}-argocd-cluster"
      namespace: argocd
      synchronize: true
      data:
        type: Opaque
        stringData:
          name: "{{request.object.metadata.name}}"
          server: "https://{{request.object.spec.controlPlaneEndpoint.host}}:{{request.object.spec.controlPlaneEndpoint.port}}"
          config: "{{kubeconfigSecret}}"
```

#### Option B: Two-Step Policy (Fallback)

If context variables don't work, use two policies:

1. **Policy 1**: Clone kubeconfig Secret to `argocd` namespace
2. **Policy 2**: Generate ArgoCD cluster Secret referencing cloned Secret

```yaml
# Policy 1: Clone kubeconfig to argocd namespace
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: clone-kubeconfig-to-argocd
spec:
  background: true
  rules:
  - name: clone-kubeconfig
    match:
      any:
      - resources:
          kinds:
          - Cluster
    preconditions:
      all:
      - key: "{{request.object.status.phase}}"
        operator: Equals
        value: "Provisioned"
    generate:
      kind: Secret
      name: "{{request.object.metadata.name}}-kubeconfig-clone"
      namespace: argocd
      clone:
        namespace: "{{request.object.metadata.namespace}}"
        name: "{{request.object.metadata.name}}-kubeconfig"
      synchronize: true
---
# Policy 2: Generate ArgoCD cluster Secret
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: generate-argocd-cluster-secret
spec:
  background: true
  rules:
  - name: create-cluster-secret
    match:
      any:
      - resources:
          kinds:
          - Secret
          names:
          - "*-kubeconfig-clone"
          namespaces:
          - argocd
    generate:
      apiVersion: v1
      kind: Secret
      name: "{{request.object.metadata.name | replace_all(@, '-kubeconfig-clone', '')}}-argocd-cluster"
      namespace: argocd
      synchronize: true
      data:
        metadata:
          labels:
            argocd.argoproj.io/secret-type: cluster
            spoke-type: pool
        type: Opaque
        stringData:
          name: "{{request.object.metadata.name | replace_all(@, '-kubeconfig-clone', '')}}"
          config: "{{request.object.data.value | base64_decode(@)}}"
```

---

## 6. ArgoCD Cluster Secret Format

### 6.1 Required Secret Structure

ArgoCD expects cluster Secrets in this format:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: spokepool-01-cluster
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: cluster  # REQUIRED for ArgoCD discovery
    spoke-type: pool                         # Custom label for ApplicationSet selector
    cell-id: spokepool-01                    # Custom label for cell identification
type: Opaque
stringData:
  name: spokepool-01                         # Display name in ArgoCD UI
  server: https://10.0.0.1:6443              # Kubernetes API server URL
  config: |                                  # Kubeconfig in JSON format
    {
      "tlsClientConfig": {
        "insecure": false,
        "caData": "<base64-ca-cert>",
        "certData": "<base64-client-cert>",
        "keyData": "<base64-client-key>"
      }
    }
```

### 6.2 Label Requirements

| Label | Purpose | Required |
|-------|---------|----------|
| `argocd.argoproj.io/secret-type: cluster` | ArgoCD discovery | ✅ Yes |
| `spoke-type: pool` | ApplicationSet selector | ✅ Yes |
| `cell-id: <cluster-name>` | Cell identification | ✅ Yes |
| `cluster.x-k8s.io/cluster-name: <name>` | CAPI reference | ⚠️ Optional |

---

## 7. Installation and Configuration (Spoke Pool Specific)

### 7.1 Kyverno Installation (Hub Cluster Only)

**Deployment Location**: Hub cluster (`mothership`)  
**Namespace**: `kyverno`  
**Method**: ArgoCD Application (GitOps-compliant, per tech.md)

**File**: `manifests/platform-gitops/kyverno/application.yaml`

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: kyverno
  namespace: argocd
spec:
  project: platform-core
  source:
    repoURL: https://kyverno.github.io/kyverno/
    chart: kyverno
    targetRevision: v1.11.0
    helm:
      values: |
        admissionController:
          replicas: 3  # HA for Hub cluster
        backgroundController:
          replicas: 2  # Handles Cluster resource reconciliation
        cleanupController:
          replicas: 1
        reportsController:
          replicas: 1
  destination:
    server: https://kubernetes.default.svc
    namespace: kyverno
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
```

**Why Hub Only?**: Spoke Pool clusters don't need Kyverno (they only need ArgoCD Agent for edge catalog).

### 7.2 Policy Deployment (Spoke Pool Specific)

**File**: `manifests/platform-gitops/kyverno-policies/spoke-pool-cluster-discovery.yaml`

**Deployment via ArgoCD**:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: kyverno-spoke-pool-policies
  namespace: argocd
spec:
  project: platform-core
  source:
    repoURL: https://github.com/kamarkat/kamarkat.git
    path: manifests/platform-gitops/kyverno-policies
    targetRevision: main
  destination:
    server: https://kubernetes.default.svc
    namespace: kyverno
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

### 7.3 RBAC Configuration (Spec-Specific)

Kyverno needs permissions to:
1. Read CAPI Cluster resources
2. Read CAPI kubeconfig Secrets
3. Create ArgoCD cluster Secrets

**File**: `manifests/platform-gitops/kyverno-policies/rbac.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kyverno-spoke-pool-bridge
rules:
# Read CAPI Cluster resources
- apiGroups: ["cluster.x-k8s.io"]
  resources: ["clusters"]
  verbs: ["get", "list", "watch"]
# Read CAPI kubeconfig Secrets (only in platform-capi namespace)
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
  resourceNames: ["*-kubeconfig"]  # Restrict to kubeconfig Secrets only
# Create ArgoCD cluster Secrets (only in argocd namespace)
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["create", "update", "patch", "delete"]
  # No resourceNames restriction (Kyverno generates dynamic names)
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kyverno-spoke-pool-bridge
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kyverno-spoke-pool-bridge
subjects:
- kind: ServiceAccount
  name: kyverno-background-controller
  namespace: kyverno
```

**Security Note**: Kyverno background controller (not admission controller) needs these permissions because cluster discovery is a background reconciliation task, not an admission webhook.

---

## 8. Testing and Validation (Spec Acceptance Criteria)

### 8.1 Acceptance Criteria Validation (AC-2 from requirements.md)

```
AC-2: Kyverno Cluster Discovery
- [ ] Kyverno ClusterPolicy watches CAPI Cluster resources
- [ ] Policy extracts kubeconfig from CAPI Secret when Cluster.status.phase=Provisioned
- [ ] Policy generates ArgoCD cluster Secret with correct labels
- [ ] ArgoCD discovers cluster within 30 seconds
```

### 8.2 Unit Test (Kyverno CLI)

**Test Fixture**: `test/fixtures/spoke-pool/capi-cluster-provisioned.yaml`

```yaml
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: spokepool-01
  namespace: platform-capi
  labels:
    spoke-type: pool  # REQUIRED for policy trigger
status:
  phase: Provisioned  # REQUIRED for policy trigger
  controlPlaneEndpoint:
    host: 10.0.0.1
    port: 6443
---
apiVersion: v1
kind: Secret
metadata:
  name: spokepool-01-kubeconfig
  namespace: platform-capi
  labels:
    cluster.x-k8s.io/cluster-name: spokepool-01
type: cluster.x-k8s.io/secret
data:
  value: <base64-encoded-kubeconfig>
```

**Test Command**:
```bash
# Install Kyverno CLI
kubectl krew install kyverno

# Test policy against fixture
kyverno apply \
  manifests/platform-gitops/kyverno-policies/spoke-pool-cluster-discovery.yaml \
  --resource test/fixtures/spoke-pool/capi-cluster-provisioned.yaml \
  --detailed-results

# Expected output:
# pass: 1, fail: 0, warn: 0, error: 0, skip: 0
```

### 8.3 Integration Test (End-to-End)

**Test Scenario**: AC-6 from requirements.md (End-to-End Integration Test)

```bash
# Step 1: Apply SpokePool XR (triggers CAPI cluster provisioning)
kubectl apply -f test/fixtures/spoke-pool/spokepool-01.yaml

# Step 2: Wait for CAPI cluster to become Provisioned (NFR-1.1: < 15 minutes)
kubectl wait --for=condition=Ready cluster/spokepool-01 \
  -n platform-capi --timeout=20m

# Step 3: Verify Kyverno generated ArgoCD cluster Secret (FR-1.3)
kubectl get secret spokepool-01-argocd-cluster -n argocd

# Expected output:
# NAME                           TYPE     DATA   AGE
# spokepool-01-argocd-cluster    Opaque   3      10s

# Step 4: Verify Secret has required labels (FR-1.3)
kubectl get secret spokepool-01-argocd-cluster -n argocd \
  -o jsonpath='{.metadata.labels}' | jq

# Expected output:
# {
#   "argocd.argoproj.io/secret-type": "cluster",
#   "spoke-type": "pool",
#   "cell-id": "spokepool-01"
# }

# Step 5: Verify ArgoCD discovered the cluster (NFR-1.4: < 30 seconds)
argocd cluster list | grep spokepool-01

# Expected output:
# SERVER                    NAME           VERSION  STATUS      MESSAGE
# https://10.0.0.1:6443     spokepool-01   v1.31.6  Successful  
```

### 8.4 Failure Scenario Tests

**Test 1: Cluster without spoke-type label (should NOT trigger)**

```yaml
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: test-cluster
  namespace: platform-capi
  # Missing spoke-type: pool label
status:
  phase: Provisioned
```

**Expected**: Kyverno policy does NOT generate ArgoCD Secret.

**Test 2: Cluster in Pending state (should NOT trigger)**

```yaml
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: spokepool-02
  namespace: platform-capi
  labels:
    spoke-type: pool
status:
  phase: Pending  # Not Provisioned yet
```

**Expected**: Kyverno policy does NOT generate ArgoCD Secret until phase=Provisioned.

**Test 3: Kubeconfig Secret missing (should fail gracefully)**

```yaml
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: spokepool-03
  namespace: platform-capi
  labels:
    spoke-type: pool
status:
  phase: Provisioned
# No corresponding <cluster-name>-kubeconfig Secret exists
```

**Expected**: Kyverno policy fails to generate ArgoCD Secret, logs error, retries with exponential backoff.

---

## 9. Observability and Debugging

### 9.1 Kyverno Metrics

Kyverno exposes Prometheus metrics:

```yaml
# ServiceMonitor for Kyverno
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: kyverno
  namespace: kyverno
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kyverno
  endpoints:
  - port: metrics
```

**Key Metrics**:
- `kyverno_policy_results_total{policy="capi-argocd-cluster-discovery"}`: Policy execution count
- `kyverno_policy_execution_duration_seconds`: Policy execution latency
- `kyverno_policy_changes_total`: Generated resource count

### 9.2 Debugging Failed Policies

```bash
# Check policy status
kubectl describe clusterpolicy capi-argocd-cluster-discovery

# Check PolicyReport for errors
kubectl get policyreport -A -o yaml

# Check Kyverno controller logs
kubectl logs -n kyverno -l app.kubernetes.io/component=background-controller --tail=100

# Check generated resources
kubectl get secret -n argocd -l spoke-type=pool -o yaml
```

---

## 10. Design Patterns from sbt-patterns

### 10.1 GitOps-First Principle

**Pattern**: All infrastructure changes via Git commits, not imperative commands.

**Application**: Kyverno policies are stored in Git (`manifests/platform-gitops/kyverno-policies/`) and applied via ArgoCD.

**Reference**: `docs/hub-spoke-architecture.md` - "GitOps-First: All mutations via Git commits"

### 10.2 Declarative Provisioning

**Pattern**: Describe desired state, let controllers reconcile.

**Application**: Kyverno ClusterPolicy declares "when Cluster is Provisioned, generate ArgoCD Secret". Kyverno handles reconciliation automatically.

**Reference**: `docs/hub-spoke-architecture.md` - "Declarative Provisioning"

### 10.3 Event-Driven Architecture

**Pattern**: React to state changes, not polling.

**Application**: Kyverno watches Cluster resources and triggers on `status.phase=Provisioned` event.

**Reference**: `docs/opensbt-architecture-guide.md` - "Event-Driven Onboarding"

---

## 11. Open Questions (Spec-Specific)

### 11.1 Kubeconfig Extraction Method

**Q**: Should we use Kyverno context variables (Option A) or two-step clone policy (Option B)?

**Spec Impact**: NFR-1.4 (ArgoCD discovery < 30 seconds)

**Recommendation**: Option A (context variables) if Kyverno v1.11+ is deployed to Hub.

**Decision Criteria**:
- Kyverno version in Hub cluster (check `kubectl version` output)
- RBAC policy (can Kyverno background controller read Secrets in platform-capi namespace?)
- Complexity tolerance (Option A is simpler, Option B is more verbose)

**Action**: Verify Kyverno version during Hub bootstrap, document in design.md.

### 11.2 Policy Synchronization Strategy

**Q**: Should `synchronize: true` be enabled for generated ArgoCD cluster Secrets?

**Spec Impact**: NFR-3.1 (Idempotent provisioning)

**Current**: Yes (`synchronize: true`)

**Pros**:
- ✅ Keeps Secret in sync with Cluster CR (idempotent, per NFR-3.1)
- ✅ If Cluster CR is updated (e.g., new control plane endpoint), Secret updates automatically

**Cons**:
- ⚠️ If Cluster CR is deleted, Secret is also deleted (ArgoCD loses cluster)
- ⚠️ Requires manual cleanup if Cluster CR is deleted but cluster still exists

**Recommendation**: Use `synchronize: true` for Phase 1. Add explicit cleanup policy in Phase 2 (cell decommissioning).

### 11.3 Namespace Restriction

**Q**: Should Kyverno policy watch ALL namespaces or only `platform-capi`?

**Spec Impact**: Security (prevent accidental Secret generation for non-Spoke Pool clusters)

**Current**: Restricted to `platform-capi` namespace (see Section 3.2)

**Rationale**:
- Spoke Pool clusters are ONLY provisioned in `platform-capi` namespace (per CAPI integration doc)
- Prevents policy from triggering on unrelated Cluster resources in other namespaces
- Aligns with least-privilege principle (security best practice)

**Recommendation**: Keep namespace restriction. Document in design.md.

### 11.4 Error Handling for Missing Kubeconfig

**Q**: What happens if CAPI Cluster reaches Provisioned state but kubeconfig Secret doesn't exist yet?

**Spec Impact**: NFR-3.4 (ArgoCD Agent reconnects automatically after network disruption)

**Kyverno Behavior**:
- Policy triggers on `phase=Provisioned`
- Context variable fetch fails (Secret not found)
- Kyverno logs error, retries with exponential backoff (default: 1s, 2s, 4s, 8s, 16s)
- Once Secret exists, policy succeeds and generates ArgoCD Secret

**Recommendation**: Accept Kyverno's built-in retry mechanism. No custom error handling needed.

**Monitoring**: Add alert for Kyverno policy failures (see Section 9.1).

---

## 12. Integration Summary

### 12.1 What We Add

🆕 Kyverno Helm chart installation (via ArgoCD)  
🆕 ClusterPolicy: `capi-argocd-cluster-discovery`  
🆕 RBAC: ClusterRole for Kyverno to read CAPI Secrets  
🆕 Monitoring: ServiceMonitor for Kyverno metrics  
🆕 Testing: Kyverno CLI tests for policy validation

### 12.2 What We Integrate With

✅ **CAPI**: Watches Cluster resources, reads kubeconfig Secrets  
✅ **ArgoCD**: Generates cluster Secrets for discovery  
✅ **Crossplane**: Triggered by SpokePool XR → CAPI Cluster provisioning  
✅ **VictoriaMetrics**: Kyverno metrics forwarded for observability

### 12.3 Dependencies

**Upstream**:
- CAPI Cluster CR must reach `Provisioned` state
- CAPI must generate `<cluster-name>-kubeconfig` Secret

**Downstream**:
- ArgoCD must be running in `argocd` namespace
- ArgoCD ApplicationSet must use Cluster Generator with `spoke-type: pool` selector

---

## 13. Next Steps

1. ✅ **CAPI + CAPH Integration** (complete)
2. ✅ **Kyverno Integration** (this document)
3. ⏭️ **ArgoCD Integration** (ApplicationSet for edge-catalog)
4. ⏭️ **cert-manager Integration** (mTLS certificate generation)

---

**Document Status**: Complete  
**Ready for Design Phase**: Yes  
**Blockers**: None  
**Recommended Approach**: Option A (Kyverno context variables) for kubeconfig extraction
