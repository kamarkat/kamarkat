Here is the comprehensive, updated **Agents-Core Implementation Pattern Guide**. 

This guide integrates the updated Monorepo structure, aligns strictly with your **Kagent Lifecycle** and **Hub-Spoke** diagrams, enforces the **Dual Provisioning Path** (GitOps for platform, Direct API for business agents), and critically, includes the **Telemetry (Metrics, Tracing, Logging)** and **Cluster Deployment** patterns required to make this testable and production-ready in your cluster.

---

# Agents-Core Implementation Pattern Guide (v9.0 Aligned)

## 1. Architectural Positioning & Boundaries
* **`open-sbt`**: The generic SaaS framework (Auth, Billing, NATS, multi-tenant primitives). Contains zero agent logic.
* **`agents-core`**: Your Platform Service handling business domain logic. Exposes the MCP API, enforces tool/model authorization, and orchestrates the `AgentRegistry`.
* **Business Agents (Data Plane):** Deployed dynamically by tenants via MCP. Provisioned instantly using a **Deployment Adapter** that applies the Kagent CRD directly to the Spoke Kubernetes API. (Bypasses GitOps).
* **Telemetry & Observability:** `agents-core` MUST instrument all MCP tools and deployment actions using Prometheus (metrics), OpenTelemetry (tracing), and OpenSearch (logging) via the `opensbt/libraries` packages, otherwise cluster-level testing and debugging of the Hub-Spoke flow is impossible.

---

## 2. Updated Project Structure (Monorepo)
This structure reflects the integration of the Deployment Adapter, Telemetry, and Kubernetes Deployment manifests required to run `agents-core` in the cluster.

```text
zero-ops/
├── cmd/
│   ├── mcp-server/               # Single binary hosting agents-core MCP tools
│   ├── nats-subscriber/          # Daemon closing the Hub DB -> AgentRegistry status loop
│   └── spoke-controller/         # Deployed to Spokes to watch Agent CRDs
│
├── internal/
│   ├── opensbt/                  # 🔒 STRICT BOUNDARY: Generic SaaS framework only
│   │   ├── interfaces/           
│   │   ├── libraries/            
│   │   │   ├── tracing/          # OTel wrappers
│   │   │   ├── metrics/          # Prometheus wrappers
│   │   │   └── logging/          # Structured logging -> OpenSearch
│   │   └── providers/            
│   │
│   ├── agent-core/               # ✅ YOUR BUSINESS DOMAIN
│   │   ├── mcp/                  # MCP Tool Handlers (create_agent, deploy_agent)
│   │   ├── service/              # Orchestration & Validation
│   │   ├── adapter/              # ★ NEW: Direct K8s Spoke Deployment Adapter
│   │   ├── telemetry/            # ★ NEW: Custom metrics, traces, and log fields
│   │   ├── client/               # External integrations (AgentRegistry OSS, Hub)
│   │   └── database/             # Platform-specific SQL & schema (authorized_tools)
│
├── operators/
│   └── spoke-controller/         # Reconciles crossplane claims & Agent CRD status
│
└── manifests/                    # ★ NEW: Cluster deployment definitions
    └── platform-core/
        ├── mcp-server.yaml       # Deployment, Service, RBAC for Hub
        ├── nats-subscriber.yaml  # Deployment for NATS status syncing
        └── rbac-spoke-access.yaml# Secrets/Kubeconfigs for Hub -> Spoke CRD application
```

---

## 3. The `deploy_agent` Flow & The Deployment Adapter
As defined in your `Kagent Lifecycle` diagram, Business Agents use a **Deployment Adapter** to apply the Kagent CRD directly to the Spoke cluster.

