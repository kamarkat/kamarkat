package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// Provisioner implements interfaces.IProvisioner using GitOps + Helm.
// It commits tenant Helm values to Git; ArgoCD picks them up via the
// ApplicationSet Git Directory Generator (15.15, 15.16).
type Provisioner struct {
	cfg    Config
	git    *gitClient
	mu     sync.Mutex // guards warm pool in-memory state
	warmMu sync.Mutex
}

// NewProvisioner creates a GitOps Helm Provisioner.
func NewProvisioner(cfg Config) (*Provisioner, error) {
	cfg.defaults()
	git, err := newGitClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Provisioner{cfg: cfg, git: git}, nil
}

// ─── IProvisioner ─────────────────────────────────────────────────────────────

// ProvisionTenant commits tenant Helm values to Git (15.2, 15.5).
// For basic/standard tiers it first tries to claim a warm slot (15.11).
func (p *Provisioner) ProvisionTenant(ctx context.Context, req models.ProvisionRequest) (*models.ProvisionResult, error) {
	// Warm pool path for shared tiers
	if req.Tier == "basic" || req.Tier == "standard" {
		slot, err := p.ClaimWarmSlot(ctx, req.TenantID, req.Tier)
		if err == nil {
			return &models.ProvisionResult{
				TenantID:  req.TenantID,
				Status:    "provisioned",
				CreatedAt: time.Now().UTC(),
				Metadata:  map[string]string{"warm_slot": slot.SlotID},
			}, nil
		}
		// Fall through to direct provisioning if no warm slots available
	}

	path := fmt.Sprintf("%s/%s/values.yaml", p.cfg.TenantsDir, req.TenantID)
	content := generateHelmValues(req.TenantID, req.Tier, req.Email, nil)
	_, sha, _ := p.git.getFile(ctx, path)

	if err := p.git.putFile(ctx, path,
		fmt.Sprintf("chore: provision tenant %s (tier=%s)", req.TenantID, req.Tier),
		content, sha); err != nil {
		return nil, fmt.Errorf("gitops: commit tenant config: %w", err)
	}

	if err := p.TriggerSync(ctx, req.TenantID); err != nil {
		// Non-fatal — ArgoCD will poll within its interval
		_ = err
	}

	return &models.ProvisionResult{
		TenantID:  req.TenantID,
		Status:    "provisioned",
		CreatedAt: time.Now().UTC(),
	}, nil
}

// DeprovisionTenant removes the tenant folder from Git (15.3).
func (p *Provisioner) DeprovisionTenant(ctx context.Context, req models.DeprovisionRequest) (*models.DeprovisionResult, error) {
	path := fmt.Sprintf("%s/%s/values.yaml", p.cfg.TenantsDir, req.TenantID)
	_, sha, err := p.git.getFile(ctx, path)
	if err != nil {
		return nil, err
	}
	if sha == "" {
		// Already gone
		return &models.DeprovisionResult{TenantID: req.TenantID, Status: "not_found", DeletedAt: time.Now().UTC()}, nil
	}
	if err := p.git.deleteFile(ctx, path,
		fmt.Sprintf("chore: deprovision tenant %s", req.TenantID), sha); err != nil {
		return nil, err
	}
	return &models.DeprovisionResult{TenantID: req.TenantID, Status: "deprovisioned", DeletedAt: time.Now().UTC()}, nil
}

// UpdateTenantResources re-commits updated Helm values (tier change, activate, deactivate).
func (p *Provisioner) UpdateTenantResources(ctx context.Context, req models.UpdateRequest) (*models.UpdateResult, error) {
	path := fmt.Sprintf("%s/%s/values.yaml", p.cfg.TenantsDir, req.TenantID)
	_, sha, err := p.git.getFile(ctx, path)
	if err != nil {
		return nil, err
	}

	extra := map[string]interface{}{}
	if req.Action == "deactivate" {
		extra["suspended"] = true
	} else if req.Action == "activate" {
		extra["suspended"] = false
	}

	tier := req.Tier
	if tier == "" {
		tier = "basic" // fallback; real tier should always be set
	}

	content := generateHelmValues(req.TenantID, tier, "", extra)
	if err := p.git.putFile(ctx, path,
		fmt.Sprintf("chore: update tenant %s action=%s", req.TenantID, req.Action),
		content, sha); err != nil {
		return nil, err
	}
	_ = p.TriggerSync(ctx, req.TenantID)
	return &models.UpdateResult{TenantID: req.TenantID, Status: "updated", UpdatedAt: time.Now().UTC()}, nil
}

