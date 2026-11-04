package argocdagent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// Config for the ArgoCD Agent provider.
type Config struct {
	// ArgoCD API endpoint (principal side)
	ArgoCDAPIURL   string
	ArgoCDAPIToken string

	// EventBus for publishing agent lifecycle events (16.10)
	EventBus interfaces.IEventBus

	// PrincipalEndpoint exposed to agents for mTLS connection (16.6)
	PrincipalEndpoint string
}

// Agent manages ArgoCD agent lifecycle, credentials, and status.
// It implements IArgoCDAgent using the ArgoCD REST API for status queries
// and in-memory state for credential management.
// Agent deployment is GitOps-driven via the Universal Tenant Helm Chart (16.2).
type Agent struct {
	cfg    Config
	mu     sync.RWMutex
	agents map[string]*agentRecord // agentID → record
}

type agentRecord struct {
	config    models.AgentConfig
	status    models.AgentStatus
	creds     map[string]string
	updatedAt time.Time
}

// NewAgent creates an ArgoCD Agent provider.
func NewAgent(cfg Config) *Agent {
	return &Agent{
		cfg:    cfg,
		agents: make(map[string]*agentRecord),
	}
}

// ─── Agent Lifecycle (16.9) ───────────────────────────────────────────────────

// DeployAgent registers an agent config and publishes opensbt_agentDeployed (16.10).
// Actual pod deployment is handled by the Helm chart (16.2); this records the
// config and fires the event so the Control Plane can track it.
func (a *Agent) DeployAgent(ctx context.Context, config models.AgentConfig) error {
	config.CreatedAt = time.Now().UTC()
	config.UpdatedAt = config.CreatedAt

	// Default mode based on tier (16.4, 16.5)
	if config.Mode == "" {
		config.Mode = modeForTier(config.Config)
	}

	a.mu.Lock()
	a.agents[config.AgentID] = &agentRecord{
		config: config,
		status: models.AgentStatus{
			AgentID:      config.AgentID,
			TenantID:     config.TenantID,
			Connected:    false,
			HealthStatus: "Unknown",
		},
		updatedAt: config.CreatedAt,
	}
	a.mu.Unlock()

	return a.publishEvent(ctx, models.EventAgentDeployed, map[string]interface{}{
		"agentId":  config.AgentID,
		"tenantId": config.TenantID,
		"mode":     config.Mode,
	})
}

// UpdateAgent updates an existing agent's config (16.9, 16.12).
func (a *Agent) UpdateAgent(ctx context.Context, agentID string, config models.AgentConfig) error {
	a.mu.Lock()
	rec, ok := a.agents[agentID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("argocdagent: agent %s not found", agentID)
	}
	config.UpdatedAt = time.Now().UTC()
	rec.config = config
	rec.updatedAt = config.UpdatedAt
	a.mu.Unlock()
	return nil
}

// RemoveAgent deregisters an agent (16.9).
func (a *Agent) RemoveAgent(_ context.Context, agentID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.agents, agentID)
	return nil
}

// ─── Agent Status (16.7) ──────────────────────────────────────────────────────

// GetAgentStatus returns the current status, refreshing from ArgoCD API if configured (16.7).
func (a *Agent) GetAgentStatus(ctx context.Context, agentID string) (*models.AgentStatus, error) {
	a.mu.RLock()
	rec, ok := a.agents[agentID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("argocdagent: agent %s not found", agentID)
	}

	// Refresh from ArgoCD API if available (16.7)
	if a.cfg.ArgoCDAPIURL != "" {
		if status, err := a.fetchArgoCDStatus(ctx, agentID); err == nil {
			a.mu.Lock()
			rec.status = *status
			a.mu.Unlock()
			return status, nil
		}
	}

	status := rec.status
	return &status, nil
}

// ListAgents returns all agents for a tenant (16.8 — filtered by tenantID).
func (a *Agent) ListAgents(_ context.Context, tenantID string) ([]models.AgentStatus, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var result []models.AgentStatus
	for _, rec := range a.agents {
		if tenantID == "" || rec.config.TenantID == tenantID {
			result = append(result, rec.status)
		}
	}
	return result, nil
}

