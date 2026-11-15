package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Client is an Infisical API client scoped to Machine Identity operations.
// It does NOT perform any PKI operations — certificates are cert-manager's domain per ADR-035.
type Client struct {
	BaseURL      string
	clientID     string
	clientSecret string
	token        string
	tokenExp     time.Time
	mu           sync.RWMutex
	HTTP         *http.Client
}

// Identity represents a Machine Identity with its Universal Auth credentials.
type Identity struct {
	ID           string
	ClientID     string
	ClientSecret string
}

func NewClient(baseURL string) *Client {
	clientID := os.Getenv("SECRET_STORE_CLIENT_ID")
	clientSecret := os.Getenv("SECRET_STORE_CLIENT_SECRET")
	return &Client{
		BaseURL:      baseURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) authenticate(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	clientID := c.clientID
	if clientID == "" {
		clientID = os.Getenv("SECRET_STORE_CLIENT_ID")
	}
	clientSecret := c.clientSecret
	if clientSecret == "" {
		clientSecret = os.Getenv("SECRET_STORE_CLIENT_SECRET")
	}
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("SECRET_STORE_CLIENT_ID and SECRET_STORE_CLIENT_SECRET must be set")
	}

	loginReq := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
	}
	body, err := json.Marshal(loginReq)
	if err != nil {
		return fmt.Errorf("marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+PathAuthUniversalAuthLogin, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("execute login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("universal auth login failed: status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var loginResp struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}

	c.token = loginResp.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(loginResp.ExpiresIn) * time.Second)
	return nil
}

func (c *Client) ensureAuthenticated(ctx context.Context) error {
	c.mu.RLock()
	valid := c.token != "" && time.Now().Add(5*time.Minute).Before(c.tokenExp)
	c.mu.RUnlock()
	if valid {
		return nil
	}
	return c.authenticate(ctx)
}

func (c *Client) getToken(ctx context.Context) (string, error) {
	if err := c.ensureAuthenticated(ctx); err != nil {
		return "", err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token, nil
}

// ErrInvalidCredentials is returned by ProbeIdentityLockout when the API
// rejects the supplied credentials as wrong (401 "Invalid credentials").
// It MUST NOT be treated like a lockout: the stored secret is defective and
// clearing a lockout would only re-arm the failure loop.
var ErrInvalidCredentials = errors.New("invalid credentials for identity auth method")

// ProbeIdentityLockout authenticates with the identity's own credentials and
// classifies the result. locked=true only when the API reports an active
// lockout (401 "temporarily locked"); a successful login (200) returns
// locked=false,nil; wrong credentials return ErrInvalidCredentials; any other
// status returns a wrapped error. A successful probe is side-effect free and
// the backend resets the lockout counter on success.
func (c *Client) ProbeIdentityLockout(ctx context.Context, clientID, clientSecret string) (bool, error) {
	loginReq := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
	}
	body, err := json.Marshal(loginReq)
	if err != nil {
		return false, fmt.Errorf("marshal probe login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+PathAuthUniversalAuthLogin, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("create probe login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("execute probe login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return false, nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	msg := string(respBody)
	if resp.StatusCode == http.StatusUnauthorized {
		switch {
		case strings.Contains(msg, "temporarily locked"):
			return true, nil
		case strings.Contains(msg, "Invalid credentials"):
			return false, fmt.Errorf("%w: %s", ErrInvalidCredentials, msg)
		}
	}
	return false, fmt.Errorf("probe login failed: status %d: %s", resp.StatusCode, msg)
}

// ClearLockout clears an active identity auth-method lockout on the
// authoritative Infisical instance using the operator's own (admin) token.
// Idempotent: an already-unlocked identity is treated as success. Only call
// this after ProbeIdentityLockout reported locked=true — never for
// ErrInvalidCredentials.
func (c *Client) ClearLockout(ctx context.Context, identityID, clientID string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}

	payload := map[string]interface{}{
		"identityId": identityID,
		"clientId":   clientID,
		"lockedOut":  false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal clear lockout request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+PathAuthUniversalAuthClearLockout, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create clear lockout request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("execute clear lockout request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusBadRequest &&
		(bytes.Contains(respBody, []byte("not locked")) || bytes.Contains(respBody, []byte("lockout"))) {
		return nil
	}
	return fmt.Errorf("clear lockout failed: status %d: %s", resp.StatusCode, string(respBody))
}

// GetOrCreateMachineIdentity finds or creates a Machine Identity in Infisical.
func (c *Client) GetOrCreateMachineIdentity(ctx context.Context, name, orgID string) (*Identity, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()

	existing, err := c.findIdentityByName(ctx, name, orgID)
	if err == nil && existing != "" {
		ua, err := c.getUniversalAuth(ctx, existing)
		if err != nil {
			return nil, fmt.Errorf("get universal auth for existing identity: %w", err)
		}
		return &Identity{ID: existing, ClientID: ua.IdentityUniversalAuth.ClientID, ClientSecret: ""}, nil
	}

	id, err := c.createIdentity(ctx, name, orgID)
	if err != nil {
		return nil, fmt.Errorf("create machine identity: %w", err)
	}

	if err := c.attachUniversalAuth(ctx, id); err != nil {
		return nil, fmt.Errorf("attach universal auth: %w", err)
	}

	clientID, err := c.getClientID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get client ID: %w", err)
	}

	clientSecret, err := c.generateClientSecret(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("generate client secret: %w", err)
	}

	return &Identity{ID: id, ClientID: clientID, ClientSecret: clientSecret}, nil
}

// GrantProjectAccess grants a Machine Identity access to an Infisical project.
func (c *Client) GrantProjectAccess(ctx context.Context, identityID, projectID, role string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}

	payload := map[string]string{"role": role}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal grant access: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+fmt.Sprintf(PathProjectMembershipsIdentities, projectID, identityID),
		bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("create grant access request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("execute grant access request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusConflict {
		respBody, _ := io.ReadAll(resp.Body)
		if bytes.Contains(respBody, []byte("already")) || bytes.Contains(respBody, []byte("exists")) {
			return nil
		}
		return fmt.Errorf("grant project access failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("grant project access failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// RotateClientSecret creates a new client secret and revokes the old one after the overlap period.
// Returns the new client secret ID and secret value.
func (c *Client) RotateClientSecret(ctx context.Context, identityID, oldSecretID string) (string, string, error) {
	secret, err := c.generateClientSecret(ctx, identityID)
	if err != nil {
		return "", "", fmt.Errorf("generate new client secret: %w", err)
	}

	if oldSecretID != "" {
		if revokeErr := c.revokeClientSecret(ctx, identityID, oldSecretID); revokeErr != nil {
			return "", secret, fmt.Errorf("revoke old client secret (new secret created): %w", revokeErr)
		}
	}

	return "", secret, nil
}

// RevokeAllClientSecrets revokes all client secrets for an identity.
func (c *Client) RevokeAllClientSecrets(ctx context.Context, identityID string, secretIDs []string) error {
	for _, secretID := range secretIDs {
		if err := c.revokeClientSecret(ctx, identityID, secretID); err != nil {
			return fmt.Errorf("revoke secret %s: %w", secretID, err)
		}
	}
	return nil
}

// DeleteIdentity deletes a Machine Identity from Infisical.
func (c *Client) DeleteIdentity(ctx context.Context, identityID string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "DELETE",
		c.BaseURL+fmt.Sprintf(PathIdentityByID, identityID), nil)
	if err != nil {
		return fmt.Errorf("create delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("execute delete request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete identity failed: status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// IdentityExists checks whether a Machine Identity exists by ID.
func (c *Client) IdentityExists(ctx context.Context, identityID string) (bool, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return false, fmt.Errorf("authenticate: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		c.BaseURL+fmt.Sprintf(PathIdentityByID, identityID), nil)
	if err != nil {
		return false, fmt.Errorf("create get identity request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("execute get identity request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("get identity failed: status %d: %s", resp.StatusCode, string(bodyBytes))
}

// --- private helpers ---

func (c *Client) findIdentityByName(ctx context.Context, name, orgID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s%s?limit=100&orgId=%s", c.BaseURL, PathIdentities, orgID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list identities failed: status %d", resp.StatusCode)
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
		return "", err
	}

	for _, item := range listResp.Identities {
		if item.Identity.Name == name {
			return item.Identity.ID, nil
		}
	}
	return "", nil
}

type universalAuthResponse struct {
	IdentityUniversalAuth struct {
		ClientID string `json:"clientId"`
	} `json:"identityUniversalAuth"`
}

func (c *Client) getUniversalAuth(ctx context.Context, identityID string) (*universalAuthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.BaseURL+fmt.Sprintf(PathAuthUniversalAuthIdentities, identityID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get universal auth failed: status %d", resp.StatusCode)
	}

	var ua universalAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&ua); err != nil {
		return nil, err
	}
	return &ua, nil
}

func (c *Client) createIdentity(ctx context.Context, name, orgID string) (string, error) {
	payload := map[string]string{
		"name":           name,
		"organizationId": orgID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+PathIdentities, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create identity failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Identity struct {
			ID string `json:"id"`
		} `json:"identity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Identity.ID, nil
}

func (c *Client) attachUniversalAuth(ctx context.Context, identityID string) error {
	payload := map[string]interface{}{
		"clientSecretTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTTL":    2592000,
		"accessTokenMaxTTL": 2592000,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+fmt.Sprintf(PathAuthUniversalAuthIdentities, identityID),
		bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusBadRequest && bytes.Contains(respBody, []byte("already")) {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("attach universal auth failed: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) getClientID(ctx context.Context, identityID string) (string, error) {
	ua, err := c.getUniversalAuth(ctx, identityID)
	if err != nil {
		return "", err
	}
	return ua.IdentityUniversalAuth.ClientID, nil
}

func (c *Client) generateClientSecret(ctx context.Context, identityID string) (string, error) {
	payload := map[string]interface{}{
		"numUsesLimit": 0,
		"ttl":          0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+fmt.Sprintf(PathAuthUniversalAuthClientSecrets, identityID),
		bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("generate client secret failed: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ClientSecret, nil
}

func (c *Client) revokeClientSecret(ctx context.Context, identityID, secretID string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+fmt.Sprintf(PathAuthUniversalAuthClientSecrets+"/%s/revoke", identityID, secretID), nil)
	if err != nil {
		return fmt.Errorf("create revoke request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("execute revoke request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revoke client secret failed: status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}
