package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// IdentityClient asks the Tenant Identity Service to provision a tenant's
// identity resources.
//
// The operator ORCHESTRATES; it does not create identity resources itself.
// ADR-041 assigns tenant identity lifecycle to that service and forbids this
// operator from "acting as a secret manager for tenant or application secrets",
// so the client id a tenant's gateway needs is allocated and published there.
// Calling across that boundary is the point: it keeps one component able to talk
// to the issuer, rather than two with their own credentials and their own idea
// of what a tenant looks like.
type IdentityClient struct {
	baseURL string
	http    *http.Client
}

// NewIdentityClient returns nil when no address is configured.
//
// A nil client means "this environment does not provision tenant identity",
// which is a legitimate state — an environment whose issuer has no notion of a
// tenant has nothing to provision. Callers skip on nil rather than failing, so
// enabling this is a configuration change and not a code path that must exist
// everywhere.
func NewIdentityClient(baseURL string) *IdentityClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	return &IdentityClient{
		baseURL: baseURL,
		// Generous but bounded. Provisioning creates several objects at the
		// issuer in sequence, so it is slower than a health check — but this runs
		// inside a reconcile, and an unbounded call would wedge the worker rather
		// than fail and requeue.
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// TenantIdentity is what the identity service reports back.
// OAuthClient is one client a FLEET declares (ADR-053).
//
// It carries no credential. The fleet says what it needs -- a name, whether the
// client authenticates with a secret, and where its callback lives -- and the
// identity service provisions it against the issuer and publishes the
// identifiers the issuer allocates. Keeping the declaration on this side of the
// boundary is what stops the platform assuming a client set (ADR-047).
type OAuthClient struct {
	// Name is unique within the tenant. The client is registered under
	// "<tenantId>-<Name>".
	Name string

	// Confidential is true when the client authenticates with a secret. A public
	// client uses PKCE and has no credential to store.
	Confidential bool

	// RedirectURIs are the client's callbacks: each declared path on each of the
	// fleet's public hosts, resolved here because this side knows the hosts. An
	// issuer matches redirect URIs EXACTLY, so a client provisioned without its
	// callback fails at the authorization endpoint before any credential is
	// entered.
	RedirectURIs []string

	// ActsForUser marks the confidential client the application's backend
	// obtains its users' tokens with (ADR-110). Its id is what a target admits
	// as a caller that carries a USER. Not sent to the identity service: it is
	// how this operator renders the targets' caller maps, not a property of the
	// registration.
	ActsForUser bool

	// TokenExchange enables the RFC 8693 grant on this client (ADR-095). Set by
	// the PLATFORM for the gateway's exchange client and for nothing else; a
	// fleet cannot ask for it, because declaredOAuthClients never reads it.
	TokenExchange bool
}

type TenantIdentity struct {
	TenantRef  string `json:"tenantRef"`
	ProjectRef string `json:"projectRef"`
	ClientID   string `json:"clientId"`

	// OwnerPassword is set only on the call that created the owner's account.
	// Empty means no new credential exists — not that the password is unknown —
	// so it must never be persisted as an empty value over a stored one.
	OwnerPassword string `json:"ownerPassword,omitempty"`

	// Incomplete says what did not finish. Empty means the tenant is usable.
	//
	// It arrives on a SUCCESSFUL response, because the identifiers above are
	// real and must be published even when a later step failed. A tenant is
	// then provisioned and unusable at the same time -- its organisation
	// exists, its application exists, its hostname serves, and a login is
	// refused -- which is a state nothing else reports.
	Incomplete []string `json:"incomplete,omitempty"`

	// Clients are the confidential OAuth clients the fleet declared, with the
	// credentials the issuer minted. Returned rather than stored by the identity
	// service: it provisions at the issuer and this operator owns where secrets
	// live, the same division OwnerPassword already follows.
	Clients []DeclaredClient `json:"clients,omitempty"`

	// ServiceClient is this application's machine identity, which it calls other
	// applications as (ADR-097). Nil when the application declared no
	// cross-application dependencies.
	//
	// ClientID is present on every reconcile once the identity exists; it is what
	// each TARGET allowlists, and a reconcile that omitted it would look like a
	// caller that no longer exists. ClientSecret is present only on the call that
	// minted it, exactly as for a confidential client.
	ServiceClient *ServiceClient `json:"serviceClient,omitempty"`
}

// ServiceClient is the client-credentials identity behind a cross-application
// call. Its subject is the SERVICE; nothing may resolve it to a person.
type ServiceClient struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// EnsureTenantIdentity is idempotent, so it is safe on every reconcile.
// projectName and knownProjectID select the APPLICATION's project (ADR-094):
// knownProjectID is the immutable binding and wins; otherwise projectName is
// found or created inside the bound organisation; both empty keeps the
// application on the legacy shared project.
func (c *IdentityClient) EnsureTenantIdentity(ctx context.Context, tenantID, knownOrgID, projectName, knownProjectID, ownerEmail string, selfRegistration bool, redirectURIs, postLogoutURIs []string, oauthClients []OAuthClient, serviceClientName string) (*TenantIdentity, error) {
	// Sent as declared. The service provisions exactly this set and no more, so a
	// fleet that declares nothing gets no clients rather than a default one.
	clients := make([]map[string]any, 0, len(oauthClients))
	for _, oc := range oauthClients {
		clients = append(clients, map[string]any{
			"name":          oc.Name,
			"confidential":  oc.Confidential,
			"redirectUris":  oc.RedirectURIs,
			"tokenExchange": oc.TokenExchange,
		})
	}

	body, err := json.Marshal(map[string]any{
		"redirectUris":     redirectURIs,
		"postLogoutUris":   postLogoutURIs,
		"knownOrgId":       knownOrgID,
		"projectName":      projectName,
		"knownProjectId":   knownProjectID,
		"ownerEmail":       ownerEmail,
		"selfRegistration": selfRegistration,
		"oauthClients":     clients,
		// Empty when the application declares no backend dependencies, and the
		// service then provisions no machine identity. An unused machine
		// credential is a standing credential nobody rotates.
		"serviceClientName": serviceClientName,
	})
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/internal/tenants/%s/identity", c.baseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)

	// 501 means the configured provider cannot provision tenants. That is a
	// configuration answer, not a transient one, so it is distinguishable here —
	// a caller that retried it would spin forever against a service behaving
	// exactly as configured.
	if resp.StatusCode == http.StatusNotImplemented {
		return nil, ErrIdentityProvisioningUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("identity service: POST %s -> %d: %s", url, resp.StatusCode, msg)
	}

	var out TenantIdentity
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("identity service: decode response: %w", err)
	}
	if out.ClientID == "" {
		return nil, fmt.Errorf("identity service: reported success with no client id for tenant %q", tenantID)
	}
	return &out, nil
}

