package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// Config holds PostgreSQL connection configuration (4.17 — pgxpool = PgBouncer-compatible)
type Config struct {
	DSN         string // postgres://user:pass@host:5432/db?sslmode=disable
	MaxConns    int32  // max pool connections (default 10)
	MinConns    int32  // min idle connections (default 2)
	BypassRLS   bool   // set app.bypass_rls=true for platform admin queries
}

// Storage implements interfaces.IStorage using pgx/v5 connection pool
type Storage struct {
	pool      *pgxpool.Pool
	bypassRLS bool
}

// NewStorage creates a new PostgreSQL storage provider and runs migrations.
func NewStorage(ctx context.Context, cfg Config) (*Storage, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	} else {
		poolCfg.MaxConns = 10
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	} else {
		poolCfg.MinConns = 2
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	s := &Storage{pool: pool, bypassRLS: cfg.BypassRLS}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return s, nil
}

// Close releases the connection pool.
func (s *Storage) Close() { s.pool.Close() }

// conn returns a connection with RLS context pre-set.
func (s *Storage) setRLS(ctx context.Context, conn *pgxpool.Conn) error {
	if s.bypassRLS {
		_, err := conn.Exec(ctx, "SET app.bypass_rls = 'true'")
		return err
	}
	if tid, ok := ctx.Value("tenant_id").(string); ok && tid != "" {
		_, err := conn.Exec(ctx, "SET app.tenant_id = $1", tid)
		return err
	}
	return nil
}

// withConn acquires a connection, sets RLS, runs fn, then releases.
func (s *Storage) withConn(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if err := s.setRLS(ctx, conn); err != nil {
		return err
	}
	return fn(conn)
}

// ─── Tenant Management (4.3–4.7) ────────────────────────────────────────────

func (s *Storage) CreateTenant(ctx context.Context, t models.Tenant) error {
	cfg, _ := json.Marshal(t.Config)
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`INSERT INTO tenants (id,name,tier,status,owner_email,config,created_at,updated_at,last_observed_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			t.ID, t.Name, t.Tier, string(t.Status), t.OwnerEmail,
			cfg, t.CreatedAt, t.UpdatedAt, t.LastObservedAt,
		)
		return err
	})
}

func (s *Storage) GetTenant(ctx context.Context, tenantID string) (*models.Tenant, error) {
	var t models.Tenant
	var cfg []byte
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		return c.QueryRow(ctx,
			`SELECT id,name,tier,status,argo_sync_status,argo_health_status,owner_email,config,created_at,updated_at,last_observed_at
			 FROM tenants WHERE id=$1`, tenantID,
		).Scan(&t.ID, &t.Name, &t.Tier, &t.Status,
			&t.ArgoSyncStatus, &t.ArgoHealthStatus, &t.OwnerEmail,
			&cfg, &t.CreatedAt, &t.UpdatedAt, &t.LastObservedAt)
	})
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(cfg, &t.Config)
	return &t, nil
}

func (s *Storage) UpdateTenant(ctx context.Context, tenantID string, u models.TenantUpdates) error {
	var cfg []byte
	if u.Config != nil {
		cfg, _ = json.Marshal(*u.Config)
	}
	var status *string
	if u.Status != nil {
		s := string(*u.Status)
		status = &s
	}
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`UPDATE tenants SET
			   name               = COALESCE($2, name),
			   tier               = COALESCE($3, tier),
			   status             = COALESCE($4, status),
			   argo_sync_status   = COALESCE($5, argo_sync_status),
			   argo_health_status = COALESCE($6, argo_health_status),
			   config             = COALESCE($7, config),
			   updated_at         = NOW()
			 WHERE id = $1`,
			tenantID, u.Name, u.Tier, status,
			u.ArgoSyncStatus, u.ArgoHealthStatus, cfg,
		)
		return err
	})
}

func (s *Storage) DeleteTenant(ctx context.Context, tenantID string) error {
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID)
		return err
	})
}

func (s *Storage) ListTenants(ctx context.Context, f models.TenantFilters) ([]models.Tenant, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var status *string
	if f.Status != nil {
		s := string(*f.Status)
		status = &s
	}
	var tenants []models.Tenant
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		rows, err := c.Query(ctx,
			`SELECT id,name,tier,status,argo_sync_status,argo_health_status,owner_email,config,created_at,updated_at,last_observed_at
			 FROM tenants
			 WHERE ($1::text IS NULL OR status=$1)
			   AND ($2::text IS NULL OR tier=$2)
			 ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
			status, f.Tier, limit, f.Offset,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t models.Tenant
			var cfg []byte
			if err := rows.Scan(&t.ID, &t.Name, &t.Tier, &t.Status,
				&t.ArgoSyncStatus, &t.ArgoHealthStatus, &t.OwnerEmail,
				&cfg, &t.CreatedAt, &t.UpdatedAt, &t.LastObservedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(cfg, &t.Config)
			tenants = append(tenants, t)
		}
		return rows.Err()
	})
	return tenants, err
}

