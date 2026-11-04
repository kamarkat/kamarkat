package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/cmd/mcp-server/tools/agents"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/client"
	agentdb "github.com/kamarkat/kamarkat/internal/platform/agent-core/database/generated"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
)

type tenantListArgs struct{}

func tenantList(_ context.Context, _ *mcp.CallToolRequest, _ tenantListArgs) (*mcp.CallToolResult, any, error) {
	result, _ := json.Marshal([]map[string]string{
		{"id": "tenant-demo", "name": "Demo Tenant", "status": "active"},
	})
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(result)}},
	}, nil, nil
}

func main() {
	// Initialize AgentRegistry HTTP client
	agentRegistryClient := client.NewAgentRegistryClient(client.Config{
		BaseURL: "http://agentregistry.platform-agentregistry.svc.cluster.local:8080",
		Timeout: 30 * time.Second,
	})

	// Initialize Hub PostgREST client
	hubClient := client.NewHubClient(client.HubConfig{
		BaseURL: "https://postgrest.hub.nutgrat.in",
		Timeout: 10 * time.Second,
	})

	// TODO: Initialize event bus and DB queries from environment
	var eventBus interfaces.IEventBus
	var queries *agentdb.Queries

	// Initialize agent service
	agentService := service.NewAgentService(service.Config{
		AgentRegistryClient: agentRegistryClient,
		HubClient:           hubClient,
		EventBus:            eventBus,
		Queries:             queries,
	})

	// Initialize deployment service
	deploymentService := service.NewDeploymentService(service.DeploymentConfig{
		AgentRegistryClient: agentRegistryClient,
		EventBus:            eventBus,
	})

	// Initialize status service
	statusService := service.NewStatusService(service.StatusConfig{
		AgentRegistryClient: agentRegistryClient,
		HubClient:           hubClient,
	})

	server := mcp.NewServer(&mcp.Implementation{Name: "mcp-server", Version: "1.0.0"}, nil)

	// Register tenant tools
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tenant_list",
		Description: "List all tenants",
	}, tenantList)

	// Register agent tools
	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_agent",
		Description: "Create a new agent definition",
	}, agents.CreateAgentHandler(agentService))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_agents",
		Description: "List all agents for current tenant",
	}, agents.ListAgentsHandler(agentService))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "deploy_agent",
		Description: "Deploy agent to Spoke cluster",
	}, agents.DeployAgentHandler(deploymentService))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_agent_status",
		Description: "Get current status of agent deployment",
	}, agents.GetAgentStatusHandler(statusService))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_agent",
		Description: "Update an existing agent configuration",
	}, agents.UpdateAgentHandler(agentService))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_agent",
		Description: "Delete agent and undeploy runtime",
	}, agents.DeleteAgentHandler(agentService))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_authorized_tools",
		Description: "List tools authorized for current tenant",
	}, agents.ListAuthorizedToolsHandler(agentService))

	http.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Println("mcp-server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
