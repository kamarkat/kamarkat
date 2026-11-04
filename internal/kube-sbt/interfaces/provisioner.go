package interfaces

import (
	"context"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// IProvisioner handles tenant resource provisioning and management
type IProvisioner interface {
	// Tenant Resource Management
	ProvisionTenant(ctx context.Context, req models.ProvisionRequest) (*models.ProvisionResult, error)
	DeprovisionTenant(ctx context.Context, req models.DeprovisionRequest) (*models.DeprovisionResult, error)
	UpdateTenantResources(ctx context.Context, req models.UpdateRequest) (*models.UpdateResult, error)

	// Status and Monitoring
	GetProvisioningStatus(ctx context.Context, tenantID string) (*models.ProvisioningStatus, error)
	ListTenantResources(ctx context.Context, tenantID string) ([]models.Resource, error)

	// Warm Pool Management
	ClaimWarmSlot(ctx context.Context, tenantID string, tier string) (*models.WarmSlotResult, error)
	RefillWarmPool(ctx context.Context, tier string, targetCount int) error
	GetWarmPoolStatus(ctx context.Context, tier string) (*models.WarmPoolStatus, error)

	// GitOps Integration
	CommitTenantConfig(ctx context.Context, tenantID string, config models.TenantConfig) error
	RollbackTenantConfig(ctx context.Context, tenantID string, commitHash string) error
	GetGitRepository(ctx context.Context, tenantID string) (*models.GitRepository, error)

	// Sync Triggers
	TriggerSync(ctx context.Context, tenantID string) error
	TriggerWebhookSync(ctx context.Context, tenantID string, webhookURL string) error
}
