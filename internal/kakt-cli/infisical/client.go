package infisical

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	neturl "net/url"
	"os"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
)

// isRetryableTransportError reports whether the error returned from
// an HTTP call is a transport-level failure (connection reset, DNS,
// dial errors) that may succeed on a subsequent attempt. HTTP status
// errors (401/403/5xx with a body) are not retryable here — those are
// the caller's responsibility to interpret.
func isRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// Client wraps Infisical API operations
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
}

// Config holds Infisical connection configuration
type Config struct {
	BaseURL         string
	ProjectSlug     string
	EnvironmentSlug string
}

// NewClient creates a new Infisical API client
// It retrieves the Infisical URL and authenticates using Universal Auth
// Reuses the same credentials that ESO uses (infisical-auth in external-secrets-system)
func NewClient(ctx context.Context, clientset *kubernetes.Clientset) (*Client, error) {
	// INFISICAL_API_URL, or nothing. There is no default.
	//
	// It defaulted to https://infisical.dev.nutgraf.in -- the PLATFORM's own
	// Infisical. On a tenant's box that is not a fallback, it is a different
	// tenant's secret store: the CLI would authenticate against the platform's
	// instance, read or write the platform's secrets, and report success. ADR-065
	// says the platform holds no secret belonging to a tenant, and a default that
	// points every box at one instance breaks that from the client side.
	//
	// Every caller sets it. EnsurePortForward does, for the in-cluster case; a box
	// reaching its own Infisical over its public hostname derives that from its own
	// domain. Not knowing which Infisical to talk to is an error, not a guess.
	baseURL := strings.TrimSpace(os.Getenv("INFISICAL_API_URL"))
	if baseURL == "" {
		return nil, fmt.Errorf("INFISICAL_API_URL is not set, so there is no Infisical to talk to.\n\n" +
			"It is this box's own instance -- infisical.<the box's domain>, or a\n" +
			"port-forward to it. There is no default: the one that existed pointed at\n" +
			"the platform's Infisical, which is a different tenant's secret store.")
	}

	// Get Universal Auth credentials from the same secret ESO uses
	esoNamespace := constants.NamespaceOps
	secret, err := clientset.CoreV1().Secrets(esoNamespace).Get(ctx, "infisical-auth", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get infisical-auth secret: %w (run 'hub init-secrets' first)", err)
	}

	clientID := string(secret.Data["client-id"])
	clientSecret := string(secret.Data["client-secret"])

	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("infisical-auth secret missing client-id or client-secret (run 'hub init-secrets' first)")
	}

	client := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				// Ignore TLS verification for Day-0 bootstrap when certificates are still provisioning
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}

	// Authenticate and get access token.
	//
	// Retry on transport errors (connection reset, DNS, dial). When the
	// Infisical pod is restarted by Step 3.5's heavy bootstrap operations
	// (org/project/identity/cert creation), the kubectl port-forward
	// tunnel to the Service drops its in-flight TCP connection and emits
	// "connection reset by peer" until it re-establishes. We give the
	// tunnel time to reconnect by retrying with a short backoff. HTTP
	// status errors (401, 403, 5xx with body) are NOT retried — they
	// are auth/config issues the operator must resolve.
	var authErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 2 * time.Second
			fmt.Printf("   → retrying Infisical authentication (attempt %d/3, waiting %s)...\n", attempt+1, backoff)
			time.Sleep(backoff)
		}
		authErr = client.authenticate(ctx, clientID, clientSecret)
		if authErr == nil {
			break
		}
		if !isRetryableTransportError(authErr) {
			break
		}
	}
	if authErr != nil {
		return nil, fmt.Errorf("failed to authenticate with Infisical: %w", authErr)
	}

	return client, nil
}

// getWorkspaceIdFromSlug converts projectSlug to workspaceId by querying Infisical API
func (c *Client) getWorkspaceIdFromSlug(ctx context.Context, projectSlug string) (string, error) {
	// List all workspaces and find the one matching the slug
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+PathWorkspace, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to list workspaces with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var workspacesResp struct {
		Workspaces []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"workspaces"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&workspacesResp); err != nil {
		return "", fmt.Errorf("failed to decode workspaces response: %w", err)
	}

	for _, ws := range workspacesResp.Workspaces {
		if ws.Slug == projectSlug {
			return ws.ID, nil
		}
	}

	return "", fmt.Errorf("workspace with slug '%s' not found", projectSlug)
}

