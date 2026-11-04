package interfaces

import (
	"context"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// IArgoCDAgent provides distributed GitOps agent management capabilities
type IArgoCDAgent interface {
	// Agent Lifecycle
	DeployAgent(ctx context.Context, config models.AgentConfig) error
	UpdateAgent(ctx context.Context, agentID string, config models.AgentConfig) error
	RemoveAgent(ctx context.Context, agentID string) error

	// Agent Status
	GetAgentStatus(ctx context.Context, agentID string) (*models.AgentStatus, error)
	ListAgents(ctx context.Context, tenantID string) ([]models.AgentStatus, error)

	// Agent Credentials
	GenerateAgentCredentials(ctx context.Context, agentID string) (map[string]string, error)
	RotateAgentCredentials(ctx context.Context, agentID string) (map[string]string, error)

	// Agent Configuration
	GetAgentConfig(ctx context.Context, agentID string) (*models.AgentConfig, error)
	UpdateAgentMode(ctx context.Context, agentID string, mode string) error // managed, autonomous

	// Application Management
	ListAgentApplications(ctx context.Context, agentID string) ([]map[string]interface{}, error)
	SyncAgentApplication(ctx context.Context, agentID string, appName string) error
}
