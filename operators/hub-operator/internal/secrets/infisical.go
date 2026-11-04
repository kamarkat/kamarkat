package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/constant"
)

const (
	// Enterprise Alignment: ADR-003 / ADR-031 Isolation Boundaries
	InfisicalSharedPathFormat = "/spoke-pool/%s/shared"
	// Takes clusterName and APP id. The segment named `tenants` holds a product and
	// always has -- before ADR-088 the field feeding it was called tenantId but
	// held one. Correcting the NAME is deferred: moving live key material is a
	// data migration, and bundling it with an identifier rename made the
	// riskiest step serve the least valuable part of the change.
	InfisicalTenantPathFormat = "/spoke-pool/%s/tenants/%s"

	// credentialRotationBackoff is the minimum interval between self-healing
	// rotations of stale shared credentials per cluster. A persistently-invalid
	// credential must not trigger an API rotation on every reconcile.
	credentialRotationBackoff = 15 * time.Minute
)

// InfisicalClient is a lightweight HTTP client for ADR-031 topology management.
// Uses Machine Identity credentials (client-id + client-secret) for folder/secret
// operations.
//
// EnvironmentSlug has no default. It used to be "dev", which made every caller
// that forgot to set it write into the development environment of whatever box
// it was running on -- and a hub manages spokes whose environments differ from
// its own, so one process-wide value cannot address them all. A client is
// obtained per environment through ForEnvironment.
type InfisicalClient struct {
	BaseURL         string
	ClientID        string
	ClientSecret    string
	ProjectID       string
	OrganizationID  string
	EnvironmentSlug string

	httpClient *http.Client
	token      string
	tokenExp   time.Time

	// mu guards lastRotationAttempt and derived. Used to rate-limit self-healing
	// credential rotations so a persistently-invalid credential does not hammer
	// Infisical.
	mu                  sync.Mutex
	lastRotationAttempt map[string]time.Time

	// derived holds one client per Infisical environment other than this one's,
	// built by ForEnvironment. Cached rather than built per call because each
	// carries its own rotation backoff: a fresh client every reconcile would have
	// an empty lastRotationAttempt and so would rotate a stale credential on every
	// pass, which is the behaviour credentialRotationBackoff exists to prevent.
	derived map[string]*InfisicalClient
}