// authenticate performs Universal Auth login to get access token
func (c *Client) authenticate(ctx context.Context, clientID, clientSecret string) error {
	loginReq := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
	}

	body, err := json.Marshal(loginReq)
	if err != nil {
		return fmt.Errorf("failed to marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+PathAuthUniversalAuthLogin, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("authentication failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var loginResp struct {
		AccessToken string `json:"accessToken"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return fmt.Errorf("failed to decode login response: %w", err)
	}

	c.token = loginResp.AccessToken
	return nil
}

// CreateOrUpdateSecret creates or updates a secret in Infisical
func (c *Client) CreateOrUpdateSecret(ctx context.Context, projectSlug, environmentSlug, secretPath, key, value string) error {
	// Convert projectSlug to workspaceId (required for v3 write operations)
	workspaceId, err := c.getWorkspaceIdFromSlug(ctx, projectSlug)
	if err != nil {
		return fmt.Errorf("failed to get workspace ID: %w", err)
	}

	// First, try to get the secret to see if it exists
	exists, err := c.secretExists(ctx, workspaceId, environmentSlug, secretPath, key)
	if err != nil {
		return fmt.Errorf("failed to check if secret exists: %w", err)
	}

	if exists {
		return c.updateSecret(ctx, workspaceId, environmentSlug, secretPath, key, value)
	}

	err = c.createSecret(ctx, workspaceId, environmentSlug, secretPath, key, value)
	if err == nil {
		return nil
	}

	// The existence check said no and the server says otherwise. The server is
	// the one that knows.
	//
	// Check-then-act is only as good as the two calls agreeing about identity,
	// and here they do not always: the GET carries workspaceId, environment and
	// secretPath, the POST carries those plus type=shared, and a lookup that
	// misses for any reason turns a re-run into a hard failure. It did --
	// resuming a bootstrap died on infisical-db-username with
	// "Secret already exists", after three and a half minutes of work that had
	// all succeeded.
	//
	// Day-0 has to be re-runnable. Every phase before this one already is, and a
	// bootstrap that cannot resume is one that starts from an empty cluster
	// after any transient failure.
	if isAlreadyExists(err) {
		return c.updateSecret(ctx, workspaceId, environmentSlug, secretPath, key, value)
	}
	return err
}

// isAlreadyExists reports whether a create failed because the secret is there.
//
// Matched on the message because Infisical answers 400 for this and for a
// genuinely malformed request, so the status alone cannot tell them apart.
// Narrow on purpose: a substring broad enough to catch an unrelated 400 would
// turn a real error into a silent overwrite.
func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Secret already exists")
}

// secretExists checks if a secret already exists
func (c *Client) secretExists(ctx context.Context, workspaceId, environmentSlug, secretPath, key string) (bool, error) {
	// Escaped rather than interpolated. A secretPath is a path and may carry
	// characters that are not query-safe; unescaped, the lookup misses and the
	// caller concludes the secret does not exist.
	q := neturl.Values{}
	q.Set("workspaceId", workspaceId)
	q.Set("environment", environmentSlug)
	q.Set("secretPath", secretPath)
	url := fmt.Sprintf("%s%s?%s", c.baseURL, fmt.Sprintf(PathSecretsRaw, neturl.PathEscape(key)), q.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(bodyBytes))
}

// createSecret creates a new secret in Infisical using v3 API with workspaceId
func (c *Client) createSecret(ctx context.Context, workspaceId, environmentSlug, secretPath, key, value string) error {
	createReq := map[string]interface{}{
		"workspaceId": workspaceId,
		"environment": environmentSlug,
		"secretPath":  secretPath,
		"secretName":  key,
		"secretValue": value,
		"type":        "shared",
	}

	body, err := json.Marshal(createReq)
	if err != nil {
		return fmt.Errorf("failed to marshal create request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+fmt.Sprintf(PathSecretsRaw, key), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create secret with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// updateSecret updates an existing secret in Infisical using v3 API with workspaceId
func (c *Client) updateSecret(ctx context.Context, workspaceId, environmentSlug, secretPath, key, value string) error {
	updateReq := map[string]interface{}{
		"workspaceId": workspaceId,
		"environment": environmentSlug,
		"secretPath":  secretPath,
		"secretValue": value,
		"type":        "shared",
	}

	body, err := json.Marshal(updateReq)
	if err != nil {
		return fmt.Errorf("failed to marshal update request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "PATCH", c.baseURL+fmt.Sprintf(PathSecretsRaw, key), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to update secret with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// GetInfisicalConfig reads this box's Infisical coordinates from
// hub-bootstrap-config.
//
// EnvironmentSlug here is the MANAGEMENT scope -- the environment the hub's own
// secrets live in. It is not the environment of a workload cluster or of an
// application, and a caller holding either of those must use that one instead.
//
// Both of this function's fallbacks are gone. It used to answer `dev` when the
// ConfigMap was missing and again when either field was empty, which meant a
// caller on a stg box was handed dev and wrote the box's secrets into the wrong
// environment -- with no error, because a fallback had been chosen for it. There
// is no safe guess for which environment someone else's secrets are in.
func GetInfisicalConfig(ctx context.Context, clientset *kubernetes.Clientset) (*Config, error) {
	// Look for the correct ConfigMap in the correct namespace
	cm, err := clientset.CoreV1().ConfigMaps(constants.NamespaceOps).Get(ctx, "hub-bootstrap-config", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read hub-bootstrap-config in %s: %w", constants.NamespaceOps, err)
	}

	// Use the secrets project slug for secret read/write operations
	projectSlug := cm.Data["INFISICAL_SECRETS_PROJECT_SLUG"]
	if projectSlug == "" {
		projectSlug = cm.Data["INFISICAL_PROJECT_SLUG"]
	}
	environmentSlug := cm.Data["INFISICAL_ENVIRONMENT_SLUG"]

	if projectSlug == "" {
		return nil, fmt.Errorf("hub-bootstrap-config carries neither " +
			"INFISICAL_SECRETS_PROJECT_SLUG nor INFISICAL_PROJECT_SLUG")
	}
	if environmentSlug == "" {
		return nil, fmt.Errorf("hub-bootstrap-config carries no INFISICAL_ENVIRONMENT_SLUG " +
			"(rendered from the box's declared management.infisicalEnvironmentSlug)")
	}

	return &Config{
		ProjectSlug:     projectSlug,
		EnvironmentSlug: environmentSlug,
	}, nil
}

// ListSecretNames returns the NAMES of the secrets at a path, and never their
// values.
//
// The response body carries `secretValue` on every entry, and this function
// deliberately does not have a field for it. That is the whole point of the
// shape: `kakt fleet secrets status` prints what it gets back, so a struct
// with a value field is one formatting mistake away from a credential on a
// terminal and in a scrollback buffer. A caller that cannot obtain a value
// cannot leak one.
//
// An absent folder is not an error. A fleet that has had nothing supplied yet
// has no folder at all, and that is the NORMAL state for a tenant between
// declaring a secret and supplying it -- the state `status` exists to report.
func (c *Client) ListSecretNames(ctx context.Context, projectSlug, environmentSlug, secretPath string) ([]string, error) {
	workspaceId, err := c.getWorkspaceIdFromSlug(ctx, projectSlug)
	if err != nil {
		return nil, fmt.Errorf("failed to get workspace ID: %w", err)
	}

	q := neturl.Values{}
	q.Set("workspaceId", workspaceId)
	q.Set("environment", environmentSlug)
	q.Set("secretPath", secretPath)
	url := fmt.Sprintf("%s%s?%s", c.baseURL, PathSecretsRawList, q.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// secretKey only. See the note above on why there is no value field.
	var body struct {
		Secrets []struct {
			SecretKey string `json:"secretKey"`
		} `json:"secrets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("failed to decode secret listing: %w", err)
	}

	names := make([]string, 0, len(body.Secrets))
	for _, s := range body.Secrets {
		names = append(names, s.SecretKey)
	}
	sort.Strings(names)
	return names, nil
}
