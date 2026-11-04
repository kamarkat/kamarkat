package agents

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
)

// GetAgentStatusArgs defines the input schema for get_agent_status tool
type GetAgentStatusArgs struct {
	AgentID string `json:"agent_id"`
}

// GetAgentStatusHandler is the MCP tool handler for get_agent_status
func GetAgentStatusHandler(statusService *service.StatusService) func(context.Context, *mcp.CallToolRequest, GetAgentStatusArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args GetAgentStatusArgs) (*mcp.CallToolResult, any, error) {
		// Convert args to params map
		params := map[string]interface{}{
			"agent_id": args.AgentID,
		}
		
		// Call status service
		result, err := statusService.GetAgentStatus(ctx, params)
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