// ─── Tenant Registration (4.8) ───────────────────────────────────────────────

func (s *Storage) CreateTenantRegistration(ctx context.Context, r models.TenantRegistration) error {
	cfg, _ := json.Marshal(r.Config)
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`INSERT INTO tenant_registrations (id,tenant_id,status,name,email,tier,config,created_at,updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			r.ID, nilIfEmpty(r.TenantID), r.Status, r.Name, r.Email, r.Tier, cfg, r.CreatedAt, r.UpdatedAt,
		)
		return err
	})
}

func (s *Storage) GetTenantRegistration(ctx context.Context, regID string) (*models.TenantRegistration, error) {
	var r models.TenantRegistration
	var cfg []byte
	var tenantID *string
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		return c.QueryRow(ctx,
			`SELECT id,tenant_id,status,name,email,tier,config,created_at,updated_at
			 FROM tenant_registrations WHERE id=$1`, regID,
		).Scan(&r.ID, &tenantID, &r.Status, &r.Name, &r.Email, &r.Tier, &cfg, &r.CreatedAt, &r.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	if tenantID != nil {
		r.TenantID = *tenantID
	}
	_ = json.Unmarshal(cfg, &r.Config)
	return &r, nil
}

func (s *Storage) UpdateTenantRegistration(ctx context.Context, regID string, u models.RegistrationUpdates) error {
	var cfg []byte
	if u.Config != nil {
		cfg, _ = json.Marshal(*u.Config)
	}
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`UPDATE tenant_registrations SET
			   tenant_id  = COALESCE($2, tenant_id),
			   status     = COALESCE($3, status),
			   config     = COALESCE($4, config),
			   updated_at = NOW()
			 WHERE id=$1`,
			regID, u.TenantID, u.Status, cfg,
		)
		return err
	})
}

func (s *Storage) DeleteTenantRegistration(ctx context.Context, regID string) error {
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM tenant_registrations WHERE id=$1`, regID)
		return err
	})
}

func (s *Storage) ListTenantRegistrations(ctx context.Context, f models.RegistrationFilters) ([]models.TenantRegistration, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var regs []models.TenantRegistration
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		rows, err := c.Query(ctx,
			`SELECT id,tenant_id,status,name,email,tier,config,created_at,updated_at
			 FROM tenant_registrations
			 WHERE ($1::text IS NULL OR status=$1)
			   AND ($2::text IS NULL OR tier=$2)
			 ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
			f.Status, f.Tier, limit, f.Offset,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r models.TenantRegistration
			var cfg []byte
			var tenantID *string
			if err := rows.Scan(&r.ID, &tenantID, &r.Status, &r.Name, &r.Email, &r.Tier, &cfg, &r.CreatedAt, &r.UpdatedAt); err != nil {
				return err
			}
			if tenantID != nil {
				r.TenantID = *tenantID
			}
			_ = json.Unmarshal(cfg, &r.Config)
			regs = append(regs, r)
		}
		return rows.Err()
	})
	return regs, err
}

// ─── Tenant Configuration (4.9) ──────────────────────────────────────────────

func (s *Storage) SetTenantConfig(ctx context.Context, tenantID string, config map[string]interface{}) error {
	cfg, _ := json.Marshal(config)
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`INSERT INTO tenant_configs (tenant_id, config, updated_at) VALUES ($1,$2,NOW())
			 ON CONFLICT (tenant_id) DO UPDATE SET config=$2, updated_at=NOW()`,
			tenantID, cfg,
		)
		return err
	})
}

func (s *Storage) GetTenantConfig(ctx context.Context, tenantID string) (map[string]interface{}, error) {
	var cfg []byte
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		return c.QueryRow(ctx, `SELECT config FROM tenant_configs WHERE tenant_id=$1`, tenantID).Scan(&cfg)
	})
	if err != nil {
		return nil, err
	}
	var result map[string]interface{}
	_ = json.Unmarshal(cfg, &result)
	return result, nil
}

func (s *Storage) DeleteTenantConfig(ctx context.Context, tenantID string) error {
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM tenant_configs WHERE tenant_id=$1`, tenantID)
		return err
	})
}

