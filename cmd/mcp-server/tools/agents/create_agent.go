package agents

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
)

// CreateAgentArgs defines the input schema for create_agent tool
type CreateAgentArgs struct {
	Name              string                 `json:"name"`
	Version           string                 `json:"version,omitempty"`
	AgentType         string                 `json:"agent_type"`
	SystemMessage     string                 `json:"system_message"`
	ToolAccess        []string               `json:"tool_access"`
	MemoryConfig      map[string]interface{} `json:"memory_config"`
	GuardrailPolicies map[string]interface{} `json:"guardrail_policies"`
	ModelConfig       map[string]interface{} `json:"model_config"`
}

// CreateAgentHandler is the MCP tool handler for create_agent
func CreateAgentHandler(agentService *service.AgentService) func(context.Context, *mcp.CallToolRequest, CreateAgentArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args CreateAgentArgs) (*mcp.CallToolResult, any, error) {
		// Convert args to params map
		params := map[string]interface{}{
			"name":               args.Name,
			"version":            args.Version,
			"agent_type":         args.AgentType,
			"system_message":     args.SystemMessage,
			"tool_access":        convertToInterfaceSlice(args.ToolAccess),
			"memory_config":      args.MemoryConfig,
			"guardrail_policies": args.GuardrailPolicies,
			"model_config":       args.ModelConfig,
		}
		
		// Call agent service
		result, err := agentService.CreateAgent(ctx, params)
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

func convertToInterfaceSlice(s []string) []interface{} {
	result := make([]interface{}, len(s))
	for i, v := range s {
		result[i] = v
	}
	return result
}
