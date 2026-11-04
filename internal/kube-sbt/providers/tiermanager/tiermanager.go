package tiermanager

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// TierManager implements ITierManager backed by PostgreSQL with an in-memory
// read-through cache (17.2). -1 quota values mean unlimited (17.12).
type TierManager struct {
	pool *pgxpool.Pool

	mu    sync.RWMutex
	cache map[string]*models.TierConfig // name → config
	ttl   time.Duration
	exp   map[string]time.Time // name → expiry
}

// New creates a TierManager. cacheTTL controls how long tier configs are cached
// before a re-read from PostgreSQL (0 disables caching).
func New(pool *pgxpool.Pool, cacheTTL time.Duration) *TierManager {
	if cacheTTL == 0 {
		cacheTTL = 5 * time.Minute
	}
	return &TierManager{
		pool:  pool,
		cache: make(map[string]*models.TierConfig),
		ttl:   cacheTTL,
		exp:   make(map[string]time.Time),
	}
}

// ─── Tier CRUD (17.1–17.5) ────────────────────────────────────────────────────

// CreateTier inserts a new tier with validation (17.1).
func (tm *TierManager) CreateTier(ctx context.Context, tier models.TierConfig) error {
	if tier.Name == "" {
		return fmt.Errorf("tiermanager: tier name is required")
	}
	quotas, _ := json.Marshal(tier.Quotas)
	features, _ := json.Marshal(tier.Features)
	pricing, _ := json.Marshal(tier.Pricing)
	metadata, _ := json.Marshal(tier.Metadata)

	_, err := tm.pool.Exec(ctx, `
		INSERT INTO tier_configs (name, display_name, description, quotas, features, pricing, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tier.Name, tier.DisplayName, tier.Description,
		quotas, features, pricing, metadata,
	)
	if err != nil {
		return fmt.Errorf("tiermanager: create tier: %w", err)
	}
	tm.invalidate(tier.Name)
	return nil
}

// GetTier returns a tier config, using cache when fresh (17.2).
func (tm *TierManager) GetTier(ctx context.Context, tierName string) (*models.TierConfig, error) {
	if t := tm.fromCache(tierName); t != nil {
		return t, nil
	}
	row := tm.pool.QueryRow(ctx, `
		SELECT name, display_name, description, quotas, features, pricing, metadata, created_at, updated_at
		FROM tier_configs WHERE name = $1`, tierName)

	var t models.TierConfig
	var quotas, features, pricing, metadata []byte
	if err := row.Scan(&t.Name, &t.DisplayName, &t.Description,
		&quotas, &features, &pricing, &metadata,
		&t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, fmt.Errorf("tiermanager: tier %q not found", tierName)
	}
	json.Unmarshal(quotas, &t.Quotas)
	json.Unmarshal(features, &t.Features)
	json.Unmarshal(pricing, &t.Pricing)
	json.Unmarshal(metadata, &t.Metadata)

	tm.toCache(&t)
	return &t, nil
}

// UpdateTier applies partial updates (17.3).
func (tm *TierManager) UpdateTier(ctx context.Context, tierName string, updates models.TierUpdates) error {
	t, err := tm.GetTier(ctx, tierName)
	if err != nil {
		return err
	}
	if updates.DisplayName != nil {
		t.DisplayName = *updates.DisplayName
	}
	if updates.Description != nil {
		t.Description = *updates.Description
	}
	if updates.Quotas != nil {
		t.Quotas = *updates.Quotas
	}
	if updates.Features != nil {
		t.Features = *updates.Features
	}
	if updates.Pricing != nil {
		t.Pricing = *updates.Pricing
	}
	if updates.Metadata != nil {
		t.Metadata = *updates.Metadata
	}

	quotas, _ := json.Marshal(t.Quotas)
	features, _ := json.Marshal(t.Features)
	pricing, _ := json.Marshal(t.Pricing)
	metadata, _ := json.Marshal(t.Metadata)

	_, err = tm.pool.Exec(ctx, `
		UPDATE tier_configs
		SET display_name=$2, description=$3, quotas=$4, features=$5,
		    pricing=$6, metadata=$7, updated_at=NOW()
		WHERE name=$1`,
		tierName, t.DisplayName, t.Description,
		quotas, features, pricing, metadata,
	)
	if err != nil {
		return fmt.Errorf("tiermanager: update tier: %w", err)
	}
	tm.invalidate(tierName)
	return nil
}

// DeleteTier removes a tier. It checks that no tenants currently use it (17.4).
func (tm *TierManager) DeleteTier(ctx context.Context, tierName string) error {
	var count int
	_ = tm.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tenants WHERE tier=$1`, tierName,
	).Scan(&count)
	if count > 0 {
		return fmt.Errorf("tiermanager: cannot delete tier %q: %d tenant(s) still using it", tierName, count)
	}
	if _, err := tm.pool.Exec(ctx, `DELETE FROM tier_configs WHERE name=$1`, tierName); err != nil {
		return fmt.Errorf("tiermanager: delete tier: %w", err)
	}
	tm.invalidate(tierName)
	return nil
}

