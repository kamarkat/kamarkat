package interfaces

import (
	"context"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// EventHandler is a function that handles an event
type EventHandler func(ctx context.Context, event models.Event) error

// IEventBus provides message bus capabilities for inter-plane communication
type IEventBus interface {
	// Event Publishing
	Publish(ctx context.Context, event models.Event) error
	PublishAsync(ctx context.Context, event models.Event) error

	// Event Subscription
	Subscribe(ctx context.Context, eventType string, handler EventHandler) error
	SubscribeQueue(ctx context.Context, eventType string, queueGroup string, handler EventHandler) error

	// Event Definitions
	GetControlPlaneEventSource() string
	GetApplicationPlaneEventSource() string
	CreateControlPlaneEvent(detailType string) models.EventDefinition
	CreateApplicationPlaneEvent(detailType string) models.EventDefinition
	CreateCustomEvent(detailType string, source string) models.EventDefinition

	// Standard Events
	GetStandardEvents() map[string]models.EventDefinition

	// Permissions
	GrantPublishPermissions(grantee string) error
}
