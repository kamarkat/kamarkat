package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kamarkat/kamarkat/internal/platform/agent-core/service"
)

// ListAuthorizedToolsArgs defines the input schema for list_authorized_tools tool
type ListAuthorizedToolsArgs struct {
	Category string `json:"category,omitempty"`
}

// ListAuthorizedToolsHandler is the MCP tool handler for list_authorized_tools
func ListAuthorizedToolsHandler(agentService *service.AgentService) func(context.Context, *mcp.CallToolRequest, ListAuthorizedToolsArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ListAuthorizedToolsArgs) (*mcp.CallToolResult, any, error) {
		// Convert args to params map
		params := map[string]interface{}{}
		if args.Category != "" {
			params["category"] = args.Category
		}

		// 1. Extract tenant context from JWT (set by auth middleware)
		tenantID, ok := ctx.Value("tenant_id").(string)
		if !ok || tenantID == "" {
			return nil, nil, fmt.Errorf("tenant_id not found in context")
		}
		
		tenantTier, ok := ctx.Value("tenant_tier").(string)
		if !ok || tenantTier == "" {
			return nil, nil, fmt.Errorf("tenant_tier not found in context")
		}
		
		// 2. Call agent service ListAuthorizedTools method
		result, err := agentService.ListAuthorizedTools(ctx, params)
		if err != nil {
			return nil, nil, fmt.Errorf("list authorized tools failed: %w", err)
		}
		
		// 3. Return MCP-formatted response
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: formatAuthorizedToolsResponse(result)}},
		}, nil, nil
	}
}

// formatAuthorizedToolsResponse formats the service response for MCP
func formatAuthorizedToolsResponse(result interface{}) string {
	data, ok := result.(map[string]interface{})
	if !ok {
		return "Error: Invalid response format"
	}
	
	tools, _ := data["tools"].([]map[string]interface{})
	totalCount, _ := data["total_count"].(int)
	tenantTier, _ := data["tenant_tier"].(string)
	category, _ := data["category"].(string)
	
	response := map[string]interface{}{
		"authorized_tools": tools,
		"total_count":      totalCount,
		"tenant_tier":      tenantTier,
	}
	
	if category != "" {
		response["filtered_by_category"] = category
	}
	
	jsonBytes, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error formatting response: %v", err)
	}
	
	return string(jsonBytes)
}