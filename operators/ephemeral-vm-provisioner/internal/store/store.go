package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kamarkat/kamarkat/operators/ephemeral-vm-provisioner/internal/hetzner"
)

// JobStatus mirrors the SDK's ephemeral_job_status enum.
type JobStatus string

const (
	StatusPending      JobStatus = "PENDING"
	StatusProvisioning JobStatus = "PROVISIONING"
	StatusRunning      JobStatus = "RUNNING"
	StatusCompleted    JobStatus = "COMPLETED"
	StatusFailed       JobStatus = "FAILED"
)

// Job is a row from the SDK's ephemeral_jobs table (the system of record).
type Job struct {
	ID         string
	TenantID   string
	Execution  string
	NodeID     string
	HookToken  string
	Status     JobStatus
	Payload    hetzner.JobPayload
	WorkerID   *string
	WorkerIP   *string
	VolumeID   *string
	StartedAt  *time.Time
	Completed  *time.Time
	ClaimedBy  *string
	LeaseUntil *time.Time
	Attempts   int
}

// Store provides Postgres access to ephemeral_jobs. The provisioner shares the
// SDK database; Postgres is the source of truth and leases are advisory locks.
type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	var payload []byte
	var startedAt, completed, leaseUntil *time.Time
	var attempts int32
	err := row.Scan(
		&j.ID, &j.TenantID, &j.Execution, &j.NodeID, &j.HookToken, &j.Status,
		&payload, &j.WorkerID, &j.WorkerIP, &j.VolumeID,
		&startedAt, &completed, &j.ClaimedBy, &leaseUntil, &attempts,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &j.Payload); err != nil {
		return nil, fmt.Errorf("failed to parse job payload for %s: %w", j.ID, err)
	}
	j.StartedAt = startedAt
	j.Completed = completed
	j.LeaseUntil = leaseUntil
	j.Attempts = int(attempts)
	return &j, nil
}

const jobColumns = `id, tenant_id, execution_id, node_id, hook_token, status, payload,
	worker_id, worker_ip, volume_id, started_at, completed_at, claimed_by, lease_until, attempts`

func statusesAsStrings(statuses []JobStatus) []string {
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = string(s)
	}
	return out
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM ephemeral_jobs WHERE id = $1`, id)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ClaimPending atomically moves a PENDING job to PROVISIONING and takes a lease
// (mirrors the SDK's claimPendingJob). Returns nil if already claimed or not pending.
func (s *Store) ClaimPending(ctx context.Context, id, claimedBy string, leaseSeconds int) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE ephemeral_jobs
		SET status = 'PROVISIONING',
			claimed_by = $2,
			started_at = NOW(),
			lease_until = NOW() + make_interval(secs => $3),
			attempts = attempts + 1,
			updated_at = NOW()
		WHERE id = $1
		  AND status = 'PENDING'
		  AND (lease_until IS NULL OR lease_until < NOW())
		RETURNING `+jobColumns,
		id, claimedBy, leaseSeconds)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ReclaimExpiredLease re-adopts a job whose lease expired (provisioner crashed
// mid-lifecycle) and drives it through reconciliation again. Mirrors the SDK's
// reclaimExpiredLease; never resets started_at so timeout stays anchored.
func (s *Store) ReclaimExpiredLease(ctx context.Context, id, claimedBy string, fromStatuses []JobStatus, leaseSeconds int) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE ephemeral_jobs
		SET claimed_by = $2,
			lease_until = NOW() + make_interval(secs => $3),
			attempts = attempts + 1,
			updated_at = NOW()
		WHERE id = $1
		  AND status = ANY($4)
		  AND lease_until IS NOT NULL
		  AND lease_until < NOW()
		RETURNING `+jobColumns,
		id, claimedBy, leaseSeconds, fromStatuses)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// RenewLease extends the lease on a job we own.
func (s *Store) RenewLease(ctx context.Context, id, claimedBy string, leaseSeconds int) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE ephemeral_jobs
		SET lease_until = NOW() + make_interval(secs => $3),
			updated_at = NOW()
		WHERE id = $1 AND claimed_by = $2
		RETURNING `+jobColumns,
		id, claimedBy, leaseSeconds)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// UpdateDetails persists provisioner-observed worker/volume facts (ADR-031).
func (s *Store) UpdateDetails(ctx context.Context, id string, patch struct {
	Status   *JobStatus
	WorkerID *string
	WorkerIP *string
	VolumeID *string
	Error    *string
}) (*Job, error) {
	set := ""
	args := []interface{}{id}
	n := 1
	appendSet := func(col string, val interface{}) {
		// Use reflect to detect typed nil pointers (e.g. *JobStatus(nil)) which
		// fail the bare `val == nil` check on interface{} parameters.
		if val == nil || (reflect.ValueOf(val).Kind() == reflect.Ptr && reflect.ValueOf(val).IsNil()) {
			return
		}
		n++
		set += fmt.Sprintf(", %s = $%d", col, n)
		args = append(args, val)
	}
	appendSet("status", patch.Status)
	appendSet("worker_id", patch.WorkerID)
	appendSet("worker_ip", patch.WorkerIP)
	appendSet("volume_id", patch.VolumeID)
	appendSet("error", patch.Error)
	if set == "" {
		return s.Get(ctx, id)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE ephemeral_jobs SET updated_at = NOW()`+set+` WHERE id = $1 RETURNING `+jobColumns,
		args...)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// MarkTerminal transitions to COMPLETED or FAILED, clears the lease.
func (s *Store) MarkTerminal(ctx context.Context, id string, status JobStatus, errMsg string) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE ephemeral_jobs
		SET status = $2,
			error = NULLIF($3, ''),
			completed_at = NOW(),
			lease_until = NULL,
			updated_at = NOW()
		WHERE id = $1
		RETURNING `+jobColumns,
		id, status, errMsg)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ClearWorkerID sets worker_id = NULL for a job once its VM has been deleted.
func (s *Store) ClearWorkerID(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE ephemeral_jobs SET worker_id = NULL, updated_at = NOW() WHERE id = $1`, id)
	return err
}

// ListWorkerIDs returns every non-null worker_id across all jobs, used by the
// orphan sweep to tell managed servers with a DB owner apart from true orphans.
func (s *Store) ListWorkerIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT worker_id FROM ephemeral_jobs WHERE worker_id IS NOT NULL AND worker_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set[id] = true
	}
	return set, rows.Err()
}

// ListByStatus returns jobs in the given statuses (optionally filtered to expired leases).
func (s *Store) ListByStatus(ctx context.Context, statuses []JobStatus, expiredLeaseOnly bool) ([]*Job, error) {
	query := `SELECT ` + jobColumns + ` FROM ephemeral_jobs WHERE status::text = ANY($1::text[])`
	args := []interface{}{statusesAsStrings(statuses)}
	if expiredLeaseOnly {
		query += ` AND lease_until IS NOT NULL AND lease_until < NOW()`
	}
	query += ` ORDER BY created_at ASC`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
