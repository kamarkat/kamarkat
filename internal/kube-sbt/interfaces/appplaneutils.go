package interfaces

import (
	"context"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// MigrationStep describes a single step in a tier migration plan (31.8).
type MigrationStep struct {
	Order        int    `json:"order"`
	Action       string `json:"action"` // "add" | "remove"
	ResourceType string `json:"resource_type"`
	Description  string `json:"description"`
}

// IApplicationPlaneUtils provides utility functions for Application Plane operations
type IApplicationPlaneUtils interface {
	// Resource Validation
	ValidateResourceConfig(ctx context.Context, spec models.ResourceSpec) error
	ValidateResourceDependencies(ctx context.Context, specs []models.ResourceSpec) error

	// Resource Naming
	GenerateResourceName(tenantID string, resourceType string) string

	// Resource Requirements
	CalculateResourceRequirements(ctx context.Context, tier string) ([]models.ResourceSpec, error)

	// Template Generation
	GenerateResourceTemplate(ctx context.Context, resourceType string, params map[string]interface{}) (string, error)

	// Cost Estimation
	EstimateResourceCost(ctx context.Context, specs []models.ResourceSpec) (float64, error)

	// Health Checks
	CheckResourceHealth(ctx context.Context, tenantID string, resourceID string) (string, error)

	// Batch Operations
	BatchProvisionResources(ctx context.Context, requests []models.ProvisionRequest) ([]models.ProvisionResult, error)

	// Custom Resource Types (31.6)
	RegisterCustomResourceType(name string, requiredKeys []string, costPerHour float64)

	// Migration Utilities (31.8)
	PlanTierMigration(fromTier, toTier string) ([]MigrationStep, error)
}
