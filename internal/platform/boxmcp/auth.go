package boxmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	authproxy "github.com/kamarkat/kamarkat/internal/auth-proxy"
)

// The roles of the kamarkat-mcp project (ADR-116 §3). A token admits a
// category only when it carries that category's role.
const (
	RoleViewer   = "platform.viewer"
	RoleObserver = "platform.observer"
	RoleVerifier = "platform.verifier"
)

// GroupPrefix names the Kubernetes groups a person is impersonated with, one
// per project role their token carries. The capability binds
// `kamarkat-mcp:platform.viewer` to a read-only ClusterRole, so a role granted
// in the box's identity provider is what gives read access -- and revoking it
// there removes it here on the next token.
const GroupPrefix = "kamarkat-mcp:"

// Identity is the person a request is made as.
type Identity struct {
	Subject  string
	Email    string
	ClientID string
	Roles    []string
}

// Has reports whether the identity holds role.
func (i *Identity) Has(role string) bool { return slices.Contains(i.Roles, role) }

// Groups are the Kubernetes groups to impersonate i with.
func (i *Identity) Groups() []string {
	out := make([]string, 0, len(i.Roles))
	for _, r := range i.Roles {
		out = append(out, GroupPrefix+r)
	}
	return out
}

type identityKey struct{}

// WithIdentity stores i in ctx.
func WithIdentity(ctx context.Context, i *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, i)
}

// IdentityFrom is the identity in ctx, or nil.
func IdentityFrom(ctx context.Context) *Identity {
	i, _ := ctx.Value(identityKey{}).(*Identity)
	return i
}

// AuthConfig is what a token must say to be accepted.
type AuthConfig struct {
	// Issuer is the box's identity provider, exactly as tokens name it.
	Issuer string
	// ProjectID is the kamarkat-mcp project: the audience every token must carry.
	ProjectID string
	// AllowedClients are the declared clients whose tokens are accepted (azp).
	AllowedClients []string
	// AgentUserID is the kakt-agent machine user, accepted only while the box
	// declares it. Empty: no agent token is accepted.
	AgentUserID string
}

// AgentUserPrefix names the kakt-agent when it is impersonated: a machine user
// has no email, and the name must not be mistaken for a person's.
const AgentUserPrefix = "kamarkat-mcp-agent:"

// Authenticator validates the bearer token on a request (ADR-116 §3).
type Authenticator struct {
	cfg       AuthConfig
	validator *authproxy.JWTValidator
	http      *http.Client

	mu     sync.Mutex
	emails map[string]cachedEmail
}

type cachedEmail struct {
	email   string
	expires time.Time
}

// NewAuthenticator validates against the issuer's published keys.
func NewAuthenticator(cfg AuthConfig) (*Authenticator, error) {
	if cfg.Issuer == "" || cfg.ProjectID == "" {
		return nil, errors.New("the issuer and the kamarkat-mcp project id are both required: without them no token can be checked")
	}
	cfg.Issuer = strings.TrimSuffix(cfg.Issuer, "/")
	return &Authenticator{
		cfg:       cfg,
		validator: authproxy.NewJWTValidator(cfg.Issuer+"/oauth/v2/keys", cfg.ProjectID, 10*time.Minute, 5*time.Second),
		http:      &http.Client{Timeout: 5 * time.Second},
		emails:    map[string]cachedEmail{},
	}, nil
}

// ErrUnauthenticated is a request with no acceptable token.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticate returns who the request is from.
func (a *Authenticator) Authenticate(r *http.Request) (*Identity, error) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return nil, fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}
	claims, err := a.validator.Validate(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if iss, _ := claims["iss"].(string); strings.TrimSuffix(iss, "/") != a.cfg.Issuer {
		return nil, fmt.Errorf("%w: issued by %q, not this box's identity provider", ErrUnauthenticated, iss)
	}
	// An ID token is not an access token (ADR-095): it carries at_hash.
	if _, isIDToken := claims["at_hash"]; isIDToken {
		return nil, fmt.Errorf("%w: an ID token was presented; an access token is required", ErrUnauthenticated)
	}
	azp, _ := claims["azp"].(string)
	if azp == "" {
		azp, _ = claims["client_id"].(string)
	}
	sub, _ := claims["sub"].(string)
	isAgent := a.cfg.AgentUserID != "" && sub == a.cfg.AgentUserID
	if !isAgent && len(a.cfg.AllowedClients) > 0 && !slices.Contains(a.cfg.AllowedClients, azp) {
		return nil, fmt.Errorf("%w: client %q is not one this box declared for its MCP server", ErrUnauthenticated, azp)
	}
	id := &Identity{Subject: sub, ClientID: azp, Roles: rolesIn(claims, a.cfg.ProjectID)}
	if isAgent {
		// The agent reads as itself, never as a person: platform.viewer is the
		// only role the box grants it.
		id.Email = AgentUserPrefix + sub
		return id, nil
	}
	id.Email, _ = claims["email"].(string)
	if id.Email == "" {
		id.Email, err = a.emailFor(r.Context(), sub, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
		}
	}
	return id, nil
}

// rolesIn reads the project's roles from a Zitadel token: the project-specific
// claim when present, otherwise the generic one.
func rolesIn(claims map[string]any, projectID string) []string {
	for _, key := range []string{"urn:zitadel:iam:org:project:" + projectID + ":roles", "urn:zitadel:iam:org:project:roles"} {
		if m, ok := claims[key].(map[string]any); ok {
			roles := make([]string, 0, len(m))
			for r := range m {
				roles = append(roles, r)
			}
			slices.Sort(roles)
			return roles
		}
	}
	return nil
}

// emailFor reads the person's email from the issuer's userinfo endpoint when the
// access token does not carry it. The API server names a person by email
// (--oidc-username-claim=email), so impersonating anything else would read as
// somebody with no bindings.
func (a *Authenticator) emailFor(ctx context.Context, sub, token string) (string, error) {
	a.mu.Lock()
	if c, ok := a.emails[sub]; ok && time.Now().Before(c.expires) {
		a.mu.Unlock()
		return c.email, nil
	}
	a.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Issuer+"/oidc/v1/userinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("read the person's email from the issuer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the issuer's userinfo answered %d: request the email scope", resp.StatusCode)
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Email == "" {
		return "", errors.New("the issuer returned no email for this person: request the email scope")
	}
	a.mu.Lock()
	a.emails[sub] = cachedEmail{email: info.Email, expires: time.Now().Add(10 * time.Minute)}
	a.mu.Unlock()
	return info.Email, nil
}
