package interfaces

import (
	"context"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// ITierManager provides tier configuration and quota management
type ITierManager interface {
	// Tier Management
	CreateTier(ctx context.Context, tier models.TierConfig) error
	GetTier(ctx context.Context, tierName string) (*models.TierConfig, error)
	UpdateTier(ctx context.Context, tierName string, updates models.TierUpdates) error
	DeleteTier(ctx context.Context, tierName string) error
	ListTiers(ctx context.Context) ([]models.TierConfig, error)

	// Quota Management
	ValidateTierQuota(ctx context.Context, tierName string, usage models.ResourceUsage) error
	GetTierQuotas(ctx context.Context, tierName string) (*models.TierQuotas, error)
	UpdateTierQuotas(ctx context.Context, tierName string, quotas models.TierQuotas) error

	// Tier Features
	GetTierFeatures(ctx context.Context, tierName string) ([]string, error)
	IsTierFeatureEnabled(ctx context.Context, tierName string, feature string) (bool, error)
}
