package interfaces

import (
	"context"
	"time"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// Transaction represents a database transaction
type Transaction interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// IStorage provides tenant-aware data persistence capabilities
type IStorage interface {
	// Tenant Management
	CreateTenant(ctx context.Context, tenant models.Tenant) error
	GetTenant(ctx context.Context, tenantID string) (*models.Tenant, error)
	UpdateTenant(ctx context.Context, tenantID string, updates models.TenantUpdates) error
	DeleteTenant(ctx context.Context, tenantID string) error
	ListTenants(ctx context.Context, filters models.TenantFilters) ([]models.Tenant, error)

	// Tenant Registration
	CreateTenantRegistration(ctx context.Context, reg models.TenantRegistration) error
	GetTenantRegistration(ctx context.Context, regID string) (*models.TenantRegistration, error)
	UpdateTenantRegistration(ctx context.Context, regID string, updates models.RegistrationUpdates) error
	DeleteTenantRegistration(ctx context.Context, regID string) error
	ListTenantRegistrations(ctx context.Context, filters models.RegistrationFilters) ([]models.TenantRegistration, error)

	// Tenant Configuration
	SetTenantConfig(ctx context.Context, tenantID string, config map[string]interface{}) error
	GetTenantConfig(ctx context.Context, tenantID string) (map[string]interface{}, error)
	DeleteTenantConfig(ctx context.Context, tenantID string) error

	// Event Idempotency (Inbox Pattern)
	RecordProcessedEvent(ctx context.Context, eventID string) error
	IsEventProcessed(ctx context.Context, eventID string) (bool, error)

	// Webhook-Driven State Management (Event-Driven State Machine)
	UpdateTenantStatus(ctx context.Context, tenantID string, status string) error
	UpdateTenantArgoStatus(ctx context.Context, tenantID string, syncStatus, healthStatus string) error
	TouchTenantObservation(ctx context.Context, tenantID string) error

	// Orphaned Infrastructure Detection
	ListStuckTenants(ctx context.Context, stuckStates []string, olderThan time.Duration) ([]models.Tenant, error)
	ListUnobservedTenants(ctx context.Context, olderThan time.Duration) ([]models.Tenant, error)

	// Transaction Support
	BeginTransaction(ctx context.Context) (Transaction, error)
}
