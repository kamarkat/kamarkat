package agents

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
)

// DeployAgentArgs defines the input schema for deploy_agent tool
type DeployAgentArgs struct {
	AgentID string `json:"agent_id"`
}

// DeployAgentHandler is the MCP tool handler for deploy_agent
func DeployAgentHandler(deploymentService *service.DeploymentService) func(context.Context, *mcp.CallToolRequest, DeployAgentArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args DeployAgentArgs) (*mcp.CallToolResult, any, error) {
		// Convert args to params map
		params := map[string]interface{}{
			"agent_id": args.AgentID,
		}
		
		// Call deployment service
		result, err := deploymentService.DeployAgent(ctx, params)
		if err != nil {
			return nil, nil, err
		}
		
		// Marshal result to JSON
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return nil, nil, err
		}
		
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		}, nil, nil
	}
}