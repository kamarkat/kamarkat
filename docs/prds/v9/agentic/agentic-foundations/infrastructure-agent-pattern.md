# Infrastructure Agent Pattern
## Component-Centric Agent & Tool Co-location

**Purpose**: Define GitOps pattern for deploying infrastructure components with their agents and MCP tool servers  
**Audience**: Platform engineering team  
**Status**: DRAFT

---

## Core Principle

**Every infrastructure component is deployed with its agent and MCP tool server as a single atomic unit.**

Components, tools, and agents are co-located in the same manifest folder and synced together by ArgoCD.

---

## Folder Structure Pattern

```
manifests/{component-name}/
├── {component}-deployment.yaml      # Core component (DB, service, etc.)
├── {component}-toolserver.yaml      # MCP server deployment
├── {component}-agent.yaml           # Kagent Agent CR
└── tools/                           # Tool implementations
    ├── tool_1.go
    ├── tool_2.go
    └── tool_3.go
```

---

## Example: Platform Database (CNPG)

### Directory Structure
```
manifests/hub-core-services/platform-database/
├── cnpg-cluster.yaml              # PostgreSQL cluster
├── cnpg-toolserver.yaml           # MCP server for CNPG operations
├── cnpg-agent.yaml                # Kagent Agent CR
└── tools/
    ├── get_cluster_status.go      # Tool: Query cluster health
    ├── list_backups.go            # Tool: List available backups
    ├── check_replication.go       # Tool: Verify replication status
    └── trigger_backup.go          # Tool: Initiate backup
```

### 1. Component Deployment
**File**: `manifests/hub-core-services/platform-database/cnpg-cluster.yaml`

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: platform-db
  namespace: platform-database
spec:
  instances: 3
  storage:
    size: 100Gi
  backup:
    barmanObjectStore:
      destinationPath: s3://backups/platform-db
```

### 2. MCP Tool Server
**File**: `manifests/hub-core-services/platform-database/cnpg-toolserver.yaml`

```yaml
apiVersion: kagent.dev/v1alpha2
kind: ToolServer
metadata:
  name: cnpg-mcp-server
  namespace: platform-database
spec:
  type: remote
  remote:
    url: http://cnpg-toolserver:8080
    protocol: sse
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cnpg-toolserver
  namespace: platform-database
spec:
  replicas: 2
  selector:
    matchLabels:
      app: cnpg-toolserver
  template:
    metadata:
      labels:
        app: cnpg-toolserver
    spec:
      serviceAccountName: cnpg-toolserver
      containers:
        - name: mcp-server
          image: zero-ops/cnpg-mcp-server:v1.0.0
          ports:
            - containerPort: 8080
              name: mcp
          env:
            - name: CNPG_NAMESPACE
              value: platform-database
            - name: CNPG_CLUSTER_NAME
              value: platform-db
---
apiVersion: v1
kind: Service
metadata:
  name: cnpg-toolserver
  namespace: platform-database
spec:
  selector:
    app: cnpg-toolserver
  ports:
    - port: 8080
      targetPort: 8080
      name: mcp
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cnpg-toolserver
  namespace: platform-database
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cnpg-toolserver
  namespace: platform-database
