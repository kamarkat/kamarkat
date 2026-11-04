package provision

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/kamarkat/kamarkat/operators/ephemeral-vm-provisioner/internal/hetzner"
	"github.com/kamarkat/kamarkat/operators/ephemeral-vm-provisioner/internal/store"
)

// Reconciler owns the Hetzner VM lifecycle for all ephemeral jobs. Postgres is
// the source of truth; leases are advisory locks that survive provisioner
// restarts (ADR-031). Graphile Worker delivers the initial kick but this loop
// is the safety net for lease recovery, timeout and orphan cleanup.
type Reconciler struct {
	store    *store.Store
	hcloud   *hetzner.Client
	hostname string
	Log      *slog.Logger
}

func NewReconciler(s *store.Store, h *hetzner.Client, log *slog.Logger) *Reconciler {
	hostname := os.Getenv("PROVISIONER_HOSTNAME")
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	return &Reconciler{store: s, hcloud: h, hostname: hostname, Log: log}
}

const (
	leaseSeconds      = 300
	reconcileInterval = 30 * time.Second
)

// Run executes the periodic reconcile + orphan sweep until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	r.Log.Info("reconciler starting", "hostname", r.hostname, "interval", reconcileInterval.String())
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	// Reconcile once immediately at startup (lease recovery after a restart).
	r.reconcileAll(ctx)
	for {
		select {
		case <-ctx.Done():
			r.Log.Info("reconciler stopping")
			return
		case <-ticker.C:
			r.reconcileAll(ctx)
		}
	}
}

// Trigger reconciles a single job (called by POST /internal/provision).
func (r *Reconciler) Trigger(ctx context.Context, jobID string) {
	if err := r.reconcileJob(ctx, jobID); err != nil {
		r.Log.Error("trigger reconcile failed", "jobId", jobID, "error", err.Error())
	}
}

func (r *Reconciler) reconcileAll(ctx context.Context) {
	// 1. Fresh PENDING jobs (safety net when Graphile delivery is unavailable).
	if err := r.reconcilePending(ctx); err != nil {
		r.Log.Error("pending reconcile failed", "error", err.Error())
	}

	// 2. All PROVISIONING jobs — check VM status and transition to RUNNING.
	provisioning, err := r.store.ListByStatus(ctx, []store.JobStatus{store.StatusProvisioning}, false)
	if err != nil {
		r.Log.Error("failed to list provisioning jobs", "error", err.Error())
	} else {
		for _, job := range provisioning {
			if err := r.reconcileJob(ctx, job.ID); err != nil {
				r.Log.Error("provisioning reconcile failed", "jobId", job.ID, "error", err.Error())
			}
		}
	}

	// 3. All RUNNING jobs — monitor health, timeout, and VM power status.
	running, err := r.store.ListByStatus(ctx, []store.JobStatus{store.StatusRunning}, false)
	if err != nil {
		r.Log.Error("failed to list running jobs", "error", err.Error())
	} else {
		for _, job := range running {
			if err := r.reconcileJob(ctx, job.ID); err != nil {
				r.Log.Error("running job reconcile failed", "jobId", job.ID, "error", err.Error())
			}
		}
	}

	// 4. Terminal jobs — reclaim their VMs and delete them.
	terminal, err := r.store.ListByStatus(ctx, []store.JobStatus{store.StatusCompleted, store.StatusFailed}, false)
	if err != nil {
		r.Log.Error("failed to list terminal jobs", "error", err.Error())
	} else {
		for _, job := range terminal {
			if err := r.cleanup(ctx, job.ID); err != nil {
				r.Log.Error("cleanup failed", "jobId", job.ID, "error", err.Error())
			}
		}
	}

	// 4. Orphan VMs left behind by a crashed provisioner or the old operator.
	if err := r.orphanSweep(ctx); err != nil {
		r.Log.Error("orphan sweep failed", "error", err.Error())
	}
}

func (r *Reconciler) reconcilePending(ctx context.Context) error {
	pending, err := r.store.ListByStatus(ctx, []store.JobStatus{store.StatusPending}, false)
	if err != nil {
		return err
	}
	for _, job := range pending {
		if err := r.reconcileJob(ctx, job.ID); err != nil {
			r.Log.Error("pending job reconcile failed", "jobId", job.ID, "error", err.Error())
		}
	}
	return nil
}

// reconcileJob drives one job's desired state. It is idempotent: safe to run on
// every tick, whether the job is PENDING, being provisioned, or running.
func (r *Reconciler) reconcileJob(ctx context.Context, jobID string) error {
	job, err := r.store.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		r.Log.Warn("job not found", "jobId", jobID)
		return nil
	}

	switch job.Status {
	case store.StatusPending:
		return r.claimAndProvision(ctx, job)
	case store.StatusProvisioning:
		return r.reconcileProvisioning(ctx, job)
	case store.StatusRunning:
		return r.reconcileRunning(ctx, job)
	case store.StatusCompleted, store.StatusFailed:
		return r.cleanup(ctx, job.ID)
	default:
		return nil
	}
}