// NewInfisicalClient creates a new InfisicalClient from env-provided credentials.
func NewInfisicalClient(baseURL, clientID, clientSecret, projectID, organizationID string) *InfisicalClient {
	if baseURL == "" {
		baseURL = "http://infisical-standalone-infisical.platform-security.svc:8080"
	}
	return &InfisicalClient{
		BaseURL:             baseURL,
		ClientID:            clientID,
		ClientSecret:        clientSecret,
		ProjectID:           projectID,
		OrganizationID:      organizationID,
		lastRotationAttempt: make(map[string]time.Time),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ForEnvironment returns the client for one Infisical environment.
//
// The hub writes three kinds of secret and they do not share an environment: its
// own (the management scope, from hub-bootstrap-config), a workload cluster's
// (the environment that cluster declares), and an application's (the environment
// the tenant deploys it to). A spoke's credentials written into the hub's
// environment are read by nothing -- the spoke's ClusterSecretStore is patched
// with the SPOKE's slug (03-platform-services-appset.yaml), so the write and the
// read land in different places and the ExternalSecret simply never syncs.
//
// Returns the receiver when the slug already matches, so the common case adds
// nothing. A new client is NOT a struct copy: this type holds a mutex, and
// copying it is both a vet error and a way to share rotation state unsafely.
func (c *InfisicalClient) ForEnvironment(slug string) (*InfisicalClient, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("no Infisical environment named for this target: " +
			"refusing to fall back to the hub's own environment, which is where a " +
			"credential goes to be unreadable by the cluster that needs it")
	}
	if slug == c.EnvironmentSlug {
		return c, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.derived[slug]; ok {
		return existing, nil
	}
	scoped := NewInfisicalClient(c.BaseURL, c.ClientID, c.ClientSecret, c.ProjectID, c.OrganizationID)
	scoped.EnvironmentSlug = slug
	if c.derived == nil {
		c.derived = make(map[string]*InfisicalClient)
	}
	c.derived[slug] = scoped
	return scoped, nil
}

// authenticate performs Universal Auth login to get an access token.
func (c *InfisicalClient) authenticate(ctx context.Context) error {
	loginReq := map[string]string{
		"clientId":     c.ClientID,
		"clientSecret": c.ClientSecret,
	}

	body, err := json.Marshal(loginReq)
	if err != nil {
		return fmt.Errorf("failed to marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+constant.APIEndpointUniversalAuthLogin, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("infisical authentication failed with status %d (transient)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("authentication failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var loginResp struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return fmt.Errorf("failed to decode login response: %w", err)
	}

	c.token = loginResp.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(loginResp.ExpiresIn) * time.Second)
	return nil
}

// ensureAuthenticated checks token validity and refreshes if needed.
func (c *InfisicalClient) ensureAuthenticated(ctx context.Context) error {
	if c.token == "" || time.Now().Add(5*time.Minute).After(c.tokenExp) {
		return c.authenticate(ctx)
	}
	return nil
}

// validateClientCredentials verifies a Machine Identity's Universal Auth
// credentials by attempting a login. It returns nil when the credentials are
// valid, and an error otherwise. Invalid credentials (HTTP 401) return
// ErrInvalidCredentials so callers can distinguish "needs rotation" from a
// transient failure.
var ErrInvalidCredentials = errors.New("invalid Universal Auth credentials")

func (c *InfisicalClient) validateClientCredentials(ctx context.Context, clientID, clientSecret string) error {
	loginReq := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
	}

	body, err := json.Marshal(loginReq)
	if err != nil {
		return fmt.Errorf("failed to marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+constant.APIEndpointUniversalAuthLogin, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute login request: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrInvalidCredentials
	case resp.StatusCode >= 500:
		return fmt.Errorf("credential validation failed with status %d (transient)", resp.StatusCode)
	default:
		return fmt.Errorf("credential validation failed with status %d", resp.StatusCode)
	}
}

// rotationBackoffElapsed reports whether enough time has passed since the last
// self-healing rotation for this cluster to attempt another one. This prevents a
// persistently-invalid credential from hammering the Infisical API.
func (c *InfisicalClient) rotationBackoffElapsed(clusterName string, backoff time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.lastRotationAttempt[clusterName]
	if !ok {
		return true
	}
	return time.Since(last) >= backoff
}

// recordRotationAttempt timestamps a self-healing rotation for backoff purposes.
func (c *InfisicalClient) recordRotationAttempt(clusterName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRotationAttempt[clusterName] = time.Now()
}

// newAuthenticatedRequest creates an HTTP request with the auth bearer token set.
func (c *InfisicalClient) newAuthenticatedRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	if err := c.ensureAuthenticated(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// ============================================================================
// FOLDER OPERATIONS
// ============================================================================

// EnsureFolder creates a folder hierarchy in Infisical.
// Multi-segment paths (e.g. /spoke-pool/<clusterName>/tenants/<tenantId>) are
// created one segment at a time. Idempotent: 400 "already exists" is treated
// as success.
func (c *InfisicalClient) EnsureFolder(ctx context.Context, folderPath string) error {
	if folderPath == "" || folderPath == "/" {
		return nil
	}

	// Build the hierarchy from root to leaf
	segments := strings.Split(strings.Trim(folderPath, "/"), "/")
	accumulated := ""
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		accumulated = path.Join(accumulated, segment)
		parentPath := path.Dir(accumulated)
		if parentPath == "." {
			parentPath = "/"
		}

		folderReq := map[string]interface{}{
			"projectId":   c.ProjectID,
			"environment": c.EnvironmentSlug,
			"path":        parentPath,
			"name":        segment,
		}
		body, err := json.Marshal(folderReq)
		if err != nil {
			return fmt.Errorf("failed to marshal folder request for %s: %w", segment, err)
		}

		req, err := c.newAuthenticatedRequest(ctx, "POST", c.BaseURL+constant.APIEndpointFolders, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create request for folder %s: %w", segment, err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to execute folder request for %s: %w", segment, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusConflict || (resp.StatusCode == http.StatusBadRequest && bytes.Contains(respBody, []byte("already exists"))) {
			// Idempotent: folder already exists
			continue
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("failed to create folder %s with status %d (transient)", segment, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("failed to create folder %s with status %d: %s", segment, resp.StatusCode, string(respBody))
		}
	}

	return nil
}

// ============================================================================
// SECRET OPERATIONS
// ============================================================================

// GetSecret retrieves a secret's value from Infisical.
// Returns the secret value and nil on success.
// Returns an error if the secret doesn't exist or can't be fetched.
func (c *InfisicalClient) GetSecret(ctx context.Context, secretPath, secretName string) (string, error) {
	url := fmt.Sprintf("%s%s?workspaceId=%s&environment=%s&secretPath=%s",
		c.BaseURL, fmt.Sprintf(constant.APIEndpointSecretsRawKey, secretName), c.ProjectID, c.EnvironmentSlug, secretPath)

	req, err := c.newAuthenticatedRequest(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to get secret %s at %s: %w", secretName, secretPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("secret %s not found at path %s", secretName, secretPath)
	}
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("get secret %s at %s returned status %d (transient)", secretName, secretPath, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get secret %s at %s returned status %d: %s", secretName, secretPath, resp.StatusCode, string(respBody))
	}

	var secretResp struct {
		Secret struct {
			SecretValue string `json:"secretValue"`
		} `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&secretResp); err != nil {
		return "", fmt.Errorf("failed to decode secret %s at %s: %w", secretName, secretPath, err)
	}

	return secretResp.Secret.SecretValue, nil
}

// SecretExists checks if a secret exists in Infisical.
func (c *InfisicalClient) SecretExists(ctx context.Context, secretPath, secretName string) (bool, error) {
	url := fmt.Sprintf("%s%s?workspaceId=%s&environment=%s&secretPath=%s",
		c.BaseURL, fmt.Sprintf(constant.APIEndpointSecretsRawKey, secretName), c.ProjectID, c.EnvironmentSlug, secretPath)

	req, err := c.newAuthenticatedRequest(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to check secret %s at %s: %w", secretName, secretPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 500 {
		return false, fmt.Errorf("check secret %s at %s returned status %d (transient)", secretName, secretPath, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("check secret %s at %s returned unexpected status %d: %s", secretName, secretPath, resp.StatusCode, string(respBody))
}

// CreateSecret creates a new secret in Infisical.
// Returns an error if the secret already exists (use UpdateSecret instead).
func (c *InfisicalClient) CreateSecret(ctx context.Context, secretPath, secretName, secretValue string) error {
	createReq := map[string]interface{}{
		"workspaceId": c.ProjectID,
		"environment": c.EnvironmentSlug,
		"secretPath":  secretPath,
		"secretName":  secretName,
		"secretValue": secretValue,
		"type":        "shared",
	}

	body, err := json.Marshal(createReq)
	if err != nil {
		return fmt.Errorf("failed to marshal create secret request: %w", err)
	}

	req, err := c.newAuthenticatedRequest(ctx, "POST", c.BaseURL+fmt.Sprintf(constant.APIEndpointSecretsRawKey, secretName), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create secret %s at %s: %w", secretName, secretPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("create secret %s at %s returned status %d (transient)", secretName, secretPath, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create secret %s at %s returned status %d: %s", secretName, secretPath, resp.StatusCode, string(respBody))
	}

	return nil
}

// UpdateSecret updates an existing secret in Infisical.
// Returns an error if the secret doesn't exist (use CreateSecret instead).
func (c *InfisicalClient) UpdateSecret(ctx context.Context, secretPath, secretName, secretValue string) error {
	updateReq := map[string]interface{}{
		"workspaceId": c.ProjectID,
		"environment": c.EnvironmentSlug,
		"secretPath":  secretPath,
		"secretValue": secretValue,
		"type":        "shared",
	}

	body, err := json.Marshal(updateReq)
	if err != nil {
		return fmt.Errorf("failed to marshal update secret request: %w", err)
	}

	req, err := c.newAuthenticatedRequest(ctx, "PATCH", c.BaseURL+fmt.Sprintf(constant.APIEndpointSecretsRawKey, secretName), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to update secret %s at %s: %w", secretName, secretPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("secret %s not found at path %s (use CreateSecret)", secretName, secretPath)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("update secret %s at %s returned status %d (transient)", secretName, secretPath, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update secret %s at %s returned status %d: %s", secretName, secretPath, resp.StatusCode, string(respBody))
	}

	return nil
}

// DeleteSecret removes a secret from Infisical. Removing one that does not exist
// is success: the caller asked for its absence, and absence is what it has.
func (c *InfisicalClient) DeleteSecret(ctx context.Context, secretPath, secretName string) error {
	body, err := json.Marshal(map[string]interface{}{
		"workspaceId": c.ProjectID,
		"environment": c.EnvironmentSlug,
		"secretPath":  secretPath,
		"type":        "shared",
	})
	if err != nil {
		return fmt.Errorf("failed to marshal delete secret request: %w", err)
	}

	req, err := c.newAuthenticatedRequest(ctx, "DELETE", c.BaseURL+fmt.Sprintf(constant.APIEndpointSecretsRawKey, secretName), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete secret %s at %s: %w", secretName, secretPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete secret %s at %s returned status %d: %s", secretName, secretPath, resp.StatusCode, string(respBody))
	}
	return nil
}

// ============================================================================
// MACHINE IDENTITY API OPERATIONS
// ============================================================================

// getOrganizationID returns the pre-configured OrganizationID for this InfisicalClient.
// Machine Identity access tokens do not include orgId in their JWT claims, so we
// rely on the bootstrap-stored org ID (sourced from INFISICAL_ORGANIZATION_ID env var).
func (c *InfisicalClient) getOrganizationID(ctx context.Context) (string, error) {
	if c.OrganizationID != "" {
		return c.OrganizationID, nil
	}
	return "", fmt.Errorf("OrganizationID not configured — set INFISICAL_ORGANIZATION_ID env var")
}

// createMachineIdentityInInfisical creates a Machine Identity in Infisical
// via POST /api/v1/identities and returns the new identity's UUID.
func (c *InfisicalClient) createMachineIdentityInInfisical(ctx context.Context, name, orgID string) (string, error) {
	payload := map[string]string{
		"name":           name,
		"organizationId": orgID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal identity request: %w", err)
	}

	req, err := c.newAuthenticatedRequest(ctx, "POST", c.BaseURL+constant.APIEndpointIdentities, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create identity: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create identity failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var identityResp struct {
		Identity struct {
			ID string `json:"id"`
		} `json:"identity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&identityResp); err != nil {
		return "", fmt.Errorf("failed to decode identity response: %w", err)
	}

	return identityResp.Identity.ID, nil
}

// attachUniversalAuth attaches Universal Auth to a Machine Identity via
// POST /api/v1/auth/universal-auth/identities/{identityId}.
// Idempotent: if UA is already configured, returns nil (no error).
func (c *InfisicalClient) attachUniversalAuth(ctx context.Context, identityID string) error {
	payload := map[string]interface{}{
		"clientSecretTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTTL":             2592000,
		"accessTokenMaxTTL":          2592000,
		"accessTokenNumUsesLimit":    0,
		"lockoutEnabled":             true,
		"lockoutThreshold":           3,
		"lockoutDurationSeconds":     300,
		"lockoutCounterResetSeconds": 30,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal attach auth request: %w", err)
	}

	req, err := c.newAuthenticatedRequest(ctx, "POST", c.BaseURL+fmt.Sprintf(constant.APIEndpointUniversalAuthIdentities, identityID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to attach universal auth: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Idempotent: UA already configured for this identity
	if resp.StatusCode == http.StatusBadRequest && bytes.Contains(respBody, []byte("already configured")) {
		return nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("attach universal auth failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// getClientIDFromUniversalAuth retrieves the clientId from the Universal Auth
// configuration via GET /api/v1/auth/universal-auth/identities/{identityId}.
func (c *InfisicalClient) getClientIDFromUniversalAuth(ctx context.Context, identityID string) (string, error) {
	req, err := c.newAuthenticatedRequest(ctx, "GET", c.BaseURL+fmt.Sprintf(constant.APIEndpointUniversalAuthIdentities, identityID), nil)
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to get universal auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get universal auth failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var uaResp struct {
		IdentityUniversalAuth struct {
			ClientID string `json:"clientId"`
		} `json:"identityUniversalAuth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&uaResp); err != nil {
		return "", fmt.Errorf("failed to decode universal auth response: %w", err)
	}

	return uaResp.IdentityUniversalAuth.ClientID, nil
}

// generateClientSecret generates a client secret for a Machine Identity via
// POST /api/v1/auth/universal-auth/identities/{identityId}/client-secrets.
// Returns the plaintext secret (shown only once by the API).
func (c *InfisicalClient) generateClientSecret(ctx context.Context, identityID string) (string, error) {
	payload := map[string]interface{}{
		"description":  "",
		"numUsesLimit": 0,
		"ttl":          0,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal client secret request: %w", err)
	}

	req, err := c.newAuthenticatedRequest(ctx, "POST", c.BaseURL+fmt.Sprintf(constant.APIEndpointUniversalAuthClientSecrets, identityID), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to generate client secret: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("generate client secret failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var credsResp struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&credsResp); err != nil {
		return "", fmt.Errorf("failed to decode client secret response: %w", err)
	}

	return credsResp.ClientSecret, nil
}

// findIdentityByName looks up a Machine Identity by name within the organization.
// Returns the identity ID if found, or empty string and nil if not found.
func (c *InfisicalClient) findIdentityByName(ctx context.Context, name, orgID string) (string, error) {
	url := fmt.Sprintf("%s%s?orgId=%s&limit=100", c.BaseURL, constant.APIEndpointIdentities, orgID)
	req, err := c.newAuthenticatedRequest(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to list identities: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("list identities failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var listResp struct {
		Identities []struct {
			Identity struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"identity"`
		} `json:"identities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return "", fmt.Errorf("failed to decode identities list: %w", err)
	}

	for _, item := range listResp.Identities {
		if item.Identity.Name == name {
			return item.Identity.ID, nil
		}
	}

	return "", nil
}

// grantProjectAccess grants a Machine Identity access to the hub-platform
// project via POST /api/v1/projects/{projectId}/memberships/identities/{identityId}.
// Idempotent: if the identity already has project access, returns nil.
func (c *InfisicalClient) grantProjectAccess(ctx context.Context, identityID, role string) error {
	payload := map[string]string{
		"role": role,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal grant access request: %w", err)
	}

	url := c.BaseURL + fmt.Sprintf(constant.APIEndpointProjectMemberships, c.ProjectID, identityID)
	req, err := c.newAuthenticatedRequest(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to grant project access: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Idempotent: identity might already have project access
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusConflict {
		if bytes.Contains(respBody, []byte("already")) || bytes.Contains(respBody, []byte("exists")) {
			return nil
		}
		return fmt.Errorf("grant project access failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("grant project access failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// ============================================================================
// ADR-031 BUSINESS LOGIC
// ============================================================================

// EnsureResult describes the outcome of an Ensure* operation for status condition reporting.
type EnsureResult int

const (
	EnsureCreated EnsureResult = iota
	EnsureAlreadyExists
	EnsureMissing
	// EnsureRotated indicates stored credentials were found to be invalid
	// (401) and were rotationally self-healed by the operator.
	EnsureRotated
	// EnsureSkipped indicates the credential was not applicable — the feature
	// that needs it is not enabled for this tenant. Distinct from
	// EnsureAlreadyExists, which asserts the credential is present: a caller
	// recording "seeded" must not do so for a credential nobody asked for.
	//
	// Appended last on purpose: these are iota values, and inserting above would
	// silently renumber every existing outcome.
	EnsureSkipped
)

// EnsureInfisicalCredentialsResult is the outcome of EnsureInfisicalCredentials.
type EnsureInfisicalCredentialsResult struct {
	Result EnsureResult
}

// EnsureTenantCredentialsResult is the outcome of EnsureTenantFolderAndCredentials.
type EnsureTenantCredentialsResult struct {
	Result                EnsureResult
	InfisicalCredsOutcome EnsureResult
	OAuthOutcome          EnsureResult
	// CacheOutcome is reported separately so the controller can record that THIS
	// credential has been seeded. Folding it into Result made the cache
	// indistinguishable from the tenant's other credentials, which is what let a
	// per-tenant marker stand in for a per-credential one.
	CacheOutcome EnsureResult
}

// OAuthClient is a declared OAuth client, reduced to what the credential producer
// needs (ADR-053). Grant types, scopes and redirect URIs are absent because they
// belong to the registration controller and never reach this boundary.
type OAuthClient struct {
	// Name is unique within the tenant. The client is registered under
	// "<tenantId>-<Name>".
	Name string

	// Confidential is true when the client authenticates with a secret. A public
	// client has no credential to generate, store or rotate.
	Confidential bool

	// PreviouslyProvisioned records that this client's credential has been
	// generated before, taken from the tenant resource's observed status. It
	// governs whether a missing credential is generated or reported as a fault,
	// and is per client rather than per tenant: a client declared today on an
	// existing tenant is new, not lost.
	PreviouslyProvisioned bool
}

// EnsureInfisicalCredentials provisions a REAL Machine Identity in Infisical
// and stores its credentials in the shared path.
//
// Per ADR-031 this replaces the previous fake credential generation with actual
// Infisical Machine Identity API calls so that the tenant SecretStore can
// authenticate (the old fake hex strings had no corresponding Machine Identity
// registered in Infisical, causing "Invalid credentials" / 401 on login).
//
// Idempotency contract (ADR-003):
//   - If infisical-credentials exist in shared path → AlreadyExists.
//   - If missing AND isFirstTime → look for existing identity by name; if found
//     reuse it, otherwise create a new MI. Then attach Universal Auth, generate
//     client secret, grant project viewer access, upload creds, return Created.
//   - If missing AND !isFirstTime → return Missing (manual intervention required).
func (c *InfisicalClient) EnsureInfisicalCredentials(ctx context.Context, clusterName string, isFirstTime bool) (*EnsureInfisicalCredentialsResult, error) {
	logger := log.FromContext(ctx)
	sharedPath := fmt.Sprintf(InfisicalSharedPathFormat, clusterName)

	logger.Info("Ensuring Infisical folder hierarchy", "path", sharedPath)
	if err := c.EnsureFolder(ctx, sharedPath); err != nil {
		logger.Error(err, "Failed to create shared folder", "path", sharedPath)
		return nil, fmt.Errorf("failed to create shared folder: %w", err)
	}

	// Idempotency check with self-healing validation. Credentials can become
	// valid at provisioning time and silently stale later (e.g. a rotated client
	// secret that was never persisted). Existence alone is not validity, so we
	// validate the stored credentials and rotate them when they no longer
	// authenticate — treating this as platform hardening (lifecycle self-heal).
	if storedValue, err := c.GetSecret(ctx, sharedPath, "infisical-credentials"); err == nil {
		stored := struct {
			ClientID     string `json:"client-id"`
			ClientSecret string `json:"client-secret"`
			ProjectID    string `json:"project-id"`
		}{}
		if jsonErr := json.Unmarshal([]byte(storedValue), &stored); jsonErr != nil {
			logger.Info("Stored infisical-credentials are malformed; will reprovision", "path", sharedPath)
			// Fall through to the provisioning path below.
		} else if stored.ClientID == "" || stored.ClientSecret == "" {
			logger.Info("Stored infisical-credentials are empty; will reprovision", "path", sharedPath)
		} else {
			validationErr := c.validateClientCredentials(ctx, stored.ClientID, stored.ClientSecret)
			if validationErr == nil {
				setCredentialStatus(clusterName, metricCredentialStatusValid)
				logger.Info("Machine Identity credentials already exist and are valid", "cluster", clusterName, "path", sharedPath)
				return &EnsureInfisicalCredentialsResult{Result: EnsureAlreadyExists}, nil
			}

			if !errors.Is(validationErr, ErrInvalidCredentials) {
				// Transient (5xx) or unexpected status — do not rotate; retry later.
				setCredentialStatus(clusterName, metricCredentialStatusFailed)
				return nil, fmt.Errorf("failed to validate existing infisical-credentials: %w", validationErr)
			}

			// Credentials are stale/invalid: rotate the shared identity client
			// secret and update the stored value atomically, subject to backoff.
			if !c.rotationBackoffElapsed(clusterName, credentialRotationBackoff) {
				setCredentialStatus(clusterName, metricCredentialStatusFailed)
				logger.Info("infisical-credentials are invalid but rotation is in backoff; skipping this reconcile",
					"cluster", clusterName, "path", sharedPath)
				return &EnsureInfisicalCredentialsResult{Result: EnsureAlreadyExists}, nil
			}

			if err := c.rotateSharedIdentityCredentials(ctx, clusterName, sharedPath, stored.ProjectID); err != nil {
				setCredentialStatus(clusterName, metricCredentialStatusFailed)
				logger.Error(err, "Failed to self-heal stale infisical-credentials", "cluster", clusterName, "path", sharedPath)
				return nil, fmt.Errorf("failed to self-heal stale infisical-credentials: %w", err)
			}

			setCredentialStatus(clusterName, metricCredentialStatusRotated)
			incCredentialRotation(clusterName)
			logger.Info("Self-healed stale infisical-credentials by rotation", "cluster", clusterName, "path", sharedPath)
			return &EnsureInfisicalCredentialsResult{Result: EnsureRotated}, nil
		}
	}

	if !isFirstTime {
		setCredentialStatus(clusterName, metricCredentialStatusMissing)
		logger.Error(nil, "CRITICAL: infisical-credentials missing from Infisical but SpokePool was already provisioned. Manual intervention required.",
			"cluster", clusterName, "path", sharedPath)
		return &EnsureInfisicalCredentialsResult{Result: EnsureMissing},
			fmt.Errorf("infisical-credentials missing from Infisical for already-provisioned SpokePool %s — manual recovery required", clusterName)
	}

	// Provision a REAL Machine Identity via Infisical API
	identityName := fmt.Sprintf("spoke-pool-%s", clusterName)

	// Step 1: Resolve the organization ID from the current auth context
	orgID, err := c.getOrganizationID(ctx)
	if err != nil {
		logger.Error(err, "Failed to get organization ID from auth context", "cluster", clusterName)
		return nil, fmt.Errorf("failed to get organization ID: %w", err)
	}

	// Step 2: Look for an existing identity with this name (idempotent recovery)
	identityID, err := c.findIdentityByName(ctx, identityName, orgID)
	if err != nil {
		logger.Error(err, "Failed to search for existing Machine Identity", "cluster", clusterName, "identity", identityName)
		return nil, fmt.Errorf("failed to search for Machine Identity %s: %w", identityName, err)
	}

	if identityID != "" {
		logger.Info("Found existing Machine Identity, reusing", "cluster", clusterName, "identityID", identityID, "identity", identityName)
	} else {
		// Step 2a: Create a new Machine Identity
		identityID, err = c.createMachineIdentityInInfisical(ctx, identityName, orgID)
		if err != nil {
			logger.Error(err, "Failed to create Machine Identity in Infisical", "cluster", clusterName, "identity", identityName)
			return nil, fmt.Errorf("failed to create Machine Identity %s: %w", identityName, err)
		}
		logger.Info("Machine Identity created in Infisical", "cluster", clusterName, "identityID", identityID)
	}

	// Step 3: Attach Universal Auth
	if err := c.attachUniversalAuth(ctx, identityID); err != nil {
		logger.Error(err, "Failed to attach Universal Auth to Machine Identity", "cluster", clusterName, "identityID", identityID)
		return nil, fmt.Errorf("failed to attach Universal Auth to identity %s: %w", identityID, err)
	}
	logger.Info("Universal Auth attached to Machine Identity", "cluster", clusterName, "identityID", identityID)

	// Step 4: Retrieve the clientId from the UA configuration
	clientID, err := c.getClientIDFromUniversalAuth(ctx, identityID)
	if err != nil {
		logger.Error(err, "Failed to get clientId from Universal Auth", "cluster", clusterName, "identityID", identityID)
		return nil, fmt.Errorf("failed to get clientId for identity %s: %w", identityID, err)
	}
	logger.Info("Retrieved clientId for Machine Identity", "cluster", clusterName)

	// Step 5: Generate a client secret (plaintext, shown once by API)
	clientSecret, err := c.generateClientSecret(ctx, identityID)
	if err != nil {
		logger.Error(err, "Failed to generate client secret", "cluster", clusterName, "identityID", identityID)
		return nil, fmt.Errorf("failed to generate client secret for identity %s: %w", identityID, err)
	}
	logger.Info("Client secret generated for Machine Identity", "cluster", clusterName)

	// Step 6: Grant project-level viewer access (least privilege for ESO)
	if err := c.grantProjectAccess(ctx, identityID, "viewer"); err != nil {
		logger.Error(err, "Failed to grant project access to Machine Identity", "cluster", clusterName, "identityID", identityID)
		// Continue — credentials still exist, just without project scope
		// The tenant SecretStore will fail with "no access" rather than "invalid credentials"
		logger.Info("Continuing despite partial project access failure; this MI will need manual role assignment")
	} else {
		logger.Info("Project viewer access granted to Machine Identity", "cluster", clusterName, "identityID", identityID)
	}

	// Store the real credentials at the shared path
	// Keys use kebab-case to match the actual data stored in Infisical
	// (the deployed operator creates kebab-case keys; remoteRef.property
	// in the ExternalSecret composition must match these JSON keys directly)
	creds := map[string]string{
		constant.KeyClientID:     clientID,
		constant.KeyClientSecret: clientSecret,
		constant.KeyProjectID:    c.ProjectID,
	}
	jsonBytes, err := json.Marshal(creds)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal infisical credentials: %w", err)
	}

	logger.Info("Uploading real Machine Identity credentials to Infisical", "cluster", clusterName, "path", sharedPath)
	if err := c.CreateSecret(ctx, sharedPath, "infisical-credentials", string(jsonBytes)); err != nil {
		logger.Error(err, "Failed to push infisical-credentials secret to Infisical", "cluster", clusterName, "path", sharedPath)
		return nil, fmt.Errorf("failed to push infisical-credentials secret: %w", err)
	}

	logger.Info("SpokePool real Machine Identity credentials seeded in Infisical", "cluster", clusterName, "path", sharedPath)
	return &EnsureInfisicalCredentialsResult{Result: EnsureCreated}, nil
}

// EnsureCrossplaneAdminPassword provisions the per-spoke crossplane_admin
// password that backs platform bootstrap (CNPG managed role + provider-sql
// ProviderConfig) on the spoke. The spoke ESO ExternalSecret
// (crossplane-admin-eso.yaml) reads exactly this key from the same secrets
// project, so the key and path MUST stay aligned with the AppSet-injected
// remoteRef (see platform-spoke-catalog AppSet kustomize patch).
//
// Idempotency contract:
//   - If <spoke>-crossplane-admin-password exists at root path → AlreadyExists.
//   - If missing AND isFirstTime → generate a secure password and upload.
//   - If missing AND !isFirstTime → Missing (manual recovery required; the
//     password was deleted after provisioning and the spoke CNPG cannot
//     silently pick up a regenerated one without coordinated bootstrap).
func (c *InfisicalClient) EnsureCrossplaneAdminPassword(ctx context.Context, spokeName string, isFirstTime bool) (EnsureResult, error) {
	logger := log.FromContext(ctx)

	logger.Info("Ensuring crossplane-admin password", "spoke", spokeName)
	infisicalKey := fmt.Sprintf("%s-crossplane-admin-password", spokeName)
	exists, err := c.SecretExists(ctx, "/", infisicalKey)
	if err != nil {
		logger.Error(err, "Failed to check Infisical for existing crossplane-admin password", "spoke", spokeName, "key", infisicalKey)
		return EnsureMissing, fmt.Errorf("failed to check Infisical for crossplane-admin password: %w", err)
	}

	if exists {
		logger.Info("Crossplane-admin password already exists in Infisical, skipping generation", "spoke", spokeName, "key", infisicalKey)
		return EnsureAlreadyExists, nil
	}

	if !isFirstTime {
		logger.Error(nil, "CRITICAL: crossplane-admin password missing from Infisical but SpokePool was already provisioned. Manual recovery required.",
			"spoke", spokeName, "key", infisicalKey)
		return EnsureMissing, fmt.Errorf("crossplane-admin password missing from Infisical for already-provisioned SpokePool %s - manual recovery required", spokeName)
	}

	password, err := GenerateSecurePassword()
	if err != nil {
		logger.Error(err, "Failed to generate crossplane-admin password", "spoke", spokeName)
		return EnsureMissing, fmt.Errorf("failed to generate crossplane-admin password: %w", err)
	}

	logger.Info("Uploading crossplane-admin password to Infisical", "spoke", spokeName, "key", infisicalKey, "passwordLength", len(password))
	if err := c.CreateSecret(ctx, "/", infisicalKey, password); err != nil {
		logger.Error(err, "Failed to upload crossplane-admin password to Infisical", "spoke", spokeName, "key", infisicalKey)
		return EnsureMissing, fmt.Errorf("failed to upload crossplane-admin password: %w", err)
	}

	logger.Info("Crossplane-admin password seeded in Infisical", "spoke", spokeName, "key", infisicalKey)
	return EnsureCreated, nil
}

// rotateSharedIdentityCredentials self-heals stale shared Machine Identity
// credentials for a cluster by rotating the existing identity's client secret and
// atomically updating the stored infisical-credentials value. It mirrors the
// provisioning steps (find/reuse identity, attach UA, generate client secret,
// grant project access) but uses the identity that already owns the stored
// clientId rather than creating a brand-new one, and updates rather than creates
// the stored secret.
//
// The rotation keeps tenant paths in sync via EnsureTenantFolderAndCredentials,
// which runs on every tenant reconcile and copies shared → tenant when stale,
// so a rotation here propagates to tenants (and their ExternalSecrets) without
// additional plumbing.
func (c *InfisicalClient) rotateSharedIdentityCredentials(ctx context.Context, clusterName, sharedPath, storedProjectID string) error {
	logger := log.FromContext(ctx).WithValues("cluster", clusterName, "path", sharedPath)

	identityName := fmt.Sprintf("spoke-pool-%s", clusterName)
	orgID, err := c.getOrganizationID(ctx)
	if err != nil {
		logger.Error(err, "Failed to get organization ID during rotation", "cluster", clusterName)
		return fmt.Errorf("failed to get organization ID: %w", err)
	}

	identityID, err := c.findIdentityByName(ctx, identityName, orgID)
	if err != nil {
		logger.Error(err, "Failed to search for existing Machine Identity during rotation", "cluster", clusterName, "identity", identityName)
		return fmt.Errorf("failed to search for Machine Identity %s: %w", identityName, err)
	}

	if identityID == "" {
		// Identity no longer exists — the stored credentials reference a deleted
		// identity. Provision a fresh one so the tenant SecretStore has a valid
		// credential to authenticate with.
		identityID, err = c.createMachineIdentityInInfisical(ctx, identityName, orgID)
		if err != nil {
			logger.Error(err, "Failed to create Machine Identity during rotation", "cluster", clusterName, "identity", identityName)
			return fmt.Errorf("failed to create Machine Identity %s: %w", identityName, err)
		}
		logger.Info("Recreated deleted Machine Identity during credential self-heal", "cluster", clusterName, "identityID", identityID)
	}

	if err := c.attachUniversalAuth(ctx, identityID); err != nil {
		logger.Error(err, "Failed to attach Universal Auth during rotation", "cluster", clusterName, "identityID", identityID)
		return fmt.Errorf("failed to attach Universal Auth to identity %s: %w", identityID, err)
	}

	clientID, err := c.getClientIDFromUniversalAuth(ctx, identityID)
	if err != nil {
		logger.Error(err, "Failed to get clientId during rotation", "cluster", clusterName, "identityID", identityID)
		return fmt.Errorf("failed to get clientId for identity %s: %w", identityID, err)
	}

	clientSecret, err := c.generateClientSecret(ctx, identityID)
	if err != nil {
		logger.Error(err, "Failed to generate client secret during rotation", "cluster", clusterName, "identityID", identityID)
		return fmt.Errorf("failed to generate client secret for identity %s: %w", identityID, err)
	}
	logger.Info("Rotated client secret for shared Machine Identity", "cluster", clusterName, "identityID", identityID)

	// Ensure the identity retains (or regains) project-level viewer access.
	if err := c.grantProjectAccess(ctx, identityID, "viewer"); err != nil {
		logger.Error(err, "Failed to grant project access during rotation (credentials still rotated)", "cluster", clusterName, "identityID", identityID)
	}

	// Preserve the stored project ID (secrets project) while rotating the secret.
	projectID := storedProjectID
	if projectID == "" {
		projectID = c.ProjectID
	}
	creds := map[string]string{
		constant.KeyClientID:     clientID,
		constant.KeyClientSecret: clientSecret,
		constant.KeyProjectID:    projectID,
	}
	jsonBytes, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("failed to marshal rotated infisical credentials: %w", err)
	}

	logger.Info("Updating rotated Machine Identity credentials in Infisical", "cluster", clusterName, "path", sharedPath)
	if err := c.UpdateSecret(ctx, sharedPath, "infisical-credentials", string(jsonBytes)); err != nil {
		logger.Error(err, "Failed to update rotated infisical-credentials", "cluster", clusterName, "path", sharedPath)
		return fmt.Errorf("failed to update rotated infisical-credentials: %w", err)
	}

	c.recordRotationAttempt(clusterName)
	logger.Info("SpokePool real Machine Identity credentials rotated and updated in Infisical", "cluster", clusterName, "path", sharedPath)
	return nil
}

// DEPRECATED: Tenant credential generation belongs to Kube-SBT (Tenant Identity Service)
// per ADR-039 and ADR-043. This function is retained for temporary backward compatibility
// only and MUST be removed once Kube-SBT is deployed and manages tenant credentials.
//
// EnsureTenantFolderAndCredentials creates the tenant folder, syncs shared
// Machine Identity credentials into the tenant's path (for SDK identity resolution
// per ADR-019), generates DB credentials for the tenant database, and materialises
// the identifier and credential of each declared OAuth confidential client
// (ADR-053).
//
// Unlike EnsureInfisicalCredentials, this does NOT read the shared path first to
// short-circuit — it always reads shared and converges tenant to match, ensuring
// stale credentials are fixed on the next reconcile.
//
// Idempotency contract (ADR-003):
//   - If db-credentials exist in tenant path → AlreadyExists (infisical-credentials
//     may still be synced if stale).
//   - If missing AND isFirstTime → copy infisical-credentials from shared path,
//     generate db-credentials, upload, return Created.
//   - If missing AND !isFirstTime → return Missing (manual intervention required).
func (c *InfisicalClient) EnsureTenantFolderAndCredentials(ctx context.Context, clusterName, tenantId, appId string, isFirstTime bool, oauthClients []OAuthClient, cacheEnabled, cacheIsFirstTime, gatewayEnabled bool) (*EnsureTenantCredentialsResult, error) {
	logger := log.FromContext(ctx).WithValues("tenant", tenantId, "app", appId, "cluster", clusterName)
	tenantPath := fmt.Sprintf(InfisicalTenantPathFormat, clusterName, appId)

	// Declared with the other outcomes so every early return reports it. A
	// tenant with no cache reports Skipped, never AlreadyExists — the controller
	// must not record "seeded" for a credential nobody asked for.
	cacheOutcome := EnsureSkipped

	logger.Info("Ensuring tenant folder hierarchy in Infisical", "path", tenantPath)
	if err := c.EnsureFolder(ctx, tenantPath); err != nil {
		logger.Error(err, "Failed to create tenant folder", "path", tenantPath)
		return nil, fmt.Errorf("failed to create tenant folder: %w", err)
	}

	// Step 1: Sync shared Machine Identity credentials to tenant path
	// Always reads shared and updates tenant if missing or stale.
	infisicalCredsOutcome := EnsureAlreadyExists
	infisicalSecretName := "infisical-credentials"
	sharedPath := fmt.Sprintf(InfisicalSharedPathFormat, clusterName)

	sharedValue, err := c.GetSecret(ctx, sharedPath, infisicalSecretName)
	if err != nil {
		logger.Error(err, "Failed to read shared infisical-credentials", "sharedPath", sharedPath)
		return nil, fmt.Errorf("failed to read shared %s for tenant %s: %w", infisicalSecretName, tenantId, err)
	}

	tenantExists, err := c.SecretExists(ctx, tenantPath, infisicalSecretName)
	if err != nil {
		logger.Error(err, "Failed to check Infisical for infisical-credentials", "path", tenantPath)
		return nil, fmt.Errorf("failed to check Infisical for %s: %w", infisicalSecretName, err)
	}

	if tenantExists {
		tenantValue, err := c.GetSecret(ctx, tenantPath, infisicalSecretName)
		if err != nil {
			logger.Error(err, "Failed to read tenant infisical-credentials", "path", tenantPath)
			return nil, fmt.Errorf("failed to read tenant %s at %s: %w", infisicalSecretName, tenantPath, err)
		}
		if tenantValue == sharedValue {
			logger.Info("Tenant infisical-credentials already up-to-date", "path", tenantPath)
		} else {
			logger.Info("Updating stale tenant infisical-credentials from shared", "path", tenantPath)
			if err := c.UpdateSecret(ctx, tenantPath, infisicalSecretName, sharedValue); err != nil {
				logger.Error(err, "Failed to update infisical-credentials in tenant path", "path", tenantPath)
				return nil, fmt.Errorf("failed to update %s for tenant %s: %w", infisicalSecretName, tenantId, err)
			}
			infisicalCredsOutcome = EnsureCreated
			logger.Info("Tenant infisical-credentials updated from shared", "path", tenantPath)
		}
	} else {
		logger.Info("Copying shared Machine Identity credentials to tenant path", "sharedPath", sharedPath)
		if err := c.CreateSecret(ctx, tenantPath, infisicalSecretName, sharedValue); err != nil {
			logger.Error(err, "Failed to push infisical-credentials to tenant path", "path", tenantPath)
			return nil, fmt.Errorf("failed to push %s for tenant %s: %w", infisicalSecretName, tenantId, err)
		}
		infisicalCredsOutcome = EnsureCreated
		logger.Info("Tenant infisical-credentials seeded in Infisical", "path", tenantPath)
	}

	// Step 2: Ensure db-credentials for PostgreSQL
	dbSecretName := "db-credentials"
	dbExists, err := c.SecretExists(ctx, tenantPath, dbSecretName)
	if err != nil {
		logger.Error(err, "Failed to check Infisical for db-credentials", "path", tenantPath)
		return nil, fmt.Errorf("failed to check Infisical for %s: %w", dbSecretName, err)
	}

	dbOutcome := EnsureAlreadyExists
	switch {
	case dbExists:
		logger.Info("DB credentials already exist in Infisical, skipping generation", "path", tenantPath, "secret", dbSecretName)

	case !isFirstTime:
		// Absence after first provisioning is a fault, never a trigger to
		// regenerate: a credential that was never created cannot be told apart
		// from one that was lost, and only the first is safe to replace.
		logger.Error(nil, "CRITICAL: DB credentials missing from Infisical for already-provisioned tenant. Manual intervention required.", "path", tenantPath)
		return &EnsureTenantCredentialsResult{
			Result:                EnsureMissing,
			InfisicalCredsOutcome: infisicalCredsOutcome,
			CacheOutcome:          cacheOutcome,
		}, fmt.Errorf("db-credentials missing from Infisical for already-provisioned tenant %s — manual recovery required", tenantId)

	default:
		password, err := GenerateSecurePassword()
		if err != nil {
			logger.Error(err, "Failed to generate password for tenant")
			return nil, fmt.Errorf("failed to generate password for tenant %s: %w", tenantId, err)
		}

		logger.Info("Generated database password for tenant", "passwordLength", len(password))

		// THE PASSWORD, AND NOTHING ELSE.
		//
		// This used to write a username too, built as "tenant-<tenantId>-user".
		// The role it named is created by the spoke composition from a DIFFERENT
		// axis, and nothing compared the two, so the credential named a role that
		// did not exist. The failure surfaced only as
		//
		//	psql: FATAL: password authentication failed for user "tenant-<tenant>-user"
		//
		// inside the tenant baseline migration Job, three systems away from either
		// half of the name, and its visible effect was a fleet with no users table.
		// It had passed for a year because the first tenant's tenantId WAS its
		// appId; two apps of one tenant would additionally have COLLIDED on one
		// role, since a postgres role is cluster-wide.
		//
		// The name now has exactly one definition -- spec.dbRoleName on the XR,
		// composed by the chart -- and the composition patches it into the
		// ExternalSecret's template. A username is a derived NAME, not a secret,
		// and it only lived here because it was generated beside a password.
		//
		// A key left over from before is simply unread: nothing fetches
		// `property: username` any more.
		//
		// COMPOSITE SECRET: Marshal to JSON for ESO 'property' parsing
		dbJsonBytes, err := json.Marshal(map[string]string{
			"password": password,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal db credentials: %w", err)
		}

		logger.Info("Uploading db-credentials to Infisical", "path", tenantPath)
		if err := c.CreateSecret(ctx, tenantPath, dbSecretName, string(dbJsonBytes)); err != nil {
			logger.Error(err, "Failed to upload db-credentials to Infisical", "path", tenantPath)
			return nil, fmt.Errorf("failed to push db-credentials for tenant %s: %w", tenantId, err)
		}
		logger.Info("Tenant DB credentials seeded in Infisical", "path", tenantPath, "secret", dbSecretName)
		dbOutcome = EnsureCreated
	}

	// Step 3: cache credential.
	//
	// A password, not an ACL file. The file is rendered from it by the delivering
	// ExternalSecret's template, so the platform owns the ACL's shape in one place
	// rather than each fleet hand-writing a file whose format nothing validates.
	// Generated once under the same rule as everything else here: its later absence
	// is a fault, because regenerating locks out a running cache's clients.
	//
	// SEEDED WHATEVER cacheEnabled SAYS, and the parameter is kept only to report
	// the outcome. This was `if cacheEnabled`, and it disagreed with the other half
	// of the platform: the AINativeSaaS composition renders the cache Deployment,
	// Service, Certificate, NetworkPolicy and ExternalSecret for EVERY tenant. Its
	// own comment claims they are "rendered only when the fleet declares
	// spec.cache.enabled", but it is a classic Composition with no pipeline and no
	// conditional function, so nothing gates them and nothing ever did.
	//
	// The two halves therefore contradicted each other, and the contradiction was
	// not inert: a tenant with cache.enabled=false received a cache ExternalSecret
	// naming a CACHE_PASSWORD the operator refused to write, so it failed
	// SecretSyncedError forever and the cache Deployment never became ready. That
	// is oranger's state on 2026-09-30, and it is the state of every future tenant
	// that leaves the cache off.
	//
	// Seeding unconditionally is the cheap half of the fix and it is sound: the
	// platform itself always asks for this credential, so writing it is not
	// "seeding something nobody asked for". An unused password costs nothing.
	//
	// The other half -- actually gating those resources on spec.cache.enabled --
	// needs the composition converted to a pipeline, which is a larger change than
	// this file. Until then a tenant with the cache off gets a working cache it
	// does not use, which is waste rather than breakage.
	{
		exists, err := c.SecretExists(ctx, tenantPath, InfisicalCachePasswordKey)
		if err != nil {
			return nil, fmt.Errorf("failed to check Infisical for %s: %w", InfisicalCachePasswordKey, err)
		}
		cacheOutcome = EnsureAlreadyExists
		if !exists {
			// PER-CREDENTIAL, not per-tenant. The tenant-wide isFirstTime is the
			// wrong question for this key, and asking it deadlocked waypoint on
			// 2026-09-02: the tenant's DB credentials survived a cluster rebuild
			// in Infisical, so TenantDBCredentialsSeeded stayed true and every
			// reconcile thereafter concluded the tenant was already provisioned
			// — while CACHE_PASSWORD, a credential introduced AFTER that tenant
			// was first provisioned, had never been written at all. The operator
			// then refused to seed a key that had never existed, the cache
			// ExternalSecret stayed SecretSyncedError, and bff and the cache
			// could not start for want of a secret nothing was allowed to create.
			//
			// This is the same argument the OAuth clients above already make:
			// "a client declared today on a tenant provisioned months ago is
			// new, and treating its absent credential as a fault would make
			// declaring a second client impossible." A cache enabled today on a
			// tenant provisioned months ago is new in exactly that way.
			//
			// The protection is unchanged where it matters: once this credential
			// HAS been seeded, its later absence is still a fault requiring
			// manual recovery, because regenerating it locks out a running
			// cache's clients.
			if !cacheIsFirstTime {
				logger.Error(nil, "CRITICAL: cache credential missing for a tenant whose cache credential was already seeded. Manual recovery required.", "path", tenantPath)
				return &EnsureTenantCredentialsResult{
					Result:                dbOutcome,
					InfisicalCredsOutcome: infisicalCredsOutcome,
					CacheOutcome:          cacheOutcome,
				}, fmt.Errorf("%s missing from Infisical for already-provisioned tenant %s — manual recovery required", InfisicalCachePasswordKey, tenantId)
			}
			value, err := GenerateSecurePassword()
			if err != nil {
				return nil, fmt.Errorf("failed to generate cache credential for tenant %s: %w", tenantId, err)
			}
			if err := c.CreateSecret(ctx, tenantPath, InfisicalCachePasswordKey, value); err != nil {
				return nil, fmt.Errorf("failed to write %s for tenant %s: %w", InfisicalCachePasswordKey, tenantId, err)
			}
			logger.Info("Seeded tenant cache credential", "path", tenantPath,
				"key", InfisicalCachePasswordKey, "cacheEnabled", cacheEnabled)
			dbOutcome = EnsureCreated
			cacheOutcome = EnsureCreated
		}
	}

	// Step 4: gateway session-cookie key.
	//
	// Written on absence whatever the tenant's age, like the OAuth credentials in
	// step 5 and for the same reason: enabling the gateway is a thing a tenant
	// does long after it is provisioned, and refusing to seed the key then would
	// make the feature unreachable for every tenant that already exists.
	//
	// The value this protects — never invalidating live sessions by replacing a
	// key in use — is held by the existence check alone. An absent key means no
	// gateway has ever started with one, so there is no session to lose.
	if gatewayEnabled {
		exists, err := c.SecretExists(ctx, tenantPath, InfisicalGatewayCookieSecretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to check Infisical for %s: %w", InfisicalGatewayCookieSecretKey, err)
		}
		if !exists {
			value, err := GenerateHexKey(32)
			if err != nil {
				return nil, fmt.Errorf("failed to generate gateway cookie key for tenant %s: %w", tenantId, err)
			}
			if err := c.CreateSecret(ctx, tenantPath, InfisicalGatewayCookieSecretKey, value); err != nil {
				return nil, fmt.Errorf("failed to write %s for tenant %s: %w", InfisicalGatewayCookieSecretKey, tenantId, err)
			}
			logger.Info("Seeded tenant gateway cookie key", "path", tenantPath, "key", InfisicalGatewayCookieSecretKey)
			dbOutcome = EnsureCreated
		}
	}

	// Step 4b: the application's internal service token (ADR-057).
	//
	// PLATFORM-SEEDED, like the cookie key above and for the same reason: nobody
	// chooses its value. The fleet's own declaration says so in as many words --
	// "any long random value" -- and a secret whose value nobody chooses is
	// plumbing, not configuration.
	//
	// It was supplied by hand, once per application, and the cost of that showed
	// up exactly where a manual step always does. oranger's was never written, so
	// both ExternalSecrets failed, so neither Secret was created, so bff-workload
	// and sdk-workload sat in CreateContainerConfigError for a day naming a
	// Kubernetes Secret rather than the Infisical key that was actually missing.
	// Nothing in that chain says "a human forgot to run a command", and every new
	// application would have met the same wall.
	//
	// Written on absence whatever the tenant's age. An absent token means neither
	// workload has ever started holding one, so there is no live channel to break
	// -- the same argument the cookie key makes. Once present it is never
	// rewritten here: rotating it is a deliberate act that must restart both
	// sides, and a reconcile must not do that to a running application.
	internalTokenKey := InfisicalInternalTokenKey(appId)
	exists, err := c.SecretExists(ctx, tenantPath, internalTokenKey)
	if err != nil {
		return nil, fmt.Errorf("failed to check Infisical for %s: %w", internalTokenKey, err)
	}
	if !exists {
		value, err := GenerateHexKey(32)
		if err != nil {
			return nil, fmt.Errorf("failed to generate internal token for tenant %s: %w", tenantId, err)
		}
		if err := c.CreateSecret(ctx, tenantPath, internalTokenKey, value); err != nil {
			return nil, fmt.Errorf("failed to write %s for tenant %s: %w", internalTokenKey, tenantId, err)
		}
		logger.Info("Seeded application internal service token", "path", tenantPath, "key", internalTokenKey)
		dbOutcome = EnsureCreated
	}

	// Step 5: OAuth confidential client credentials (ADR-053).
	//
	// Reached whether or not step 2 generated anything, because a tenant
	// provisioned before it declared a client must still receive that client's
	// credential. The db-credentials step used to return here, which is why this
	// is a switch above rather than an early return.
	oauthOutcome, err := c.ensureOAuthClientCredentials(ctx, tenantPath, tenantId, oauthClients)
	if err != nil {
		return &EnsureTenantCredentialsResult{
			Result:                dbOutcome,
			InfisicalCredsOutcome: infisicalCredsOutcome,
			CacheOutcome:          cacheOutcome,
			OAuthOutcome:          EnsureMissing,
		}, err
	}

	return &EnsureTenantCredentialsResult{
		Result:                dbOutcome,
		InfisicalCredsOutcome: infisicalCredsOutcome,
		CacheOutcome:          cacheOutcome,
		OAuthOutcome:          oauthOutcome,
	}, nil
}

// InfisicalCachePasswordKey names the tenant cache credential. The delivering
// ExternalSecret templates both the ACL file and the connection URL from this one
// value, so nothing downstream stores a second copy of it.
const InfisicalCachePasswordKey = "CACHE_PASSWORD"

// InfisicalInternalTokenKey names an application's internal service token: the
// credential its BFF presents to its own SDK (ADR-057's forwarding rule).
//
// DERIVED FROM THE APPLICATION ID, and that derivation is the platform contract:
// an application whose workloads want an internal token read
// `<APPID>_INTERNAL_TOKEN`, and the platform seeds it. Both applications on this
// platform already spell it that way -- ORANGER_INTERNAL_TOKEN,
// WAYPOINT_INTERNAL_TOKEN -- so this names an existing convention rather than
// imposing a new one, and a fleet that spells it differently is simply not served
// by the platform and supplies its own.
//
// Not a tenant identifier in platform code (ADR-047): the appId is an input, and
// this is a function of it. A constant here would give every application of every
// customer the same key name in its own folder, which is fine, and the same VALUE
// only if the folder were shared, which it is not -- but it would also stop the
// key naming the application it belongs to, which is what makes a workload's env
// readable.
func InfisicalInternalTokenKey(appId string) string {
	return OAuthKeySegment(appId) + "_INTERNAL_TOKEN"
}

// InfisicalGatewayCookieSecretKey names the tenant gateway's session-cookie
// encryption key.
//
// Per tenant, not shared. Each tenant runs its own gateway instance (ADR-050
// amendment 2026-08-31), and the gateway encrypts its OIDC session cookie with
// this value — so a shared key would let one tenant's gateway decrypt another's
// sessions. That is currently harmless, because cookies are host-only and never
// presented across hostnames, and it is exactly the kind of latent dependency
// that stops being harmless without anyone noticing.
//
// 32 bytes hex-encoded: the gateway hex-decodes it and requires exactly 32 bytes
// for AES-256-GCM. A password from the general generator is the right length in
// characters and the wrong alphabet, and fails at startup complaining about an
// invalid character rather than about encoding.
const InfisicalGatewayCookieSecretKey = "AGENTGATEWAY_OIDC_COOKIE_SECRET"

// InfisicalOAuthClientIDKey and InfisicalOAuthClientSecretKey name the two scalars
// an OAuth confidential client occupies in a tenant's Infisical folder.
//
// Scalars rather than one composite JSON value, matching how the tenant's other
// application secrets are stored and read: the fleet's ExternalSecret maps each
// key by name, with no 'property' parsing. db-credentials is composite because ESO
// parses a property out of it; these are consumed as whole values in two different
// places — the registration controller wants them under fixed key names, the
// workload wants them under whatever names it reads — so a scalar each keeps both
// projections a plain rename.
//
// The tenant is not part of the key. The folder path already scopes it, and
// repeating it here would put a tenant identifier in platform code (ADR-047).
func InfisicalOAuthClientIDKey(clientName string) string {
	return "OAUTH_" + OAuthKeySegment(clientName) + "_CLIENT_ID"
}

func InfisicalOAuthClientSecretKey(clientName string) string {
	return "OAUTH_" + OAuthKeySegment(clientName) + "_CLIENT_SECRET"
}

// OAuthKeySegment turns an application or client name into the form a key name
// uses: upper case, hyphens as underscores.
//
// Exported because the key GRAMMAR is one rule and several producers follow it --
// the OAuth client keys here, and the per-dependency backend keys the controller
// publishes. A second copy would be a second grammar that drifts silently, since
// a key nobody writes is a key nobody reads and the failure is an ExternalSecret
// that never resolves.
func OAuthKeySegment(clientName string) string {
	return strings.ToUpper(strings.ReplaceAll(clientName, "-", "_"))
}

// OAuthClientIdentifier is the identifier a client is registered under: derived
// from the tenant and the client name, declared before registration and stable for
// the client's life (ADR-053). Changing it is not an in-place update — the
// registration is removed and recreated — so it is derived, never stored as an
// independent input.
func OAuthClientIdentifier(tenantId, clientName string) string {
	return fmt.Sprintf("%s-%s", tenantId, clientName)
}

// ensureOAuthClientCredentials materialises each declared confidential client's
// identifier and credential in the tenant's Infisical folder.
//
// The two halves obey different rules on purpose. The identifier is derived and
// public, so it is written convergently and a drifted value is repaired. The
// credential is secret material under ADR-003: generated once, and its later
// absence is a fault rather than a trigger to regenerate, because regenerating
// would invalidate a credential that may already be registered and in use.
//
// PreviouslyProvisioned is per client, not per tenant. A client declared today on
// a tenant provisioned months ago is being provisioned for the first time even
// though the tenant is not, and treating that as a fault would make declaring a
// second client impossible.
func (c *InfisicalClient) ensureOAuthClientCredentials(ctx context.Context, tenantPath, tenantId string, clients []OAuthClient) (EnsureResult, error) {
	logger := log.FromContext(ctx).WithValues("tenant", tenantId, "path", tenantPath)

	outcome := EnsureAlreadyExists
	for _, client := range clients {
		if !client.Confidential {
			// A public client authenticates with no secret; there is nothing to
			// generate, store or rotate. It is declared so the set is complete.
			continue
		}

		idKey := InfisicalOAuthClientIDKey(client.Name)
		secretKey := InfisicalOAuthClientSecretKey(client.Name)
		identifier := OAuthClientIdentifier(tenantId, client.Name)

		// Identifier: converge, repairing drift.
		idFound, err := c.SecretExists(ctx, tenantPath, idKey)
		if err != nil {
			return outcome, fmt.Errorf("failed to check Infisical for %s: %w", idKey, err)
		}
		storedID := ""
		if idFound {
			if storedID, err = c.GetSecret(ctx, tenantPath, idKey); err != nil {
				return outcome, fmt.Errorf("failed to read %s for tenant %s: %w", idKey, tenantId, err)
			}
		}
		switch {
		case !idFound:
			if err := c.CreateSecret(ctx, tenantPath, idKey, identifier); err != nil {
				return outcome, fmt.Errorf("failed to write %s for tenant %s: %w", idKey, tenantId, err)
			}
			logger.Info("Seeded OAuth client identifier", "client", client.Name, "key", idKey)
			outcome = EnsureCreated
		case storedID != identifier:
			if err := c.UpdateSecret(ctx, tenantPath, idKey, identifier); err != nil {
				return outcome, fmt.Errorf("failed to repair %s for tenant %s: %w", idKey, tenantId, err)
			}
			logger.Info("Repaired drifted OAuth client identifier", "client", client.Name, "key", idKey)
			outcome = EnsureCreated
		}

		// Credential: generate once, never regenerate.
		secretExists, err := c.SecretExists(ctx, tenantPath, secretKey)
		if err != nil {
			return outcome, fmt.Errorf("failed to check Infisical for %s: %w", secretKey, err)
		}
		if secretExists {
			continue
		}
		if client.PreviouslyProvisioned {
			logger.Error(nil, "CRITICAL: OAuth client credential missing for an already-provisioned client. Manual recovery required.",
				"client", client.Name, "key", secretKey)
			return EnsureMissing, fmt.Errorf(
				"%s missing from Infisical for already-provisioned client %s of tenant %s — manual recovery required",
				secretKey, client.Name, tenantId)
		}

		value, err := GenerateSecurePassword()
		if err != nil {
			return outcome, fmt.Errorf("failed to generate credential for client %s of tenant %s: %w", client.Name, tenantId, err)
		}
		if err := c.CreateSecret(ctx, tenantPath, secretKey, value); err != nil {
			return outcome, fmt.Errorf("failed to write %s for tenant %s: %w", secretKey, tenantId, err)
		}
		logger.Info("Seeded OAuth confidential client credential", "client", client.Name, "key", secretKey)
		outcome = EnsureCreated
	}

	return outcome, nil
}
