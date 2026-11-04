package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/constant"
	"time"
)

// BootstrapAPI handles low-level Infisical API calls for bootstrap
type BootstrapAPI struct {
	baseURL    string
	httpClient *http.Client
}

// NewBootstrapAPI creates a new bootstrap API client
func NewBootstrapAPI() *BootstrapAPI {
	return &BootstrapAPI{
		baseURL: InfisicalBaseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ProjectResponse represents the response from /api/v1/projects
type ProjectResponse struct {
	Project struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"project"`
}

// LoginResponse represents the response from /api/v3/auth/login
type LoginResponse struct {
	AccessToken string `json:"accessToken"`
}

// SelectOrganizationResponse represents the response from /api/v3/auth/select-organization
type SelectOrganizationResponse struct {
	Token string `json:"token"`
}

// Login calls /api/v3/auth/login to get initial token
func (api *BootstrapAPI) Login(ctx context.Context, email, password string) (*LoginResponse, error) {
	payload := map[string]string{
		"email":    email,
		"password": password,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+constant.APIEndpointLogin, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var loginResp LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return nil, fmt.Errorf("failed to decode login response: %w", err)
	}

	return &loginResp, nil
}

// SelectOrganization calls /api/v3/auth/select-organization to get org-scoped token
func (api *BootstrapAPI) SelectOrganization(ctx context.Context, token, orgID string) (*SelectOrganizationResponse, error) {
	payload := map[string]string{
		"organizationId": orgID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal select organization request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+constant.APIEndpointSelectOrg, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create select organization request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute select organization request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("select organization failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var selectOrgResp SelectOrganizationResponse
	if err := json.NewDecoder(resp.Body).Decode(&selectOrgResp); err != nil {
		return nil, fmt.Errorf("failed to decode select organization response: %w", err)
	}

	return &selectOrgResp, nil
}

// CreateProject creates a new project
func (api *BootstrapAPI) CreateProject(ctx context.Context, adminToken, projectName, orgID string) (*ProjectResponse, error) {
	payload := map[string]string{
		"projectName":    projectName,
		"organizationId": orgID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal project request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+constant.APIEndpointProjects, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create project request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute project request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create project failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var projectResp ProjectResponse
	if err := json.NewDecoder(resp.Body).Decode(&projectResp); err != nil {
		return nil, fmt.Errorf("failed to decode project response: %w", err)
	}

	return &projectResp, nil
}

// UpdateProjectSlug updates the project slug to match the desired constant
func (api *BootstrapAPI) UpdateProjectSlug(ctx context.Context, adminToken, projectID, newSlug string) error {
	payload := map[string]string{
		"slug": newSlug,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal update slug request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "PATCH", api.baseURL+constant.APIEndpointProjects+"/"+projectID, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create update slug request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute update slug request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update slug failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// IdentityResponse represents machine identity creation response
// API returns the identity wrapped in an "identity" field
type IdentityResponse struct {
	Identity struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"identity"`
}

// CreateIdentity creates a machine identity
func (api *BootstrapAPI) CreateIdentity(ctx context.Context, adminToken, identityName, orgID string) (*IdentityResponse, error) {
	payload := map[string]string{
		"name":           identityName,
		"organizationId": orgID,
		"role":           "admin",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal identity request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+constant.APIEndpointIdentities, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create identity request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute identity request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create identity failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var identityResp IdentityResponse
	if err := json.NewDecoder(resp.Body).Decode(&identityResp); err != nil {
		return nil, fmt.Errorf("failed to decode identity response: %w", err)
	}

	return &identityResp, nil
}

// AttachUniversalAuth attaches Universal Auth to machine identity
func (api *BootstrapAPI) AttachUniversalAuth(ctx context.Context, adminToken, identityID string) error {
	// Use default values for Universal Auth configuration
	payload := map[string]interface{}{
		"clientSecretTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTrustedIps": []map[string]string{
			{"ipAddress": "0.0.0.0/0"},
			{"ipAddress": "::/0"},
		},
		"accessTokenTTL":             2592000, // 30 days
		"accessTokenMaxTTL":          2592000, // 30 days
		"accessTokenNumUsesLimit":    0,
		"accessTokenPeriod":          0,
		"lockoutEnabled":             true,
		"lockoutThreshold":           3,
		"lockoutDurationSeconds":     300,
		"lockoutCounterResetSeconds": 30,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal attach auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+constant.APIEndpointUniversalAuth+"/"+identityID, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create attach auth request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute attach auth request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	// Handle idempotency: if already configured, treat as success
	if resp.StatusCode == http.StatusBadRequest && bytes.Contains(bodyBytes, []byte("already configured")) {
		return nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("attach universal auth failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// ClientCredentialsResponse represents client credentials generation response
type ClientCredentialsResponse struct {
	ClientSecret     string `json:"clientSecret"`
	ClientSecretData struct {
		ID string `json:"id"`
	} `json:"clientSecretData"`
}

// UniversalAuthResponse represents the universal auth configuration
type UniversalAuthResponse struct {
	IdentityUniversalAuth struct {
		ClientID string `json:"clientId"`
	} `json:"identityUniversalAuth"`
}

// GetUniversalAuth retrieves the universal auth configuration to get clientId
func (api *BootstrapAPI) GetUniversalAuth(ctx context.Context, adminToken, identityID string) (*UniversalAuthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", api.baseURL+constant.APIEndpointUniversalAuth+"/"+identityID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create get universal auth request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute get universal auth request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get universal auth failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var uaResp UniversalAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&uaResp); err != nil {
		return nil, fmt.Errorf("failed to decode universal auth response: %w", err)
	}

	return &uaResp, nil
}

// GenerateClientCredentials generates client credentials for machine identity
func (api *BootstrapAPI) GenerateClientCredentials(ctx context.Context, adminToken, identityID string) (*ClientCredentialsResponse, error) {
	// Use default values for client secret configuration
	payload := map[string]interface{}{
		"description":  "",
		"numUsesLimit": 0,
		"ttl":          0,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal client credentials request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+constant.APIEndpointUniversalAuth+"/"+identityID+constant.APIEndpointClientSecrets, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create client credentials request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute client credentials request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("generate client credentials failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var credsResp ClientCredentialsResponse
	if err := json.NewDecoder(resp.Body).Decode(&credsResp); err != nil {
		return nil, fmt.Errorf("failed to decode client credentials response: %w", err)
	}

	return &credsResp, nil
}

// GrantProjectAccess grants machine identity access to project with role
func (api *BootstrapAPI) GrantProjectAccess(ctx context.Context, adminToken, projectID, identityID, role string) error {
	payload := map[string]string{
		"role": role,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal grant access request: %w", err)
	}

	url := fmt.Sprintf(api.baseURL+constant.APIEndpointProjectMemberships, projectID, identityID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create grant access request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute grant access request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("grant project access failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// // GetUserByEmail retrieves user information by email
// func (api *BootstrapAPI) GetUserByEmail(ctx context.Context, adminToken, email string) (string, error) {
// 	req, err := http.NewRequestWithContext(ctx, "GET", api.baseURL+"/api/v3/users/me", nil)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to create get user request: %w", err)
// 	}

// 	req.Header.Set("Authorization", "Bearer "+adminToken)

// 	resp, err := api.httpClient.Do(req)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to execute get user request: %w", err)
// 	}
// 	defer resp.Body.Close()

// 	if resp.StatusCode != http.StatusOK {
// 		bodyBytes, _ := io.ReadAll(resp.Body)
// 		return "", fmt.Errorf("get user failed with status %d: %s", resp.StatusCode, string(bodyBytes))
// 	}

// 	var userResp struct {
// 		User struct {
// 			Username string `json:"username"`
// 		} `json:"user"`
// 	}

// 	if err := json.NewDecoder(resp.Body).Decode(&userResp); err != nil {
// 		return "", fmt.Errorf("failed to decode user response: %w", err)
// 	}

// 	return userResp.User.Username, nil
// }

// AddUserToProject adds a user to a project with specified roles
func (api *BootstrapAPI) AddUserToProject(ctx context.Context, adminToken, projectID, username string, roles []string) error {
	payload := map[string]interface{}{
		"usernames": []string{username},
		"emails":    []string{},
		"roleSlugs": roles,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal add user request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+fmt.Sprintf(constant.APIEndpointProjectUserMemberships, projectID), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create add user request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute add user request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add user to project failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// AddUserToProjectByEmail adds a user to a project by email with specified roles
func (api *BootstrapAPI) AddUserToProjectByEmail(ctx context.Context, adminToken, projectID, email string, roles []string) error {
	payload := map[string]interface{}{
		"usernames": []string{},
		"emails":    []string{email},
		"roleSlugs": roles,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal add user request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", api.baseURL+fmt.Sprintf(constant.APIEndpointProjectUserMemberships, projectID), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create add user request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute add user request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add user to project failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}