// claimAndProvision claims a PENDING job and starts provisioning the VM.
// A nil claim return means another instance already owns an active lease — no-op.
func (r *Reconciler) claimAndProvision(ctx context.Context, job *store.Job) error {
	claimed, err := r.store.ClaimPending(ctx, job.ID, r.hostname, leaseSeconds)
	if err != nil {
		return err
	}
	if claimed == nil {
		r.Log.Info("job already claimed by another lease", "jobId", job.ID)
		return nil
	}
	r.Log.Info("claimed pending job", "jobId", job.ID, "attempt", claimed.Attempts)
	return r.provision(ctx, claimed)
}

// provision creates the VM + attaches the model volume for a claimed job.
func (r *Reconciler) provision(ctx context.Context, job *store.Job) error {
	if job.WorkerID != nil && *job.WorkerID != "" {
		return nil // already created in a prior attempt
	}

	cloudInit, err := hetzner.GenerateCloudInit(job.Payload, job.ID)
	if err != nil {
		return r.fail(ctx, job, "failed to generate cloud-init: "+err.Error())
	}

	serverType := ""
	if job.Payload.ServerType != nil {
		serverType = *job.Payload.ServerType
	}
	server, err := r.hcloud.CreateServer(ctx, job.ID, cloudInit, serverType)
	if err != nil {
		// Per ADR-031 a recoverable provisioning failure is retried; permanent
		// hcloud errors (auth, bad server type) fail the job for good.
		if unrecoverableHcloudError(err) {
			return r.fail(ctx, job, "failed to create server: "+err.Error())
		}
		return err
	}

	var volumeID string
	if hetzner.RequiresModelVolume(job.Payload) {
		volumeID = os.Getenv("VOLUME_ID")
		if job.Payload.Storage != nil && job.Payload.Storage.ModelVolume != nil && *job.Payload.Storage.ModelVolume != "" {
			volumeID = *job.Payload.Storage.ModelVolume
		}
	}

	var patch = struct {
		Status   *store.JobStatus
		WorkerID *string
		WorkerIP *string
		VolumeID *string
		Error    *string
	}{
		WorkerID: &server.ID,
		WorkerIP: &server.IP,
	}
	if volumeID != "" {
		patch.VolumeID = &volumeID
	}
	if _, err := r.store.UpdateDetails(ctx, job.ID, patch); err != nil {
		r.Log.Error("failed to persist worker id", "jobId", job.ID, "error", err.Error())
		return err
	}

	if volumeID != "" {
		r.Log.Info("attaching volume", "jobId", job.ID, "volumeId", volumeID, "serverId", server.ID)
		if err := r.hcloud.AttachVolume(ctx, volumeID, server.ID); err != nil {
			r.Log.Error("failed to attach volume, continuing without it", "jobId", job.ID, "error", err.Error())
		}
	}

	r.Log.Info("provisioned server", "jobId", job.ID, "serverId", server.ID, "ip", server.IP)
	return nil
}

// reconcileProvisioning waits for the previously-created VM to reach "running".
func (r *Reconciler) reconcileProvisioning(ctx context.Context, job *store.Job) error {
	// Renew our lease if it is still ours so a healthy run is not stolen.
	if job.ClaimedBy != nil && *job.ClaimedBy == r.hostname {
		if _, err := r.store.RenewLease(ctx, job.ID, r.hostname, leaseSeconds); err != nil {
			return err
		}
	}

	if job.WorkerID == nil {
		// Lease was reclaimed but the VM was never created — drive provisioning now.
		return r.provision(ctx, job)
	}

	if r.provisioningTimedOut(job) {
		return r.fail(ctx, job, "VM did not become ready before the provisioning timeout")
	}

	server, err := r.hcloud.GetServer(ctx, *job.WorkerID)
	if err != nil {
		// Hetzner may return 404 for a half-deleted VM. Treat as transient once,
		// permanent when the VM has genuinely gone — the orphan sweep will catch it.
		return err
	}

	if server.Status == "off" || server.Status == "stopped" {
		r.Log.Warn("worker VM powered off during provisioning", "jobId", job.ID, "serverId", *job.WorkerID)
		return r.fail(ctx, job, "worker VM powered off during provisioning")
	}

	if server.Status == "running" {
		running := store.StatusRunning
		if _, err := r.store.UpdateDetails(ctx, job.ID, struct {
			Status   *store.JobStatus
			WorkerID *string
			WorkerIP *string
			VolumeID *string
			Error    *string
		}{Status: &running}); err != nil {
			return err
		}
		r.Log.Info("worker is running", "jobId", job.ID, "serverId", server.ID, "ip", server.IP)
	}
	return nil
}

