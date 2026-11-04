package agents

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
)

// DeleteAgentArgs defines the input schema for delete_agent tool
type DeleteAgentArgs struct {
	AgentID     string `json:"agent_id"`
	PurgeMemory bool   `json:"purge_memory,omitempty"`
}

// DeleteAgentHandler is the MCP tool handler for delete_agent
func DeleteAgentHandler(agentService *service.AgentService) func(context.Context, *mcp.CallToolRequest, DeleteAgentArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args DeleteAgentArgs) (*mcp.CallToolResult, any, error) {
		// Convert args to params map
		params := map[string]interface{}{
			"agent_id":     args.AgentID,
			"purge_memory": args.PurgeMemory,
		}
		
		// Call agent service
		result, err := agentService.DeleteAgent(ctx, params)
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