// ListTiers returns all tiers ordered by name (17.5).
func (tm *TierManager) ListTiers(ctx context.Context) ([]models.TierConfig, error) {
	rows, err := tm.pool.Query(ctx, `
		SELECT name, display_name, description, quotas, features, pricing, metadata, created_at, updated_at
		FROM tier_configs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tiers []models.TierConfig
	for rows.Next() {
		var t models.TierConfig
		var quotas, features, pricing, metadata []byte
		if err := rows.Scan(&t.Name, &t.DisplayName, &t.Description,
			&quotas, &features, &pricing, &metadata,
			&t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal(quotas, &t.Quotas)
		json.Unmarshal(features, &t.Features)
		json.Unmarshal(pricing, &t.Pricing)
		json.Unmarshal(metadata, &t.Metadata)
		tiers = append(tiers, t)
	}
	return tiers, rows.Err()
}

// ─── Quota Management (17.6–17.8, 17.12) ─────────────────────────────────────

// ValidateTierQuota checks usage against tier quotas. -1 means unlimited (17.6, 17.12).
func (tm *TierManager) ValidateTierQuota(ctx context.Context, tierName string, usage models.ResourceUsage) error {
	t, err := tm.GetTier(ctx, tierName)
	if err != nil {
		return err
	}
	q := t.Quotas
	if q.Users != -1 && usage.Users > q.Users {
		return fmt.Errorf("tiermanager: user quota exceeded (%d/%d)", usage.Users, q.Users)
	}
	if q.StorageGB != -1 && int(usage.StorageGB) > q.StorageGB {
		return fmt.Errorf("tiermanager: storage quota exceeded (%.1f/%d GB)", usage.StorageGB, q.StorageGB)
	}
	if q.APIRequests != -1 && usage.APIRequests > q.APIRequests {
		return fmt.Errorf("tiermanager: API request quota exceeded (%d/%d)", usage.APIRequests, q.APIRequests)
	}
	return nil
}

// GetTierQuotas returns just the quotas for a tier (17.7).
func (tm *TierManager) GetTierQuotas(ctx context.Context, tierName string) (*models.TierQuotas, error) {
	t, err := tm.GetTier(ctx, tierName)
	if err != nil {
		return nil, err
	}
	q := t.Quotas
	return &q, nil
}

// UpdateTierQuotas replaces the quotas for a tier (17.8).
func (tm *TierManager) UpdateTierQuotas(ctx context.Context, tierName string, quotas models.TierQuotas) error {
	return tm.UpdateTier(ctx, tierName, models.TierUpdates{Quotas: &quotas})
}

// ─── Feature Flags (17.9–17.10) ───────────────────────────────────────────────

// GetTierFeatures returns the feature list for a tier (17.9).
func (tm *TierManager) GetTierFeatures(ctx context.Context, tierName string) ([]string, error) {
	t, err := tm.GetTier(ctx, tierName)
	if err != nil {
		return nil, err
	}
	return t.Features, nil
}

// IsTierFeatureEnabled checks whether a specific feature is in the tier's list (17.10).
func (tm *TierManager) IsTierFeatureEnabled(ctx context.Context, tierName, feature string) (bool, error) {
	features, err := tm.GetTierFeatures(ctx, tierName)
	if err != nil {
		return false, err
	}
	for _, f := range features {
		if f == feature {
			return true, nil
		}
	}
	return false, nil
}

// ─── Cache helpers (17.2) ─────────────────────────────────────────────────────

func (tm *TierManager) fromCache(name string) *models.TierConfig {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if exp, ok := tm.exp[name]; ok && time.Now().Before(exp) {
		if t, ok := tm.cache[name]; ok {
			cp := *t
			return &cp
		}
	}
	return nil
}

func (tm *TierManager) toCache(t *models.TierConfig) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	cp := *t
	tm.cache[t.Name] = &cp
	tm.exp[t.Name] = time.Now().Add(tm.ttl)
}

func (tm *TierManager) invalidate(name string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	delete(tm.cache, name)
	delete(tm.exp, name)
}

// Compile-time assertion
var _ interfaces.ITierManager = (*TierManager)(nil)
