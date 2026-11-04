package agents

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
)

// UpdateAgentArgs defines the input schema for update_agent tool
type UpdateAgentArgs struct {
	AgentID           string                 `json:"agent_id"`
	SystemMessage     string                 `json:"system_message,omitempty"`
	ToolAccess        []string               `json:"tool_access,omitempty"`
	MemoryConfig      map[string]interface{} `json:"memory_config,omitempty"`
	GuardrailPolicies map[string]interface{} `json:"guardrail_policies,omitempty"`
	ModelConfig       map[string]interface{} `json:"model_config,omitempty"`
}

// UpdateAgentHandler is the MCP tool handler for update_agent
func UpdateAgentHandler(agentService *service.AgentService) func(context.Context, *mcp.CallToolRequest, UpdateAgentArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args UpdateAgentArgs) (*mcp.CallToolResult, any, error) {
		// Convert args to params map
		params := map[string]interface{}{
			"agent_id": args.AgentID,
		}
		
		// Only include non-empty fields
		if args.SystemMessage != "" {
			params["system_message"] = args.SystemMessage
		}
		if len(args.ToolAccess) > 0 {
			params["tool_access"] = convertToInterfaceSlice(args.ToolAccess)
		}
		if args.MemoryConfig != nil {
			params["memory_config"] = args.MemoryConfig
		}
		if args.GuardrailPolicies != nil {
			params["guardrail_policies"] = args.GuardrailPolicies
		}
		if args.ModelConfig != nil {
			params["model_config"] = args.ModelConfig
		}
		
		// Call agent service
		result, err := agentService.UpdateAgent(ctx, params)
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