// EnsurePlatformProject provisions a project in the platform's own
// organisation and returns its organisation and project ids (ADR-109): the
// audience a platform capability admits its one caller by.
func (c *IdentityClient) EnsurePlatformProject(ctx context.Context, name string) (orgID, projectID string, err error) {
	body, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		return "", "", err
	}

	url := fmt.Sprintf("%s/internal/platform/projects", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("identity service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotImplemented {
		return "", "", ErrIdentityProvisioningUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", "", fmt.Errorf("identity service: POST %s -> %d: %s", url, resp.StatusCode, msg)
	}

	var out struct {
		OrgID     string `json:"orgId"`
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("identity service: decode response: %w", err)
	}
	if out.OrgID == "" || out.ProjectID == "" {
		return "", "", fmt.Errorf("identity service: reported success with no organisation or project for platform project %q", name)
	}
	return out.OrgID, out.ProjectID, nil
}

// EnsurePlatformProjectAccess admits the platform's own people to a platform
// project: the named role exists on it, and every human member of the platform
// organisation holding no grant there is given that role (ADR-078 add.5 §7).
//
// Called for the observability project and no other. Every project this
// platform creates carries projectRoleCheck, so the issuer refuses a token for
// an application inside it to anyone holding no role -- which is exactly what
// makes an `aud` claim naming the observability project mean "someone granted
// observability access", and exactly what locks the box's own operators out of
// Grafana until this runs. The refusal arrives at the issuer, so neither
// Grafana nor the gateway can explain it.
func (c *IdentityClient) EnsurePlatformProjectAccess(ctx context.Context, orgID, projectID, roleKey, roleDisplay string) error {
	body, err := json.Marshal(map[string]any{
		"orgId":       orgID,
		"projectId":   projectID,
		"roleKey":     roleKey,
		"roleDisplay": roleDisplay,
	})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/internal/platform/projects/access", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("identity service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotImplemented {
		return ErrIdentityProvisioningUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("identity service: POST %s -> %d: %s", url, resp.StatusCode, msg)
	}
	return nil
}

// ErrIdentityProvisioningUnsupported is returned when the configured identity
// provider has no tenant to provision. Sentinel so a caller can treat it as a
// steady state rather than a failure to retry.
var ErrIdentityProvisioningUnsupported = fmt.Errorf("identity service: provider does not provision tenant identities")

// DeclaredClient is one confidential OAuth client and its minted credentials.
type DeclaredClient struct {
	Name         string `json:"name"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// EnsurePlatformConfidentialClient provisions a platform capability's
// confidential OIDC client in a project the caller has already resolved, and
// returns its credentials (ADR-078 add.5 §7).
//
// The secret comes back ONLY when the issuer just minted it: it discloses a
// secret once, so an existing client returns an empty one and the caller keeps
// what it published. That is why this never asks for regeneration — rotating on
// a reconcile would invalidate the credential the running workload holds, and
// the failure would land on the next sign-in rather than here.
func (c *IdentityClient) EnsurePlatformConfidentialClient(
	ctx context.Context, orgID, projectID, name string, redirectURIs []string,
) (clientID, clientSecret string, err error) {
	body, err := json.Marshal(map[string]any{
		"orgId":        orgID,
		"projectId":    projectID,
		"name":         name,
		"redirectUris": redirectURIs,
	})
	if err != nil {
		return "", "", err
	}

	url := fmt.Sprintf("%s/internal/platform/clients", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("identity service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotImplemented {
		return "", "", ErrIdentityProvisioningUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", "", fmt.Errorf("identity service: POST %s -> %d: %s", url, resp.StatusCode, msg)
	}

	var out struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("identity service: decode response: %w", err)
	}
	if out.ClientID == "" {
		return "", "", fmt.Errorf("identity service: reported success with no client id for platform client %q", name)
	}
	return out.ClientID, out.ClientSecret, nil
}
