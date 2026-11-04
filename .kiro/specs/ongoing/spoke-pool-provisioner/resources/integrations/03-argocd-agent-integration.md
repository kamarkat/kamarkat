# ArgoCD Agent Integration Analysis

**Spec**: spoke-pool-provisioner  
**Component**: ArgoCD Agent (Hub-and-Spoke GitOps)  
**Purpose**: Automated edge catalog deployment to Spoke Pool clusters via managed mode agent  
**Created**: 2026-04-08

---

## 1. Integration Context

### 1.1 Position in Spoke Pool Provisioning Flow

```
CAPI Cluster Provisioned (FR-1.2)
         ↓
ClusterResourceSet Injection (Secret Zero)
         ↓
ArgoCD Agent Starts (< 2 min) ← THIS DOCUMENT
         ↓
Agent Connects to Hub (mTLS)
         ↓
ApplicationSet Deploys Edge Catalog (FR-2.1)
         ↓
Sync Waves Execute (Wave 0-4)
         ↓
Cell Ready for Tenant Onboarding
```

### 1.2 Spec Requirements Mapping

| Requirement | ArgoCD Agent Responsibility |
|-------------|----------------------------|
| **FR-1.2** | Agent deployment via ClusterResourceSet (5 resources: Deployment, ConfigMap, mTLS cert, CA, RBAC) |
| **FR-2.1** | Pull edge catalog Applications from Hub using Cluster Generator selector: `spoke-type: pool` |
| **AC-3** | Agent starts within 2 minutes, connects via mTLS, can pull Applications |
| **AC-4** | ApplicationSet with App-of-Apps pattern deploys edge catalog with sync waves |
| **NFR-3.4** | Agent reconnects automatically after network disruption |
| **NFR-4.1** | All Hub-Spoke communication uses mTLS authentication |

---

## 2. ArgoCD Agent Architecture

### 2.1 Hub-and-Spoke Model

**Problem Statement** (from `archived/argo/argocd-agent/docs/architecture.md`):
- Standard ArgoCD doesn't scale to 10,000+ clusters (Hub bottleneck)
- Spoke Pool requires autonomous operation during Hub unavailability
- Centralized observability needed for fleet management

**Solution**: ArgoCD Agent with pull-based architecture
- **Hub (Principal)**: Manages Application definitions, ApplicationSets, AppProjects
- **Spoke (Agent)**: Pulls Applications, reconciles locally, syncs status back to Hub
- **Managed Mode**: Hub controls all Application lifecycle (create/update/delete)

### 2.2 Agent Modes

**Managed Mode** (Spoke Pool uses this):
- Hub ArgoCD creates Applications
- Principal emits creation events
- Agent receives events, creates local Application CRs
- Local application-controller reconciles Applications
- Agent syncs status back to Principal
- Changes on Spoke are reverted (Hub is source of truth)

**Autonomous Mode** (NOT used for Spoke Pool):
- Agent creates Applications locally
- Agent syncs to Principal for observability only
- Spoke is source of truth

**Reference**: `archived/argo/argocd-agent/docs/concepts/agent-modes/managed.md`

---

## 3. ClusterResourceSet Injection (Secret Zero)

### 3.1 Bootstrap Resources

**FR-1.2 Requirement**: ClusterResourceSet contains exactly 5 resources

```yaml
apiVersion: addons.cluster.x-k8s.io/v1beta1
kind: ClusterResourceSet
metadata:
  name: argocd-agent-bootstrap
  namespace: platform-capi
spec:
  clusterSelector:
    matchLabels:
      spoke-type: pool  # Only Spoke Pool clusters
  resources:
    - name: argocd-agent-deployment    # 1. Agent Deployment (image: quay.io/argoproj-labs/argocd-agent:v0.1.0)
      kind: ConfigMap
    - name: argocd-agent-params        # 2. Agent ConfigMap
      kind: ConfigMap
    - name: argocd-agent-client-tls    # 3. mTLS Client Certificate
      kind: Secret
    - name: argocd-agent-ca            # 4. CA Certificate
      kind: Secret
    - name: argocd-agent-rbac          # 5. RBAC (SA, Role, RoleBinding, ClusterRole, ClusterRoleBinding)
      kind: ConfigMap
```

