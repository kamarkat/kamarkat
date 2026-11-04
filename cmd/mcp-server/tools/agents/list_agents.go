package agents

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
)

// ListAgentsArgs defines the input schema for list_agents tool
type ListAgentsArgs struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

// ListAgentsHandler is the MCP tool handler for list_agents
func ListAgentsHandler(agentService *service.AgentService) func(context.Context, *mcp.CallToolRequest, ListAgentsArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListAgentsArgs) (*mcp.CallToolResult, any, error) {
		// Convert args to params map
		params := map[string]interface{}{
			"limit":  float64(args.Limit),
			"offset": float64(args.Offset),
		}
		
		// Call agent service
		result, err := agentService.ListAgents(ctx, params)
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