### 3.1 The MCP Orchestrator (`internal/agent-core/service/deployment_service.go`)
```go
func (s *DeploymentService) DeployAgent(ctx context.Context, params map[string]interface{}) (interface{}, error) {
    // 1. Telemetry & Context
    ctx, span := s.tracer.StartSpan(ctx, "DeployAgent")
    defer span.End()
    
    tenantID := ctx.Value("tenant_id").(string)
    spokeClusterID := ctx.Value("spoke_cluster_id").(string)
    agentID := params["agent_id"].(string)

    // 2. Register intent in AgentRegistry DB (Status: "deploying")
    deployResp, err := s.agentRegistryClient.CreateDeployment(ctx, req)
    if err != nil {
        s.metrics.RecordError(ctx, "agentregistry_api_failure")
        return nil, err
    }

    // 3. Materialize Kagent CRD
    agentCRD := s.generateKagentCRD(agentID, tenantID, deployResp.ID)

    // 4. Deployment Adapter: Apply DIRECTLY to Spoke Cluster API
    err = s.k8sSpokeAdapter.ApplyCRD(ctx, spokeClusterID, agentCRD)
    if err != nil {
        s.metrics.RecordError(ctx, "spoke_apply_failure")
        return nil, fmt.Errorf("failed to apply CRD to spoke: %w", err)
    }

    // 5. Publish Audit/Billing event to NATS (Fire & Forget)
    s.eventBus.PublishAsync(ctx, models.NewEvent(
        "zeroops_agentDeployed", // Platform domain event
        models.PlatformEventSource,
        map[string]interface{}{
            "tenant_id": tenantID,
            "agent_id":  agentID,
        },
    ))

    // 6. Return immediately (Client polls for 'Ready' status)
    return map[string]interface{}{
        "status":        "deploying",
        "deployment_id": deployResp.ID,
        "message":       "CRD applied to Spoke. Poll get_agent_status.",
    }, nil
}
```

### 3.2 The Deployment Adapter (`internal/agent-core/adapter/k8s_spoke_adapter.go`)
The Hub must maintain secure connections (via `kubeconfig` secrets managed by Cluster API) to the Spoke clusters to apply CRDs.

```go
type K8sSpokeAdapter struct {
    // A cache of client-go dynamic clients keyed by spokeClusterID
    spokeClients map[string]dynamic.Interface 
}

func (a *K8sSpokeAdapter) ApplyCRD(ctx context.Context, spokeClusterID string, crdYAML []byte) error {
    client, err := a.getSpokeClient(spokeClusterID)
    if err != nil {
        return err
    }
    
    // Parse YAML to Unstructured
    obj := &unstructured.Unstructured{}
    err = yaml.Unmarshal(crdYAML, &obj.Object)
    
    // Apply using Server-Side Apply (SSA) for idempotency
    gvr := schema.GroupVersionResource{Group: "kagent.dev", Version: "v1alpha2", Resource: "agents"}
    _, err = client.Resource(gvr).Namespace(obj.GetNamespace()).Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
        FieldManager: "mcp-server-adapter",
        Force:        true,
    })
    return err
}
```

---

## 4. Telemetry Implementation (Metrics, Tracing, Logging)
Testing the Hub-Spoke architecture is impossible without distributed tracing and metrics. `agents-core` MUST utilize the `open-sbt` telemetry libraries.

### 4.1 Prometheus Metrics (`internal/agent-core/telemetry/metrics.go`)
Create custom business metrics to track agent deployments across the fleet.

```go
import "github.com/kamarkat/kamarkat/internal/opensbt/libraries/metrics"

type AgentMetrics struct {
    Manager *metrics.Manager
    AgentsDeployed *prometheus.CounterVec
    DeployDuration *prometheus.HistogramVec
}

func InitAgentMetrics(m *metrics.Manager) *AgentMetrics {
    return &AgentMetrics{
        Manager: m,
        AgentsDeployed: m.RegisterGauge(prometheus.DefaultRegisterer, "zeroops", "agents_deployed_total", "Total agents deployed to spokes"),
    }
}
```

### 4.2 OpenTelemetry Tracing (`cmd/mcp-server/main.go`)
When the `mcp-server` boots, it must initialize the OpenTelemetry provider and wrap the MCP handler. This ensures traces propagate from the MCP client -> Hub -> AgentRegistry -> Spoke.

```go
import "github.com/kamarkat/kamarkat/internal/opensbt/libraries/tracing"

func main() {
    // 1. Initialize OTel Tracer (pointing to Hub's Tempo/Jaeger)
    tracer, _ := tracing.New(ctx, "mcp-server", "http://tempo.zero-ops-system:4318")
    defer tracer.Shutdown(ctx)

    // 2. Wrap MCP handlers with tracing context
    // The tracer automatically extracts TenantID and Tier from the context
    mcpHandler := func(ctx context.Context, req map[string]interface{}) {
        ctx, span := tracer.StartSpan(ctx, "mcp_request")
        defer span.End()
        
        span.SetAttributes(attribute.String("mcp.tool", req["method"].(string)))
        // Route to agent-core service...
    }
}
```