### 3.2 Agent Deployment Configuration

**Source**: `archived/argo/argocd-agent/install/kubernetes/agent/agent-deployment.yaml`

**Image**: `quay.io/argoproj-labs/argocd-agent:v0.1.0` (ArgoCD Agent Labs official release)

**Key Configuration** (from `agent-params-cm.yaml`):
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-agent-params
  namespace: argocd
data:
  # CRITICAL: Managed mode for Spoke Pool
  agent.mode: "managed"
  
  # Hub Principal connection
  agent.server.address: "argocd-agent-principal.platform-ops.svc.cluster.local"
  agent.server.port: "8443"
  
  # mTLS authentication (recommended)
  agent.creds: "mtls:^CN=(.+)$"
  agent.tls.secret-name: "argocd-agent-client-tls"
  agent.tls.root-ca-secret-name: "argocd-agent-ca"
  agent.tls.client.insecure: "false"
  
  # Namespace management
  agent.namespace: "argocd"
  
  # Redis connection (local to Spoke)
  agent.redis.address: "argocd-redis:6379"
  
  # Resource proxy (for live resources feature)
  agent.resource-proxy.enable: "true"
  
  # Namespace-based mapping (default for Spoke Pool)
  agent.destination-based-mapping: "false"
  agent.create-namespace: "false"
  agent.allowed-namespaces: ""
```

### 3.3 RBAC Requirements

**Source**: `archived/argo/argocd-agent/install/kubernetes/agent/agent-clusterrole.yaml`

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: argocd-agent-agent
rules:
- apiGroups: [""]
  resources: [namespaces]
  verbs: [create, get, list, watch]
- apiGroups: [argoproj.io]
  resources: [applications]
  verbs: [create, get, list, watch, update, delete, patch]
```

**Spec Alignment**: Agent has RBAC limited to its own cluster (NFR-4.4)

---

## 4. mTLS Certificate Management

### 4.1 Certificate Generation (Pre-Provisioning)

**Source**: `archived/argo/argocd-agent/docs/configuration/tls-certificates.md`

**Spoke Pool Requirement**: Certificates MUST be pre-generated before ClusterResourceSet injection

```bash
# Step 1: Initialize CA (one-time, Hub cluster)
argocd-agentctl pki init \
  --principal-context hub-cluster \
  --principal-namespace argocd

# Step 2: Issue Principal server certificate (one-time, Hub cluster)
argocd-agentctl pki issue principal \
  --principal-context hub-cluster \
  --ip "10.96.0.100" \
  --dns "argocd-agent-principal.platform-ops.svc.cluster.local" \
  --upsert

# Step 3: Issue Agent client certificate (per Spoke Pool cluster)
argocd-agentctl pki issue agent spokepool-01 \
  --principal-context hub-cluster \
  --agent-context spokepool-01 \
  --agent-namespace argocd \
  --upsert
```

**Integration with Crossplane Composition**:
1. Crossplane Composition triggers cert-manager Certificate CR
2. cert-manager generates client certificate signed by Hub CA
3. Certificate stored in Hub cluster Secret: `spokepool-01-agent-client-tls`
4. Crossplane patches certificate into ClusterResourceSet Secret
5. CAPI injects Secret into Spoke Pool cluster during bootstrap

### 4.2 Certificate Secrets Structure

**Agent Cluster Secrets** (injected via ClusterResourceSet):

```yaml
# Secret 1: Client Certificate
apiVersion: v1
kind: Secret
metadata:
  name: argocd-agent-client-tls
  namespace: argocd
type: kubernetes.io/tls
data:
  tls.crt: <base64-encoded-cert>
  tls.key: <base64-encoded-key>

# Secret 2: CA Certificate (public only)
apiVersion: v1
kind: Secret
metadata:
  name: argocd-agent-ca
  namespace: argocd
type: Opaque
data:
  ca.crt: <base64-encoded-ca-cert>
```

**Spec Alignment**:
- NFR-4.2: Certificates auto-rotate 7 days before expiration (cert-manager handles this)
- NFR-4.1: All Hub-Spoke communication uses mTLS

---

## 5. ApplicationSet for Edge Catalog Deployment

### 5.1 Cluster Generator with Selector