// GetProvisioningStatus reads tenant status from Storage (Status Controller Pattern).
// The Control Plane MUST NOT query ArgoCD API directly; it reads from the PostgreSQL
// cache which is updated by ArgoCD webhook events (opensbt_argoSyncCompleted).
func (p *Provisioner) GetProvisioningStatus(ctx context.Context, tenantID string) (*models.ProvisioningStatus, error) {
	if p.cfg.Storage == nil {
		return &models.ProvisioningStatus{TenantID: tenantID, Status: "unknown"}, nil
	}

	tenant, err := p.cfg.Storage.GetTenant(ctx, tenantID)
	if err != nil {
		return &models.ProvisioningStatus{TenantID: tenantID, Status: "not_found"}, nil
	}

	// Two vocabularies meet here and only one can win. ProvisioningStatus.Status
	// documents lower-case sync states (synced, degraded, progressing, ...) and
	// the sentinel returns above use them; TenantStatus is the upper-case
	// lifecycle machine (SYNCING, READY, FAILED). The conversion is explicit
	// rather than a mapping because the state machine is the source this
	// function is documented to report, and inventing a translation would put a
	// value here that no consumer was written against either.
	//
	// The missing conversion is why this package did not compile.
	return &models.ProvisioningStatus{
		TenantID:     tenantID,
		Status:       string(tenant.Status),
		LastSyncTime: tenant.UpdatedAt,
	}, nil
}

// ListTenantResources returns an empty list — resource enumeration is ArgoCD's domain.
func (p *Provisioner) ListTenantResources(_ context.Context, _ string) ([]models.Resource, error) {
	return nil, nil
}

// ─── Warm Pool (15.11, 15.12) ─────────────────────────────────────────────────

// ClaimWarmSlot atomically claims the first available warm slot for a tier.
func (p *Provisioner) ClaimWarmSlot(ctx context.Context, tenantID, tier string) (*models.WarmSlotResult, error) {
	p.warmMu.Lock()
	defer p.warmMu.Unlock()

	// Find first unassigned slot by checking Git
	for i := 1; i <= p.cfg.WarmPoolTarget; i++ {
		slotID := fmt.Sprintf("warm-%s-%02d", tier, i)
		path := fmt.Sprintf("%s/%s/values.yaml", p.cfg.TenantsDir, slotID)
		content, sha, err := p.git.getFile(ctx, path)
		if err != nil || sha == "" {
			continue
		}
		// Check if unassigned
		if bytes.Contains(content, []byte("assigned: false")) {
			// Reassign to tenant
			newContent := generateHelmValues(tenantID, tier, "", nil)
			if err := p.git.putFile(ctx, path,
				fmt.Sprintf("chore: claim warm slot %s for tenant %s", slotID, tenantID),
				newContent, sha); err != nil {
				return nil, err
			}
			_ = p.TriggerSync(ctx, slotID)
			return &models.WarmSlotResult{
				SlotID:    slotID,
				TenantID:  tenantID,
				Tier:      tier,
				ClaimedAt: time.Now().UTC(),
			}, nil
		}
	}
	return nil, fmt.Errorf("gitops: no warm slots available for tier %s", tier)
}

// RefillWarmPool creates warm slot entries in Git up to targetCount (15.11).
func (p *Provisioner) RefillWarmPool(ctx context.Context, tier string, targetCount int) error {
	for i := 1; i <= targetCount; i++ {
		slotID := fmt.Sprintf("warm-%s-%02d", tier, i)
		path := fmt.Sprintf("%s/%s/values.yaml", p.cfg.TenantsDir, slotID)
		_, sha, _ := p.git.getFile(ctx, path)
		if sha != "" {
			continue // already exists
		}
		content := generateWarmSlotValues(slotID, tier)
		if err := p.git.putFile(ctx, path,
			fmt.Sprintf("chore: add warm slot %s", slotID),
			content, ""); err != nil {
			return err
		}
	}
	return nil
}