---

## 5. The Status Synchronization Loop
Adhere to the Status Controller pattern. `mcp-server` **must not** use the `Deployment Adapter` to check status.

1. **Spoke:** Kagent Controller starts the Pod.
2. **Spoke Controller:** Watches the Pod. Uses `client_credentials` JWT to call `POST https://postgrest.hub.nutgrat.in/agent_infra_status`.
3. **Hub DB:** Inserts record. Trigger fires `pg_notify`.
4. **NATS Subscriber (`cmd/nats-subscriber/main.go`):**
   ```go
   // Listens to NATS (forwarded from pg_notify)
   func handleStatusUpdate(msg *nats.Msg) {
       var event AgentStatusEvent
       json.Unmarshal(msg.Data, &event)
       
       // Maps infrastructure status (Running/Idle) to AgentRegistry status (deployed)
       if event.Phase == "Running" || event.Phase == "Idle" {
           db.Exec("UPDATE agentregistry.deployments SET status = 'deployed' WHERE id = $1", event.DeploymentID)
       }
   }
   ```
5. **MCP Client:** Calls `get_agent_status`. `agents-core` queries `AgentRegistry` DB and returns `"deployed"` instantly.

---

## 6. Cluster Deployment & RBAC Details
To test this in a real cluster, the platform team must deploy these components with the correct permissions.

### 6.1 `mcp-server` Deployment (`manifests/platform-core/mcp-server.yaml`)
The `mcp-server` needs network access to the AgentRegistry, PostgREST, and crucially, **secrets containing the kubeconfigs for the Spoke clusters** (so the Deployment Adapter can connect).

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mcp-server
  namespace: zero-ops-system
spec:
  template:
    spec:
      containers:
        - name: mcp-server
          image: ghcr.io/zero-ops/mcp-server:latest
          env:
            - name: JAEGER_ENDPOINT
              value: "http://tempo.zero-ops-system:4318"
            - name: AGENT_REGISTRY_URL
              value: "http://agentregistry.platform-agentregistry.svc.cluster.local:8080"
          volumeMounts:
            - name: spoke-kubeconfigs
              mountPath: /etc/spoke-kubeconfigs
              readOnly: true
      volumes:
        # Secret managed by Cluster API containing spoke cluster credentials
        - name: spoke-kubeconfigs
          secret:
            secretName: all-spoke-kubeconfigs 
```

### 6.2 `nats-subscriber` Deployment (`manifests/platform-core/nats-subscriber.yaml`)
This daemon needs access to NATS and the Control Plane DB.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nats-subscriber
  namespace: zero-ops-system
spec:
  template:
    spec:
      containers:
        - name: nats-subscriber
          image: ghcr.io/zero-ops/nats-subscriber:latest
          env:
            - name: NATS_URL
              value: "nats://nats.zero-ops-system.svc.cluster.local:4222"
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: control-plane-db-credentials
                  key: dsn
```

---

## 7. Code Review Checklist for Platform Team
When platform engineers submit PRs for `agents-core`, reviewers must check:

* [ ] **Direct K8s Application:** Does `deploy_agent` use the `Deployment Adapter` to apply CRDs directly to the Spoke K8s API? *(Reject if it uses GitOps/IProvisioner for business agents).*
* [ ] **Async API Response:** Does `deploy_agent` return immediately after applying the CRD, without waiting for Pod readiness?
* [ ] **Status Isolation:** Does `get_agent_status` only query the `AgentRegistry` DB? *(Reject if it uses K8s `client-go` to check the Spoke).*
* [ ] **Telemetry Inclusion:** Are `metrics.RecordRequest` and `tracer.StartSpan` wrapping the MCP handler logic? *(Reject if missing, as it breaks observability).*
* [ ] **RLS Context:** Are all direct database queries inside `agents-core` (like `authorized_tools`) wrapped in a transaction that sets `SET LOCAL app.tenant_id`?