**Source**: `archived/argo/argocd-agent/docs/user-guide/applicationsets.md`

**FR-2.1 Requirement**: ApplicationSet uses Cluster Generator with selector: `spoke-type: pool`

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: spoke-pool-edge-catalog
  namespace: argocd
spec:
  generators:
    - clusters:
        selector:
          matchLabels:
            spoke-type: pool  # Only Spoke Pool clusters
  template:
    metadata:
      name: 'edge-catalog-{{name}}'
      labels:
        cell-id: '{{name}}'
    spec:
      project: platform-infrastructure
      source:
        repoURL: https://github.com/kamarkat/kamarkat
        path: edge-catalog/spoke-pool
        targetRevision: main
      destination:
        name: '{{name}}'  # Cluster name from generator
        namespace: argocd
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
```

**How It Works**:
1. Kyverno creates ArgoCD cluster Secret with label `spoke-type: pool` (FR-1.3)
2. ArgoCD discovers cluster within 30 seconds (NFR-1.4)
3. Cluster Generator matches selector, creates Application
4. Principal emits Application creation event
5. Agent receives event, creates local Application CR
6. Local application-controller reconciles Application
7. Agent syncs status back to Principal

### 5.2 App-of-Apps Pattern

**FR-2.1 Requirement**: App-of-Apps umbrella Application creates individual Applications

**Umbrella Application** (`edge-catalog/spoke-pool/app-of-apps.yaml`):
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: edge-catalog-umbrella
  namespace: argocd
spec:
  project: platform-infrastructure
  source:
    repoURL: https://github.com/kamarkat/kamarkat
    path: edge-catalog/spoke-pool/apps
    targetRevision: main
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

**Individual Applications** (`edge-catalog/spoke-pool/apps/`):
- `shared-cnpg.yaml` (Wave 1)
- `atlas-operator.yaml` (Wave 2)
- `postgrest.yaml` (Wave 3)
- `nats-leaf-node.yaml` (Wave 4)
- `spire-agent.yaml` (Wave 4)
- `grafana-alloy.yaml` (Wave 4)

---

## 6. Sync Waves and Dependency Ordering

### 6.1 Wave Configuration

**FR-2.1 Requirement**: Sync waves enforce dependency ordering (Wave 0-4)

```yaml
# Wave 0: Database Extensions (if needed)
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cnpg-extensions
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  # pgvector, pg_stat_statements, etc.

# Wave 1: CNPG Cluster + PgBouncer
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: shared-cnpg
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  source:
    path: edge-catalog/spoke-pool/cnpg
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true

# Wave 2: Atlas Operator + AtlasMigration CRs
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: atlas-operator
  annotations:
    argocd.argoproj.io/sync-wave: "2"
spec:
  source:
    path: edge-catalog/spoke-pool/atlas

# Wave 3: PostgREST (requires schemas to exist)
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: postgrest
  annotations:
    argocd.argoproj.io/sync-wave: "3"
spec:
  source:
    path: edge-catalog/spoke-pool/postgrest

# Wave 4: Tenant Workloads + Observability
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: nats-leaf-node
  annotations:
    argocd.argoproj.io/sync-wave: "4"
spec:
  source:
    path: edge-catalog/spoke-pool/nats
```

### 6.2 Health Checks

**FR-2.1 Requirement**: Health checks gate progression

**CNPG Health Check** (Wave 1 → Wave 2):
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: shared-cnpg
spec:
  ignoreDifferences:
    - group: postgresql.cnpg.io
      kind: Cluster
      jsonPointers:
        - /status
  # ArgoCD waits for CNPG status.phase=Ready before Wave 2
```

**Atlas Operator Health Check** (Wave 2 → Wave 3):
```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: atlas-operator
spec:
  # ArgoCD waits for AtlasMigration CR status.conditions[Ready=True] before Wave 3
```

---

## 7. Agent Connection and Status Sync

### 7.1 Agent Startup Sequence

**AC-3 Requirement**: Agent starts within 2 minutes, connects via mTLS