// GetWarmPoolStatus counts available warm slots for a tier (15.12).
func (p *Provisioner) GetWarmPoolStatus(ctx context.Context, tier string) (*models.WarmPoolStatus, error) {
	available := 0
	total := 0
	for i := 1; i <= p.cfg.WarmPoolTarget; i++ {
		slotID := fmt.Sprintf("warm-%s-%02d", tier, i)
		path := fmt.Sprintf("%s/%s/values.yaml", p.cfg.TenantsDir, slotID)
		content, sha, _ := p.git.getFile(ctx, path)
		if sha == "" {
			continue
		}
		total++
		if bytes.Contains(content, []byte("assigned: false")) {
			available++
		}
	}
	return &models.WarmPoolStatus{
		Tier:           tier,
		AvailableSlots: available,
		TotalSlots:     total,
		TargetSlots:    p.cfg.WarmPoolTarget,
		LastRefill:     time.Now().UTC(),
	}, nil
}

// ─── GitOps Integration (15.2, 15.3, 15.13) ──────────────────────────────────

// CommitTenantConfig writes a TenantConfig as Helm values to Git (15.2).
func (p *Provisioner) CommitTenantConfig(ctx context.Context, tenantID string, config models.TenantConfig) error {
	path := fmt.Sprintf("%s/%s/values.yaml", p.cfg.TenantsDir, tenantID)
	_, sha, _ := p.git.getFile(ctx, path)
	content := generateHelmValues(tenantID, config.Tier, "", config.HelmValues)
	return p.git.putFile(ctx, path,
		fmt.Sprintf("chore: update config for tenant %s", tenantID),
		content, sha)
}

// RollbackTenantConfig is a no-op stub — Git history provides rollback via
// the ArgoCD UI or direct git revert. Full implementation requires GitHub
// Revert API which is out of scope for the toolkit layer.
func (p *Provisioner) RollbackTenantConfig(_ context.Context, tenantID, commitHash string) error {
	return fmt.Errorf("gitops: rollback not implemented; revert commit %s manually for tenant %s", commitHash, tenantID)
}

// GetGitRepository returns the repository reference for a tenant (15.2).
func (p *Provisioner) GetGitRepository(_ context.Context, tenantID string) (*models.GitRepository, error) {
	return &models.GitRepository{
		URL:    p.cfg.RepoURL,
		Branch: p.cfg.Branch,
		Path:   fmt.Sprintf("%s/%s", p.cfg.TenantsDir, tenantID),
	}, nil
}

// TriggerSync fires an ArgoCD webhook to trigger immediate sync (15.13).
func (p *Provisioner) TriggerSync(ctx context.Context, tenantID string) error {
	if p.cfg.ArgoCDWebhookURL != "" {
		return p.TriggerWebhookSync(ctx, tenantID, p.cfg.ArgoCDWebhookURL)
	}
	if p.cfg.ArgoCDAPIURL != "" {
		return p.triggerAPISync(ctx, tenantID)
	}
	return nil
}

// TriggerWebhookSync sends a GitHub-style push webhook to ArgoCD (15.13).
func (p *Provisioner) TriggerWebhookSync(ctx context.Context, _ string, webhookURL string) error {
	payload := map[string]interface{}{
		"ref":        "refs/heads/" + p.cfg.Branch,
		"repository": map[string]string{"html_url": p.cfg.RepoURL},
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gitops: webhook sync: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// triggerAPISync calls ArgoCD API to sync a specific application (15.13).
func (p *Provisioner) triggerAPISync(ctx context.Context, tenantID string) error {
	url := fmt.Sprintf("%s/api/v1/applications/tenant-%s/sync", p.cfg.ArgoCDAPIURL, tenantID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+p.cfg.ArgoCDAPIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// Compile-time assertion
var _ interfaces.IProvisioner = (*Provisioner)(nil)
