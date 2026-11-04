package applicationplane

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// Config holds all dependencies for the Application Plane.
// Provisioner is required; ArgoCDAgent is optional.
type Config struct {
	EventBus    interfaces.IEventBus
	Provisioner interfaces.IProvisioner
	Storage     interfaces.IStorage // for idempotency checks
	ArgoCDAgent interfaces.IArgoCDAgent

	// RetryDelay between provisioning retries (default 5s)
	RetryDelay time.Duration
	// MaxRetries per event (default 3)
	MaxRetries int
}

func (c *Config) validate() error {
	if c.EventBus == nil {
		return fmt.Errorf("applicationplane: EventBus is required")
	}
	if c.Provisioner == nil {
		return fmt.Errorf("applicationplane: Provisioner is required")
	}
	return nil
}

func (c *Config) defaults() {
	if c.RetryDelay == 0 {
		c.RetryDelay = 5 * time.Second
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
}

// ApplicationPlane subscribes to Control Plane events and drives tenant
// provisioning through the IProvisioner interface.
// It publishes result events back to the Control Plane on success or failure.
type ApplicationPlane struct {
	cfg         Config
	eventBus    interfaces.IEventBus
	provisioner interfaces.IProvisioner
}

// NewApplicationPlane validates config, applies defaults, and returns a ready
// ApplicationPlane. Call Start to begin processing events.
func NewApplicationPlane(cfg Config) (*ApplicationPlane, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.defaults()
	return &ApplicationPlane{
		cfg:         cfg,
		eventBus:    cfg.EventBus,
		provisioner: cfg.Provisioner,
	}, nil
}

// Start subscribes to all Control Plane events and blocks until ctx is cancelled.
func (ap *ApplicationPlane) Start(ctx context.Context) error {
	if err := ap.subscribeEvents(ctx); err != nil {
		return fmt.Errorf("applicationplane: subscribe: %w", err)
	}
	log.Printf("applicationplane: started, waiting for events")
	<-ctx.Done()
	return nil
}

// Stop is a no-op — subscriptions are drained when the EventBus is closed.
func (ap *ApplicationPlane) Stop() {}

// ─── Event subscriptions (13.3, 13.5) ────────────────────────────────────────

func (ap *ApplicationPlane) subscribeEvents(ctx context.Context) error {
	handlers := map[string]interfaces.EventHandler{
		models.EventOnboardingRequest:  ap.onOnboardingRequest,
		models.EventOffboardingRequest: ap.onOffboardingRequest,
		models.EventActivateRequest:    ap.onActivateRequest,
		models.EventDeactivateRequest:  ap.onDeactivateRequest,
		models.EventTierChanged:        ap.onTierChanged,
	}
	for eventType, handler := range handlers {
		if err := ap.eventBus.SubscribeQueue(ctx, eventType, "application-plane", handler); err != nil {
			return fmt.Errorf("subscribe %s: %w", eventType, err)
		}
	}
	return nil
}

// ─── Provisioning Workflows (14.1–14.8) ──────────────────────────────────────

// onOnboardingRequest provisions a new tenant and publishes success/failure (14.1).
func (ap *ApplicationPlane) onOnboardingRequest(ctx context.Context, event models.Event) error {
	if dup, err := ap.isDuplicate(ctx, event.ID); dup || err != nil {
		return err
	}
	tenantID, _ := event.Detail["tenantId"].(string)
	tier, _ := event.Detail["tier"].(string)
	name, _ := event.Detail["name"].(string)
	email, _ := event.Detail["email"].(string)

	log.Printf("applicationplane: onboarding tenant=%s tier=%s", tenantID, tier)

	result, err := ap.withRetry(ctx, func() (interface{}, error) {
		return ap.provisioner.ProvisionTenant(ctx, models.ProvisionRequest{
			TenantID: tenantID, Tier: tier, Name: name, Email: email,
		})
	})
	if err != nil {
		return ap.publishFailure(ctx, models.EventProvisionFailure, tenantID, err)
	}
	pr := result.(*models.ProvisionResult)
	return ap.publishSuccess(ctx, models.EventProvisionSuccess, tenantID, map[string]interface{}{
		"tenantId":      tenantID,
		"gitCommitHash": pr.GitCommitHash,
		"status":        pr.Status,
	})
}

// onOffboardingRequest deprovisions a tenant (14.2).
func (ap *ApplicationPlane) onOffboardingRequest(ctx context.Context, event models.Event) error {
	if dup, err := ap.isDuplicate(ctx, event.ID); dup || err != nil {
		return err
	}
	tenantID, _ := event.Detail["tenantId"].(string)
	log.Printf("applicationplane: offboarding tenant=%s", tenantID)

	_, err := ap.withRetry(ctx, func() (interface{}, error) {
		return ap.provisioner.DeprovisionTenant(ctx, models.DeprovisionRequest{TenantID: tenantID})
	})
	if err != nil {
		return ap.publishFailure(ctx, models.EventDeprovisionFailure, tenantID, err)
	}
	return ap.publishSuccess(ctx, models.EventDeprovisionSuccess, tenantID, map[string]interface{}{
		"tenantId": tenantID,
	})
}

// onActivateRequest re-activates a suspended tenant (14.3).
func (ap *ApplicationPlane) onActivateRequest(ctx context.Context, event models.Event) error {
	if dup, err := ap.isDuplicate(ctx, event.ID); dup || err != nil {
		return err
	}
	tenantID, _ := event.Detail["tenantId"].(string)
	log.Printf("applicationplane: activating tenant=%s", tenantID)

	_, err := ap.withRetry(ctx, func() (interface{}, error) {
		return ap.provisioner.UpdateTenantResources(ctx, models.UpdateRequest{
			TenantID: tenantID, Action: "activate",
		})
	})
	if err != nil {
		return ap.publishFailure(ctx, models.EventActivateFailure, tenantID, err)
	}
	return ap.publishSuccess(ctx, models.EventActivateSuccess, tenantID, map[string]interface{}{
		"tenantId": tenantID,
	})
}

// onDeactivateRequest suspends a tenant (14.4).
func (ap *ApplicationPlane) onDeactivateRequest(ctx context.Context, event models.Event) error {
	if dup, err := ap.isDuplicate(ctx, event.ID); dup || err != nil {
		return err
	}
	tenantID, _ := event.Detail["tenantId"].(string)
	log.Printf("applicationplane: deactivating tenant=%s", tenantID)

	_, err := ap.withRetry(ctx, func() (interface{}, error) {
		return ap.provisioner.UpdateTenantResources(ctx, models.UpdateRequest{
			TenantID: tenantID, Action: "deactivate",
		})
	})
	if err != nil {
		return ap.publishFailure(ctx, models.EventDeactivateFailure, tenantID, err)
	}
	return ap.publishSuccess(ctx, models.EventDeactivateSuccess, tenantID, map[string]interface{}{
		"tenantId": tenantID,
	})
}

// onTierChanged adjusts tenant resources when tier changes (14.5).
func (ap *ApplicationPlane) onTierChanged(ctx context.Context, event models.Event) error {
	if dup, err := ap.isDuplicate(ctx, event.ID); dup || err != nil {
		return err
	}
	tenantID, _ := event.Detail["tenantId"].(string)
	newTier, _ := event.Detail["newTier"].(string)
	log.Printf("applicationplane: tier change tenant=%s newTier=%s", tenantID, newTier)

	_, err := ap.withRetry(ctx, func() (interface{}, error) {
		return ap.provisioner.UpdateTenantResources(ctx, models.UpdateRequest{
			TenantID: tenantID, Tier: newTier, Action: "tier_change",
		})
	})
	if err != nil {
		return ap.publishFailure(ctx, models.EventProvisionFailure, tenantID, err)
	}
	return ap.publishSuccess(ctx, models.EventProvisionSuccess, tenantID, map[string]interface{}{
		"tenantId": tenantID,
		"newTier":  newTier,
	})
}

// ─── Helpers (13.6, 14.6, 14.7, 14.8) ───────────────────────────────────────

// isDuplicate checks the Inbox Pattern via IStorage when available (14.6).
func (ap *ApplicationPlane) isDuplicate(ctx context.Context, eventID string) (bool, error) {
	if ap.cfg.Storage == nil {
		return false, nil
	}
	processed, err := ap.cfg.Storage.IsEventProcessed(ctx, eventID)
	if err != nil || !processed {
		return processed, err
	}
	log.Printf("applicationplane: duplicate event %s, skipping", eventID)
	return true, nil
}

// withRetry executes fn with exponential backoff up to MaxRetries (13.6, 14.7).
func (ap *ApplicationPlane) withRetry(ctx context.Context, fn func() (interface{}, error)) (interface{}, error) {
	var lastErr error
	delay := ap.cfg.RetryDelay
	for i := 0; i <= ap.cfg.MaxRetries; i++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if i < ap.cfg.MaxRetries {
			log.Printf("applicationplane: retry %d/%d after %s: %v", i+1, ap.cfg.MaxRetries, delay, err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				delay *= 2
			}
		}
	}
	return nil, lastErr
}

func (ap *ApplicationPlane) publishSuccess(ctx context.Context, detailType, tenantID string, detail map[string]interface{}) error {
	log.Printf("applicationplane: %s tenant=%s", detailType, tenantID)
	return ap.eventBus.Publish(ctx, models.NewEvent(detailType, models.ApplicationPlaneEventSource, detail))
}

func (ap *ApplicationPlane) publishFailure(ctx context.Context, detailType, tenantID string, cause error) error {
	log.Printf("applicationplane: %s tenant=%s err=%v", detailType, tenantID, cause)
	return ap.eventBus.Publish(ctx, models.NewEvent(detailType, models.ApplicationPlaneEventSource, map[string]interface{}{
		"tenantId": tenantID,
		"error":    cause.Error(),
	}))
}