```
1. CAPI Cluster reaches Ready state
2. ClusterResourceSet injects 5 resources
3. Agent Deployment starts (image pull + pod start)
4. Agent reads ConfigMap: agent.server.address, agent.mode=managed
5. Agent loads mTLS certificates from Secrets
6. Agent connects to Principal: argocd-agent-principal.platform-ops.svc:8443
7. Principal validates client certificate CN matches agent name
8. Principal creates queue pair for agent
9. Agent subscribes to Application events
10. Agent sends initial status sync
```

**Expected Logs** (from `archived/argo/argocd-agent/docs/user-guide/adding-agents.md`):
```
INFO[0001] Starting argocd-agent (agent) v0.1.0 (ns=argocd, mode=managed, auth=mtls)
INFO[0002] Authentication successful
INFO[0003] Connected to argocd-agent-principal v0.1.0
```

### 7.2 Status Synchronization

**Managed Mode Behavior**:
- Agent watches local Application CRs
- Agent detects status field changes (sync status, health status, operation state)
- Agent transmits status updates to Principal via gRPC
- Principal merges status into leading copy of Application
- Hub ArgoCD UI shows real-time status from all Spoke Pool clusters

**NFR-3.4 Requirement**: Agent reconnects automatically after network disruption
- Agent implements exponential backoff: 1s, 2s, 4s, 8s, 16s
- Agent maintains local Application state during disconnection
- Agent resumes status sync after reconnection

---

## 8. Namespace-Based vs Destination-Based Mapping

### 8.1 Spoke Pool Uses Namespace-Based Mapping

**Configuration**: `agent.destination-based-mapping: "false"` (default)

**How It Works**:
- Hub creates Application in namespace matching agent name: `namespace: spokepool-01`
- Agent watches Applications in its own namespace only
- Agent creates local Application CR in `argocd` namespace
- Local application-controller reconciles Application

**Limitation**: Each ApplicationSet targets a single agent (or uses Cluster Generator)

### 8.2 Destination-Based Mapping (NOT used for Spoke Pool)

**Configuration**: `agent.destination-based-mapping: "true"`

**How It Works**:
- Hub creates Application with `spec.destination.name: spokepool-01`
- Principal routes Application to agent based on destination.name
- Allows single ApplicationSet to target multiple agents

**Why NOT used**: Spoke Pool uses Cluster Generator, which works with namespace-based mapping

**Reference**: `archived/argo/argocd-agent/docs/concepts/agent-mapping.md`

---

## 9. Implementation Checklist

### 9.1 Hub Cluster Setup (One-Time)

- [ ] Deploy ArgoCD Principal to Hub cluster
- [ ] Initialize PKI: `argocd-agentctl pki init`
- [ ] Issue Principal server certificate
- [ ] Issue Resource Proxy certificate
- [ ] Create JWT signing key
- [ ] Deploy ApplicationSet for Spoke Pool edge catalog
- [ ] Create AppProject: `platform-infrastructure`

### 9.2 Per-Cluster Setup (Automated via Crossplane)

- [ ] Generate agent client certificate via cert-manager
- [ ] Create ClusterResourceSet with 5 resources
- [ ] Patch mTLS certificate into ClusterResourceSet Secret
- [ ] Bind ClusterResourceSet to CAPI Cluster (label: `spoke-type: pool`)
- [ ] Wait for CAPI Cluster Ready
- [ ] Verify Agent Deployment starts within 2 minutes
- [ ] Verify Agent connects to Principal (check logs)
- [ ] Verify Kyverno creates ArgoCD cluster Secret
- [ ] Verify ApplicationSet creates edge catalog Application
- [ ] Verify edge catalog components reach Healthy status

### 9.3 Verification Commands

```bash
# Check Agent status
kubectl --context spokepool-01 get pods -n argocd -l app.kubernetes.io/name=argocd-agent-agent

# Check Agent logs
kubectl --context spokepool-01 logs -n argocd deployment/argocd-agent-agent

# Check Principal logs (Hub cluster)
kubectl --context hub-cluster logs -n argocd deployment/argocd-agent-principal | grep spokepool-01

# Check ArgoCD cluster Secret
kubectl --context hub-cluster get secret -n argocd -l cell-id=spokepool-01

# Check ApplicationSet generated Applications
argocd app list | grep spokepool-01

# Check edge catalog sync status
argocd app get edge-catalog-spokepool-01
```

---

## 10. Troubleshooting Guide