// reconcileRunning monitors a live job for timeout and VM disappearance.
func (r *Reconciler) reconcileRunning(ctx context.Context, job *store.Job) error {
	if job.ClaimedBy != nil && *job.ClaimedBy == r.hostname {
		if _, err := r.store.RenewLease(ctx, job.ID, r.hostname, leaseSeconds); err != nil {
			return err
		}
	}

	// Global timeout anchored at first claim (started_at), never reset on reclaim.
	if job.StartedAt != nil && job.Payload.TimeoutSeconds != nil && *job.Payload.TimeoutSeconds > 0 {
		if time.Since(*job.StartedAt) > time.Duration(*job.Payload.TimeoutSeconds)*time.Second {
			// Include a grace window so a just-completed callback isn't raced.
			r.Log.Warn("job timed out", "jobId", job.ID, "elapsed", time.Since(*job.StartedAt).Round(time.Second).String())
			return r.fail(ctx, job, "job timed out")
		}
	}

	if job.WorkerID != nil && *job.WorkerID != "" {
		server, err := r.hcloud.GetServer(ctx, *job.WorkerID)
		if err != nil {
			if isGone(err) {
				return r.fail(ctx, job, "worker VM no longer reachable: "+err.Error())
			}
			r.Log.Warn("transient error getting server status, will retry", "jobId", job.ID, "serverId", *job.WorkerID, "error", err.Error())
			return err
		}
		if server.Status == "deleting" || server.Status == "deleted" || server.Status == "off" || server.Status == "stopped" {
			r.Log.Warn("worker VM powered off or deleted while job was running", "jobId", job.ID, "serverId", *job.WorkerID, "status", server.Status)
			return r.fail(ctx, job, "worker VM powered off or deleted while the job was running")
		}
	}
	return nil
}

func (r *Reconciler) provisioningTimedOut(job *store.Job) bool {
	if job.StartedAt == nil {
		return false
	}
	return time.Since(*job.StartedAt) > 15*time.Minute
}

func (r *Reconciler) fail(ctx context.Context, job *store.Job, reason string) error {
	r.Log.Warn("failing job", "jobId", job.ID, "reason", reason)
	if _, err := r.store.MarkTerminal(ctx, job.ID, store.StatusFailed, reason); err != nil {
		return err
	}
	return r.cleanup(ctx, job.ID)
}

// cleanup detaches the model volume and deletes the worker VM for a terminal job.
func (r *Reconciler) cleanup(ctx context.Context, jobID string) error {
	job, err := r.store.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}

	if job.WorkerID != nil && *job.WorkerID != "" {
		r.Log.Info("deleting worker VM", "jobId", job.ID, "serverId", *job.WorkerID)
		err := r.hcloud.DeleteServer(ctx, *job.WorkerID)
		if err == nil || isGone(err) {
			r.Log.Info("server deleted or already gone", "jobId", job.ID, "serverId", *job.WorkerID)
			_ = r.store.ClearWorkerID(ctx, job.ID)
		} else {
			r.Log.Error("failed to delete server", "jobId", job.ID, "serverId", *job.WorkerID, "error", err.Error())
		}
	}
	sharedVolumeID := os.Getenv("VOLUME_ID")
	if job.VolumeID != nil && *job.VolumeID != "" {
		if *job.VolumeID == sharedVolumeID {
			r.Log.Info("skipping volume detach for static shared model volume", "jobId", job.ID, "volumeId", *job.VolumeID)
		} else {
			r.Log.Info("detaching volume", "jobId", job.ID, "volumeId", *job.VolumeID)
			if err := r.hcloud.DetachVolume(ctx, *job.VolumeID); err != nil {
				if isGone(err) {
					r.Log.Info("volume already detached", "jobId", job.ID, "volumeId", *job.VolumeID)
				} else {
					r.Log.Error("failed to detach volume", "jobId", job.ID, "volumeId", *job.VolumeID, "error", err.Error())
				}
			}
		}
	}
	return nil
}

// orphanSweep deletes managed VMs that no job row references. This provides the
// final safety net if a provisioner crashed between CreateServer and the DB write,
// or VMs are left over from the old operator.
func (r *Reconciler) orphanSweep(ctx context.Context) error {
	managedIDs, err := r.store.ListWorkerIDs(ctx)
	if err != nil {
		return err
	}
	servers, err := r.hcloud.ListServers(ctx)
	if err != nil {
		return err
	}
	for _, srv := range servers {
		if srv.Status == "deleting" {
			continue
		}
		if managedIDs[srv.ID] {
			continue
		}
		r.Log.Warn("deleting orphan VM", "serverId", srv.ID, "name", srv.Name)
		if err := r.hcloud.DeleteServer(ctx, srv.ID); err != nil {
			r.Log.Error("failed to delete orphan VM", "serverId", srv.ID, "error", err.Error())
		}
	}
	return nil
}
