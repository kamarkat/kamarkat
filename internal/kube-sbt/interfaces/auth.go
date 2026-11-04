package interfaces

import (
	"context"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// IAuth provides authentication and authorization capabilities
type IAuth interface {
	// User Management
	CreateUser(ctx context.Context, user models.User) (*models.User, error)
	GetUser(ctx context.Context, userID string) (*models.User, error)
	UpdateUser(ctx context.Context, userID string, updates models.UserUpdates) (*models.User, error)
	DeleteUser(ctx context.Context, userID string) error
	ListUsers(ctx context.Context, tenantID, page, pageSize string) ([]models.User, error)
	SetUserGroups(ctx context.Context, email string, groups []string) error
	// CreateAdminUser bootstraps the platform administrator. Idempotent: it runs
	// on every control-plane start, so an existing admin is a success, not a
	// conflict.
	CreateAdminUser(ctx context.Context, props models.CreateAdminUserProps) error

	// Discovery
	// GetWellKnownEndpoint returns the OIDC discovery URL, or "" when the
	// provider is not configured. Readiness reports on it, so it must not error.
	GetWellKnownEndpoint() string

	// Session Management
	ValidateSession(ctx context.Context, token string) (*models.Session, error)
}