### 10.1 Agent Fails to Start

**Symptom**: Agent pod CrashLoopBackOff

**Checks**:
```bash
# Verify Secrets exist
kubectl --context spokepool-01 get secrets -n argocd | grep argocd-agent

# Verify ConfigMap exists
kubectl --context spokepool-01 get cm -n argocd argocd-agent-params

# Check pod events
kubectl --context spokepool-01 describe pod -n argocd -l app.kubernetes.io/name=argocd-agent-agent
```

**Common Causes**:
- Missing mTLS certificate Secret
- Missing CA certificate Secret
- Invalid ConfigMap configuration

### 10.2 Agent Fails to Connect

**Symptom**: Agent logs show "connection refused" or "certificate validation failed"

**Checks**:
```bash
# Verify Principal service is reachable
kubectl --context spokepool-01 exec -it deployment/argocd-agent-agent -n argocd -- \
  nc -zv argocd-agent-principal.platform-ops.svc.cluster.local 8443

# Verify certificate CN matches agent name
kubectl --context spokepool-01 get secret argocd-agent-client-tls -n argocd -o yaml | \
  base64 -d | openssl x509 -text -noout | grep "Subject: CN"

# Verify CA certificate is valid
kubectl --context spokepool-01 get secret argocd-agent-ca -n argocd -o yaml | \
  base64 -d | openssl x509 -text -noout
```

**Common Causes**:
- Network policy blocking Hub-Spoke communication
- Certificate CN mismatch (must match cluster name)
- CA certificate mismatch (Spoke CA != Hub CA)

### 10.3 ApplicationSet Not Creating Applications

**Symptom**: No Applications created for Spoke Pool cluster

**Checks**:
```bash
# Verify ArgoCD cluster Secret exists
kubectl --context hub-cluster get secret -n argocd -l spoke-type=pool

# Verify cluster Secret has correct labels
kubectl --context hub-cluster get secret cluster-spokepool-01 -n argocd -o yaml | grep -A5 labels

# Check ApplicationSet status
kubectl --context hub-cluster get appset spoke-pool-edge-catalog -n argocd -o yaml

# Check ApplicationSet controller logs
kubectl --context hub-cluster logs -n argocd deployment/argocd-applicationset-controller
```

**Common Causes**:
- Kyverno policy failed to create cluster Secret
- Cluster Secret missing label: `spoke-type: pool`
- ApplicationSet selector mismatch

---

## 11. Design Patterns Alignment

**Source**: sbt-patterns MCP

### 11.1 GitOps-First Pattern
- All edge catalog components deployed via ArgoCD
- No manual `kubectl apply` allowed
- Git is single source of truth

### 11.2 Declarative Provisioning Pattern
- SpokePool XR triggers entire provisioning flow
- No imperative scripts or manual steps
- Idempotent operations (re-applying XR has no side effects)

### 11.3 Event-Driven Pattern
- CAPI emits Cluster Ready event
- Kyverno watches event, generates ArgoCD Secret
- ArgoCD discovers cluster, triggers ApplicationSet
- Agent receives Application events, reconciles locally

---

## 12. Acceptance Criteria Validation

| AC | Requirement | ArgoCD Agent Implementation |
|----|-------------|----------------------------|
| **AC-3** | ClusterResourceSet contains 5 resources | ✅ Deployment, ConfigMap, mTLS cert, CA, RBAC |
| **AC-3** | Agent starts within 2 minutes | ✅ Lightweight container, fast startup |
| **AC-3** | Agent connects via mTLS | ✅ Pre-generated certificates, CN validation |
| **AC-3** | Agent can pull Applications | ✅ Managed mode, Principal emits events |
| **AC-4** | ApplicationSet with Cluster Generator | ✅ Selector: `spoke-type: pool` |
| **AC-4** | App-of-Apps pattern | ✅ Umbrella Application creates children |
| **AC-4** | Sync waves enforce ordering | ✅ Wave 0-4 annotations |
| **AC-4** | Health checks gate progression | ✅ CNPG Ready before Wave 2 |

---

**Document Version**: 1.0  
**Last Updated**: 2026-04-08  
**Next Steps**: Proceed to Atlas Operator integration analysis (FR-4.4, FR-5.2)