// ─── Event Idempotency / Inbox Pattern (4.10) ────────────────────────────────

func (s *Storage) RecordProcessedEvent(ctx context.Context, eventID string) error {
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`, eventID)
		return err
	})
}

func (s *Storage) IsEventProcessed(ctx context.Context, eventID string) (bool, error) {
	var exists bool
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		return c.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM processed_events WHERE event_id=$1)`, eventID,
		).Scan(&exists)
	})
	return exists, err
}

// ─── Webhook-Driven State Management (4.11, 4.12) ────────────────────────────

func (s *Storage) UpdateTenantStatus(ctx context.Context, tenantID string, status string) error {
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`UPDATE tenants SET status=$2, updated_at=NOW() WHERE id=$1`, tenantID, status)
		return err
	})
}

func (s *Storage) UpdateTenantArgoStatus(ctx context.Context, tenantID, syncStatus, healthStatus string) error {
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx,
			`UPDATE tenants SET argo_sync_status=$2, argo_health_status=$3, updated_at=NOW() WHERE id=$1`,
			tenantID, syncStatus, healthStatus)
		return err
	})
}

func (s *Storage) TouchTenantObservation(ctx context.Context, tenantID string) error {
	return s.withConn(ctx, func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx, `UPDATE tenants SET last_observed_at=NOW() WHERE id=$1`, tenantID)
		return err
	})
}

// ─── Active Reconciliation (4.13, 4.14) ──────────────────────────────────────

func (s *Storage) ListStuckTenants(ctx context.Context, stuckStates []string, olderThan time.Duration) ([]models.Tenant, error) {
	cutoff := time.Now().Add(-olderThan)
	var tenants []models.Tenant
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		rows, err := c.Query(ctx,
			`SELECT id,name,tier,status,argo_sync_status,argo_health_status,owner_email,config,created_at,updated_at,last_observed_at
			 FROM tenants WHERE status=ANY($1) AND updated_at<$2`,
			stuckStates, cutoff,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		return scanTenants(rows, &tenants)
	})
	return tenants, err
}

func (s *Storage) ListUnobservedTenants(ctx context.Context, olderThan time.Duration) ([]models.Tenant, error) {
	cutoff := time.Now().Add(-olderThan)
	var tenants []models.Tenant
	err := s.withConn(ctx, func(c *pgxpool.Conn) error {
		rows, err := c.Query(ctx,
			`SELECT id,name,tier,status,argo_sync_status,argo_health_status,owner_email,config,created_at,updated_at,last_observed_at
			 FROM tenants WHERE last_observed_at<$1 AND status NOT IN ('CREATING','DELETED')`,
			cutoff,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		return scanTenants(rows, &tenants)
	})
	return tenants, err
}

// ─── Transaction Support (4.15) ──────────────────────────────────────────────

type tx struct {
	pgxTx pgx.Tx
}

func (t *tx) Commit(ctx context.Context) error   { return t.pgxTx.Commit(ctx) }
func (t *tx) Rollback(ctx context.Context) error { return t.pgxTx.Rollback(ctx) }

func (s *Storage) BeginTransaction(ctx context.Context) (interfaces.Transaction, error) {
	pgxTx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &tx{pgxTx: pgxTx}, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func scanTenants(rows pgx.Rows, out *[]models.Tenant) error {
	for rows.Next() {
		var t models.Tenant
		var cfg []byte
		if err := rows.Scan(&t.ID, &t.Name, &t.Tier, &t.Status,
			&t.ArgoSyncStatus, &t.ArgoHealthStatus, &t.OwnerEmail,
			&cfg, &t.CreatedAt, &t.UpdatedAt, &t.LastObservedAt); err != nil {
			return err
		}
		_ = json.Unmarshal(cfg, &t.Config)
		*out = append(*out, t)
	}
	return rows.Err()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