// fetchArgoCDStatus queries the ArgoCD API for application health in the tenant namespace (16.7).
func (a *Agent) fetchArgoCDStatus(ctx context.Context, agentID string) (*models.AgentStatus, error) {
	a.mu.RLock()
	rec, ok := a.agents[agentID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("not found")
	}

	url := fmt.Sprintf("%s/api/v1/applications?appNamespace=tenant-%s", a.cfg.ArgoCDAPIURL, rec.config.TenantID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.ArgoCDAPIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Items []struct {
			Status struct {
				Health struct{ Status string } `json:"health"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	health := "Unknown"
	connected := false
	if len(result.Items) > 0 {
		health = result.Items[0].Status.Health.Status
		connected = true
	}

	return &models.AgentStatus{
		AgentID:       agentID,
		TenantID:      rec.config.TenantID,
		Connected:     connected,
		HealthStatus:  health,
		LastHeartbeat: time.Now().UTC(),
	}, nil
}

// ─── Agent Credentials — mTLS (16.3) ─────────────────────────────────────────

// GenerateAgentCredentials creates a self-signed mTLS client cert/key pair (16.3).
func (a *Agent) GenerateAgentCredentials(_ context.Context, agentID string) (map[string]string, error) {
	creds, err := generateMTLSCredentials(agentID)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if rec, ok := a.agents[agentID]; ok {
		rec.creds = creds
	}
	a.mu.Unlock()
	return creds, nil
}

// RotateAgentCredentials regenerates mTLS credentials (16.3).
func (a *Agent) RotateAgentCredentials(ctx context.Context, agentID string) (map[string]string, error) {
	return a.GenerateAgentCredentials(ctx, agentID)
}

// generateMTLSCredentials produces a self-signed ECDSA cert/key for agent mTLS (16.3).
func generateMTLSCredentials(agentID string) (map[string]string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("argocdagent: generate key: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "argocd-agent-" + agentID},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("argocdagent: create cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return map[string]string{
		"client_cert":        string(certPEM),
		"client_key":         string(keyPEM),
		"principal_endpoint": "",
	}, nil
}

// ─── Agent Configuration (16.12) ─────────────────────────────────────────────

// GetAgentConfig returns the stored config for an agent.
func (a *Agent) GetAgentConfig(_ context.Context, agentID string) (*models.AgentConfig, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	rec, ok := a.agents[agentID]
	if !ok {
		return nil, fmt.Errorf("argocdagent: agent %s not found", agentID)
	}
	cfg := rec.config
	return &cfg, nil
}

// UpdateAgentMode switches an agent between managed and autonomous modes (16.4, 16.5).
func (a *Agent) UpdateAgentMode(ctx context.Context, agentID string, mode string) error {
	if mode != "managed" && mode != "autonomous" {
		return fmt.Errorf("argocdagent: invalid mode %q (must be managed or autonomous)", mode)
	}
	a.mu.Lock()
	rec, ok := a.agents[agentID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("argocdagent: agent %s not found", agentID)
	}
	rec.config.Mode = mode
	rec.updatedAt = time.Now().UTC()
	a.mu.Unlock()
	return nil
}

// ─── Application Management (16.8) ───────────────────────────────────────────

// ListAgentApplications returns ArgoCD applications filtered to the agent's tenant namespace (16.8).
func (a *Agent) ListAgentApplications(ctx context.Context, agentID string) ([]map[string]interface{}, error) {
	a.mu.RLock()
	rec, ok := a.agents[agentID]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("argocdagent: agent %s not found", agentID)
	}

	if a.cfg.ArgoCDAPIURL == "" {
		return nil, nil
	}

	url := fmt.Sprintf("%s/api/v1/applications?appNamespace=tenant-%s", a.cfg.ArgoCDAPIURL, rec.config.TenantID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+a.cfg.ArgoCDAPIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// SyncAgentApplication triggers a sync for a specific application via ArgoCD API.
func (a *Agent) SyncAgentApplication(ctx context.Context, agentID string, appName string) error {
	if a.cfg.ArgoCDAPIURL == "" {
		return fmt.Errorf("argocdagent: ArgoCDAPIURL not configured")
	}
	url := fmt.Sprintf("%s/api/v1/applications/%s/sync", a.cfg.ArgoCDAPIURL, appName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+a.cfg.ArgoCDAPIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("argocdagent: sync %s: HTTP %d: %s", appName, resp.StatusCode, body)
	}
	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// modeForTier returns the agent mode based on tier stored in config (16.4, 16.5).
func modeForTier(config map[string]interface{}) string {
	if config == nil {
		return "managed"
	}
	tier, _ := config["tier"].(string)
	if tier == "premium" || tier == "enterprise" {
		return "autonomous"
	}
	return "managed"
}

// publishEvent publishes an agent lifecycle event if EventBus is configured (16.10).
func (a *Agent) publishEvent(ctx context.Context, detailType string, detail map[string]interface{}) error {
	if a.cfg.EventBus == nil {
		return nil
	}
	return a.cfg.EventBus.Publish(ctx, models.NewEvent(detailType, models.ApplicationPlaneEventSource, detail))
}

// Compile-time assertion
var _ interfaces.IArgoCDAgent = (*Agent)(nil)