rules:
  - apiGroups: ["postgresql.cnpg.io"]
    resources: ["clusters", "backups"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cnpg-toolserver
  namespace: platform-database
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cnpg-toolserver
subjects:
  - kind: ServiceAccount
    name: cnpg-toolserver
    namespace: platform-database
```

### 3. Kagent Agent CR
**File**: `manifests/hub-core-services/platform-database/cnpg-agent.yaml`

```yaml
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: cnpg-ops-agent
  namespace: platform-database
  labels:
    component: platform-database
    agent-type: infrastructure
spec:
  type: Declarative
  declarative:
    systemMessage: |
      You are a PostgreSQL cluster operations expert for CloudNativePG.
      
      Your responsibilities:
      - Query cluster health and status
      - List and verify backups
      - Check replication lag
      - Analyze performance metrics
      
      Always provide clear, actionable information about the database state.
    
    modelConfig:
      name: gpt-4-turbo
    
    tools:
      - type: McpServer
        mcpServer:
          name: cnpg-mcp-server
          toolNames:
            - get_cluster_status
            - list_backups
            - check_replication
            - get_pod_status
            - analyze_logs
    
    memory:
      type: kagent
      config:
        namespace: cnpg-ops-agent
```

### 4. Tool Implementations
**File**: `manifests/hub-core-services/platform-database/tools/get_cluster_status.go`

```go
package tools

import (
    "context"
    "encoding/json"
    
    cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

type GetClusterStatusInput struct {
    ClusterName string `json:"cluster_name"`
    Namespace   string `json:"namespace"`
}

type ClusterStatusOutput struct {
    Status      string `json:"status"`
    Instances   int    `json:"instances"`
    ReadyPods   int    `json:"ready_pods"`
    Primary     string `json:"primary"`
    Replication string `json:"replication_status"`
}

func GetClusterStatus(ctx context.Context, k8sClient client.Client, input GetClusterStatusInput) (*ClusterStatusOutput, error) {
    cluster := &cnpgv1.Cluster{}
    err := k8sClient.Get(ctx, client.ObjectKey{
        Name:      input.ClusterName,
        Namespace: input.Namespace,
    }, cluster)
    if err != nil {
        return nil, err
    }
    
    return &ClusterStatusOutput{
        Status:      string(cluster.Status.Phase),
        Instances:   cluster.Spec.Instances,
        ReadyPods:   cluster.Status.ReadyInstances,
        Primary:     cluster.Status.CurrentPrimary,
        Replication: cluster.Status.ReplicationStatus,
    }, nil
}
```

**File**: `manifests/hub-core-services/platform-database/tools/list_backups.go`

```go
package tools

import (
    "context"
    
    cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

type ListBackupsInput struct {
    ClusterName string `json:"cluster_name"`
    Namespace   string `json:"namespace"`
}

type BackupInfo struct {
    Name        string `json:"name"`
    Status      string `json:"status"`
    StartTime   string `json:"start_time"`
    CompletedAt string `json:"completed_at"`
    Size        string `json:"size"`
}

type ListBackupsOutput struct {
    Backups []BackupInfo `json:"backups"`
}

func ListBackups(ctx context.Context, k8sClient client.Client, input ListBackupsInput) (*ListBackupsOutput, error) {
    backupList := &cnpgv1.BackupList{}
    err := k8sClient.List(ctx, backupList, client.InNamespace(input.Namespace))
    if err != nil {
        return nil, err
    }
    
    backups := make([]BackupInfo, 0, len(backupList.Items))
    for _, backup := range backupList.Items {
        backups = append(backups, BackupInfo{
            Name:        backup.Name,
            Status:      string(backup.Status.Phase),
            StartTime:   backup.Status.StartedAt.String(),
            CompletedAt: backup.Status.StoppedAt.String(),
            Size:        backup.Status.BackupSize,
        })
    }
    
    return &ListBackupsOutput{Backups: backups}, nil
}
```

---

## Example: Platform Identity (Ory Kratos)

### Directory Structure
```
manifests/hub-core-services/platform-identity/ory-kratos/
├── kratos-deployment.yaml         # Kratos service
├── kratos-toolserver.yaml         # MCP server for identity ops
├── kratos-agent.yaml              # Kagent Agent CR
└── tools/
    ├── list_users.go              # Tool: List identities
    ├── get_identity.go            # Tool: Get user details
    ├── verify_session.go          # Tool: Check session validity
    └── list_sessions.go           # Tool: List active sessions
```

### Kratos Agent CR
**File**: `manifests/hub-core-services/platform-identity/ory-kratos/kratos-agent.yaml`

```yaml
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: kratos-ops-agent
  namespace: platform-identity
spec:
  type: Declarative
  declarative:
    systemMessage: |
      You are an identity management expert for Ory Kratos.
      
      Your responsibilities:
      - Query user identities and sessions
      - Verify authentication status
      - Analyze login patterns
      - Troubleshoot identity issues
    
    modelConfig:
      name: gpt-4-turbo
    
    tools:
      - type: McpServer
        mcpServer:
          name: kratos-mcp-server
          toolNames:
            - list_users
            - get_identity
            - verify_session
            - list_sessions
            - get_login_flows
```

---

## Example: API Gateway (AgentGateway)

### Directory Structure
```
manifests/api-gateway/
├── agentgateway.yaml              # Gateway deployment
├── agentgateway-toolserver.yaml   # MCP server for gateway ops
├── agentgateway-agent.yaml        # Kagent Agent CR
└── tools/
    ├── get_route_stats.go         # Tool: Route metrics
    ├── check_auth_status.go       # Tool: Auth validation status
    ├── list_active_sessions.go    # Tool: Active A2A sessions
    └── analyze_traffic.go         # Tool: Traffic patterns
```

### AgentGateway Agent CR
**File**: `manifests/api-gateway/agentgateway-agent.yaml`

```yaml
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: agentgateway-ops-agent
  namespace: api-gateway
spec:
  type: Declarative
  declarative:
    systemMessage: |
      You are an API gateway operations expert for AgentGateway.
      
      Your responsibilities:
      - Monitor gateway health and performance
      - Analyze routing and traffic patterns
      - Troubleshoot authentication issues
      - Verify A2A protocol connectivity
    
    modelConfig:
      name: gpt-4-turbo
    
    tools:
      - type: McpServer
        mcpServer:
          name: agentgateway-mcp-server
          toolNames:
            - get_route_stats
            - check_auth_status
            - list_active_sessions
            - analyze_traffic
            - get_error_logs
```

---

## GitOps Integration

### ArgoCD Application Pattern

**Each component folder becomes one ArgoCD Application:**

```yaml
# manifests/argocd/apps/platform-database.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: platform-database
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/kamarkat/kamarkat
    targetRevision: HEAD
    path: manifests/hub-core-services/platform-database  # Syncs ALL: component + toolserver + agent
  destination:
    server: https://kubernetes.default.svc
    namespace: platform-database
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

**Result**: ArgoCD syncs component, MCP tool server, and agent together as atomic unit.

---

## Component Inventory

### Required Infrastructure Agents

| Component | Folder | Agent Name | Tool Server | Tools |
|-----------|--------|------------|-------------|-------|
| **CNPG** | `manifests/hub-core-services/platform-database/` | `cnpg-ops-agent` | `cnpg-mcp-server` | get_cluster_status, list_backups, check_replication |
| **Ory Kratos** | `manifests/hub-core-services/platform-identity/ory-kratos/` | `kratos-ops-agent` | `kratos-mcp-server` | list_users, get_identity, verify_session |
| **Ory Keto** | `manifests/hub-core-services/platform-identity/ory-keto/` | `keto-ops-agent` | `keto-mcp-server` | check_permission, list_relations |
| **Ory Hydra** | `manifests/hub-core-services/platform-identity/ory-hydra/` | `hydra-ops-agent` | `hydra-mcp-server` | list_clients, get_token_info |
| **AgentGateway** | `manifests/api-gateway/` | `agentgateway-ops-agent` | `agentgateway-mcp-server` | get_route_stats, check_auth_status |
| **ArgoCD** | `manifests/argocd/` | `argocd-ops-agent` | `argocd-mcp-server` | get_app_status, sync_app, list_apps |
| **VictoriaMetrics** | `manifests/observability/victoriametrics/` | `victoriametrics-ops-agent` | `victoriametrics-mcp-server` | query_metrics, list_targets |
| **ClickHouse** | `manifests/observability/clickhouse/` | `clickhouse-ops-agent` | `clickhouse-mcp-server` | run_query, get_table_stats |
| **Tempo** | `manifests/observability/tempo/` | `tempo-ops-agent` | `tempo-mcp-server` | query_traces, get_trace_by_id |

---

## Tool Server Implementation Pattern

### MCP Server Structure

```
tools/cnpg-mcp-server/
├── main.go                    # MCP server entrypoint
├── handlers/
│   ├── get_cluster_status.go
│   ├── list_backups.go
│   └── check_replication.go
├── k8s/
│   └── client.go              # Kubernetes client wrapper
├── Dockerfile
└── go.mod
```

### MCP Server Main
**File**: `tools/cnpg-mcp-server/main.go`

```go
package main

import (
    "context"
    "log"
    "net/http"
    
    "github.com/modelcontextprotocol/go-sdk/server"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
    // Initialize Kubernetes client
    cfg, err := config.GetConfig()
    if err != nil {
        log.Fatal(err)
    }
    
    k8sClient, err := client.New(cfg, client.Options{})
    if err != nil {
        log.Fatal(err)
    }
    
    // Create MCP server
    mcpServer := server.NewMCPServer("cnpg-mcp-server", "1.0.0")
    
    // Register tools
    mcpServer.AddTool("get_cluster_status", handlers.GetClusterStatus(k8sClient))
    mcpServer.AddTool("list_backups", handlers.ListBackups(k8sClient))
    mcpServer.AddTool("check_replication", handlers.CheckReplication(k8sClient))
    
    // Start HTTP server
    http.Handle("/", mcpServer)
    log.Println("CNPG MCP Server listening on :8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

---

## Deployment Workflow

### 1. Component Development
```bash
# Create component folder
mkdir -p manifests/hub-core-services/platform-database/tools

# Add component manifest
vim manifests/hub-core-services/platform-database/cnpg-cluster.yaml

# Implement MCP tool server
cd tools/cnpg-mcp-server
go mod init github.com/kamarkat/kamarkat/tools/cnpg-mcp-server
# ... implement tools

# Build and push image
docker build -t zero-ops/cnpg-mcp-server:v1.0.0 .
docker push zero-ops/cnpg-mcp-server:v1.0.0
```

### 2. Add Tool Server Manifest
```bash
vim manifests/hub-core-services/platform-database/cnpg-toolserver.yaml
# Add ToolServer CR + Deployment + Service + RBAC
```

### 3. Add Agent CR
```bash
vim manifests/hub-core-services/platform-database/cnpg-agent.yaml
# Define Kagent Agent with tools reference
```

### 4. Create ArgoCD Application
```bash
vim manifests/argocd/apps/platform-database.yaml
# Point to manifests/hub-core-services/platform-database/ folder
```

### 5. Commit and Sync
```bash
git add manifests/hub-core-services/platform-database/
git add manifests/argocd/apps/platform-database.yaml
git commit -m "Add CNPG component with agent and tools"
git push

# ArgoCD auto-syncs: component + toolserver + agent deployed together
```

---

## Key Benefits

1. **Atomic Deployment**: Component, tools, and agent deployed together
2. **Co-location**: All related resources in one folder
3. **GitOps Native**: ArgoCD syncs entire component as unit
4. **Discoverability**: Easy to find agent for any component
5. **Consistency**: Same pattern across all infrastructure components
6. **Rollback Safety**: Rollback component + agent together

---

## Anti-Patterns to Avoid

❌ **Separate agent folders**:
```
manifests/agents/cnpg-agent.yaml        # DON'T: Separated from component
manifests/hub-core-services/platform-database/cnpg.yaml
```

❌ **Shared tool servers**:
```
manifests/shared-toolserver/            # DON'T: One server for all components
```

❌ **Manual agent creation**:
```
kubectl apply -f agent.yaml             # DON'T: Bypass GitOps
```

✅ **Correct pattern**:
```
manifests/hub-core-services/platform-database/
├── cnpg-cluster.yaml
├── cnpg-toolserver.yaml
├── cnpg-agent.yaml
└── tools/
```

---

## Implementation Checklist

- [ ] Create component folder structure
- [ ] Implement MCP tool server (Go)
- [ ] Build and push tool server image
- [ ] Add ToolServer CR + Deployment manifest
- [ ] Define Kagent Agent CR with tool references
- [ ] Create ArgoCD Application pointing to folder
- [ ] Commit to Git
- [ ] Verify ArgoCD sync
- [ ] Test agent via A2A protocol
- [ ] Document component-specific tools

---

## Next Steps

1. Implement CNPG MCP tool server (reference implementation)
2. Create template for new component agents
3. Build tool server base image with common utilities
4. Define RBAC patterns for tool servers
5. Create testing framework for MCP tools
