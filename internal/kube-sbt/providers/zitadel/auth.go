package zitadel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// Auth implements interfaces.IAuth against an issuer with structural tenancy.
type Auth struct {
	cfg Config
	api *apiClient
	jwt *jwtValidator
}

// NewAuth constructs the provider. It performs no I/O: a provider that reached
// out at construction would make the API server's startup depend on the issuer
// being up, turning a slow identity service into a crash loop.
func NewAuth(cfg Config) (*Auth, error) {
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Auth{
		cfg: cfg,
		api: &apiClient{
			base:  cfg.Issuer,
			token: cfg.ServiceToken,
			http:  &http.Client{Timeout: cfg.HTTPTimeout},
		},
		jwt: newJWTValidator(cfg),
	}, nil
}

// ─── User management ─────────────────────────────────────────────────────────

// CreateUser creates a human user INSIDE a tenant's organisation.
//
// TenantID is required and there is no default. The organisation owns the user,
// so creating one without naming the tenant does not produce a tenant-less user
// — it produces a user in the SERVICE ACCOUNT's organisation, which is the
// platform's. That is a cross-tenant write dressed as a missing field, so it is
// rejected here rather than resolved.
func (a *Auth) CreateUser(ctx context.Context, user models.User) (*models.User, error) {
	if user.Email == "" {
		return nil, fmt.Errorf("zitadel: email is required")
	}
	if user.TenantID == "" {
		return nil, fmt.Errorf("zitadel: tenant is required to create a user; the organisation owns the identity")
	}

	given, family := splitName(user.Name)
	body := map[string]any{
		"username":     user.Email,
		"organization": map[string]any{"orgId": user.TenantID},
		"profile":      map[string]any{"givenName": given, "familyName": family},
		"email":        map[string]any{"email": user.Email, "isVerified": true},
	}
	if user.Password != "" {
		// changeRequired, because the password the platform supplies here is a
		// BOOTSTRAP credential and must never become the account's standing one.
		//
		// Tenant owner provisioning no longer supplies one at all — it creates the
		// account passwordless so the issuer mails an initialisation link
		// (tenant.go, ADR-060 Amendment 2026-09-26). This branch remains for any
		// caller that does supply a password, and for those it must not become the
		// standing credential. With changeRequired false — as this read until
		// 2026-09-26 — a supplied string IS the account's password forever: ZITADEL
		// never prompts, so the only route in is for someone to retrieve that
		// string on the user's behalf, and the account accumulates no
		// `user.human.password.changed` event for as long as it exists.
		//
		// Observed exactly that way. A tenant owner created 2026-09-22 had the
		// bootstrap hash still in place four days later, one successful password
		// check in its entire history, and nothing but
		// `user.human.password.check.failed` after it. A browser that still held
		// the session minted by that one success kept working, which made the
		// account look intermittently broken rather than permanently un-enterable
		// — the sessions hid it, and only a device with no cookie exposed it.
		//
		// true hands the lifecycle back to ZITADEL: first login must set a
		// password the user chose, and the bootstrap value is spent.
		body["password"] = map[string]any{"password": user.Password, "changeRequired": true}
	}

	var created struct {
		UserID string `json:"userId"`
	}
	if err := a.api.do(ctx, http.MethodPost, "/v2/users/human", user.TenantID, body, &created); err != nil {
		return nil, err
	}

	// A role grant is part of creating a usable user, not a later step.
	//
	// The tenant's project denies authentication to anyone holding no role in it
	// (projectRoleCheck), so a user created without one is not a user with less
	// access — it is a user who cannot log in at all, and the failure appears at
	// their first sign-in rather than here. Granting it now keeps "created" and
	// "able to authenticate" the same event.
	roles := user.Roles
	if len(roles) == 0 {
		roles = []string{defaultUserRole}
	}
	projectID, err := a.ensureProject(ctx, user.TenantID, a.cfg.ProjectName)
	if err != nil {
		return nil, fmt.Errorf("zitadel: locate project for %q: %w", user.TenantID, err)
	}
	if err := a.GrantRole(ctx, user.TenantID, projectID, created.UserID, roles); err != nil {
		return nil, fmt.Errorf("zitadel: grant %v to %s: %w", roles, created.UserID, err)
	}

	out := user
	out.ID = created.UserID
	out.Roles = roles
	out.Active = true
	out.Password = ""
	return &out, nil
}

func (a *Auth) GetUser(ctx context.Context, userID string) (*models.User, error) {
	var resp struct {
		User struct {
			UserID  string `json:"userId"`
			Details struct {
				ResourceOwner string `json:"resourceOwner"`
			} `json:"details"`
			Human struct {
				Profile struct {
					GivenName  string `json:"givenName"`
					FamilyName string `json:"familyName"`
				} `json:"profile"`
				Email struct {
					Email string `json:"email"`
				} `json:"email"`
			} `json:"human"`
			State string `json:"state"`
		} `json:"user"`
	}
	if err := a.api.do(ctx, http.MethodGet, "/v2/users/"+userID, "", nil, &resp); err != nil {
		return nil, err
	}
	u := resp.User
	return &models.User{
		ID:    u.UserID,
		Email: u.Human.Email.Email,
		Name:  strings.TrimSpace(u.Human.Profile.GivenName + " " + u.Human.Profile.FamilyName),
		// The resource owner IS the tenant. Read from the identity rather than
		// from a claim on it, which is what makes it impossible to be absent.
		TenantID: u.Details.ResourceOwner,
		Active:   u.State == "USER_STATE_ACTIVE",
	}, nil
}

func (a *Auth) UpdateUser(ctx context.Context, userID string, updates models.UserUpdates) (*models.User, error) {
	body := map[string]any{}
	if updates.Name != nil {
		given, family := splitName(*updates.Name)
		body["profile"] = map[string]any{"givenName": given, "familyName": family}
	}
	if updates.Email != nil {
		body["email"] = map[string]any{"email": *updates.Email, "isVerified": true}
	}
	if len(body) > 0 {
		if err := a.api.do(ctx, http.MethodPut, "/v2/users/human/"+userID, "", body, nil); err != nil {
			return nil, err
		}
	}
	// Roles are not set here. Authorisation is a GRANT within a project, not a
	// property of the user, so writing it through a user update would put the
	// platform's model back on top of the provider's — see GrantRole.
	return a.GetUser(ctx, userID)
}

func (a *Auth) DeleteUser(ctx context.Context, userID string) error {
	return a.api.do(ctx, http.MethodDelete, "/v2/users/"+userID, "", nil, nil)
}

// ListUsers returns the users of ONE tenant.
//
// tenantID is required. An unscoped listing across every organisation is exactly
// the cross-tenant read this service exists to prevent, so absence is an error
// rather than "all".
func (a *Auth) ListUsers(ctx context.Context, tenantID, page, pageSize string) ([]models.User, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("zitadel: tenant is required to list users")
	}
	var resp struct {
		Result []struct {
			UserID  string `json:"userId"`
			Details struct {
				ResourceOwner string `json:"resourceOwner"`
			} `json:"details"`
			Human struct {
				Profile struct {
					GivenName  string `json:"givenName"`
					FamilyName string `json:"familyName"`
				} `json:"profile"`
				Email struct {
					Email string `json:"email"`
				} `json:"email"`
			} `json:"human"`
			State string `json:"state"`
		} `json:"result"`
	}
	q := map[string]any{
		"query":   map[string]any{"limit": pageLimit(pageSize)},
		"queries": []any{map[string]any{"organizationIdQuery": map[string]any{"organizationId": tenantID}}},
	}
	if err := a.api.do(ctx, http.MethodPost, "/v2/users", tenantID, q, &resp); err != nil {
		return nil, err
	}
	out := make([]models.User, 0, len(resp.Result))
	for _, u := range resp.Result {
		out = append(out, models.User{
			ID:       u.UserID,
			Email:    u.Human.Email.Email,
			Name:     strings.TrimSpace(u.Human.Profile.GivenName + " " + u.Human.Profile.FamilyName),
			TenantID: u.Details.ResourceOwner,
			Active:   u.State == "USER_STATE_ACTIVE",
		})
	}
	return out, nil
}

// SetUserGroups maps the platform's group vocabulary onto project roles.
//
// The issuer has no "groups". It has roles granted within a project, which is
// the same idea expressed once instead of twice — so this translates rather than
// storing a parallel list on the identity. Storing one is what the removed
// metadata reconciler did, and it drifted from the provider's own model because
// nothing kept the two in step.
func (a *Auth) SetUserGroups(ctx context.Context, email string, groups []string) error {
	u, err := a.findUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	projectID, err := a.ensureProject(ctx, u.TenantID, a.cfg.ProjectName)
	if err != nil {
		return err
	}
	return a.GrantRole(ctx, u.TenantID, projectID, u.ID, groups)
}

// CreateAdminUser bootstraps a platform administrator.
//
// The admin belongs to the platform's OWN organisation and carries it as their
// tenant, like any other user. There is no tenant-less identity here and so no
// exemption anywhere else — the rule that every token carries a tenant holds
// without a special case (ADR-059).
//
// Idempotent: it runs on every control-plane start, so an existing admin is a
// success rather than a conflict.
func (a *Auth) CreateAdminUser(ctx context.Context, props models.CreateAdminUserProps) error {
	if props.Email == "" {
		return fmt.Errorf("zitadel: admin email is required")
	}
	orgID, err := a.platformOrg(ctx)
	if err != nil {
		return err
	}

	existing, err := a.findUserByEmail(ctx, props.Email)
	if err != nil && !isNotFound(err) {
		return err
	}

	if existing == nil {
		if _, cerr := a.CreateUser(ctx, models.User{
			Email:    props.Email,
			Name:     props.Name,
			TenantID: orgID,
			Roles:    []string{"admin"},
		}); cerr != nil {
			return cerr
		}
		return nil
	}

	// The administrator exists — ensure the GRANT anyway.
	//
	// Returning early here was wrong. An account created by the issuer's own
	// installation, or by an operator before this ran, holds no role in the
	// platform's project, and the issuer denies authentication to a user with no
	// role — so the administrator existed and could not sign in to anything the
	// platform owns, including the Kubernetes API server.
	//
	// Bootstrap runs on every start precisely so it converges; stopping at
	// "exists" made it converge on presence rather than on access.
	projectID, perr := a.ensureProject(ctx, orgID, a.cfg.ProjectName)
	if perr != nil {
		return fmt.Errorf("zitadel: locate the platform project: %w", perr)
	}
	if gerr := a.GrantRole(ctx, orgID, projectID, existing.ID, []string{"admin"}); gerr != nil {
		return fmt.Errorf("zitadel: grant the administrator access: %w", gerr)
	}
	return a.EnsureOrgOwner(ctx, orgID, existing.ID)
}

// ─── Discovery ───────────────────────────────────────────────────────────────

// GetWellKnownEndpoint returns the discovery URL, or "" when unconfigured.
// Readiness reports on it, so it must not error.
func (a *Auth) GetWellKnownEndpoint() string {
	if a.cfg.Issuer == "" {
		return ""
	}
	return a.cfg.Issuer + "/.well-known/openid-configuration"
}

// ─── Sessions ────────────────────────────────────────────────────────────────

// ValidateSession verifies a token and reports the identity it carries.
//
// The tenant is read from the issuer's resource-owner claim, and roles are kept
// only where they were granted in THAT tenant — a role granted in another
// organisation is not a role here, and flattening the claim would leak it across
// tenants while looking correct.
func (a *Auth) ValidateSession(ctx context.Context, token string) (*models.Session, error) {
	raw, err := a.jwt.validate(ctx, token)
	if err != nil {
		return nil, err
	}

	userID, _ := raw["sub"].(string)
	tenantID := tenantFromClaims(raw)
	roles := rolesGrantedInTenant(raw, tenantID)

	var expiresAt, issuedAt time.Time
	if exp, ok := raw["exp"].(float64); ok {
		expiresAt = time.Unix(int64(exp), 0)
	}
	if iat, ok := raw["iat"].(float64); ok {
		issuedAt = time.Unix(int64(iat), 0)
	}

	return &models.Session{
		ID: userID,
		Identity: models.Identity{
			ID: userID,
			Traits: map[string]interface{}{
				"tenant_id": tenantID,
				"roles":     roles,
			},
		},
		ExpiresAt: expiresAt,
		IssuedAt:  issuedAt,
	}, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (a *Auth) findUserByEmail(ctx context.Context, email string) (*models.User, error) {
	var resp struct {
		Result []struct {
			UserID  string `json:"userId"`
			Details struct {
				ResourceOwner string `json:"resourceOwner"`
			} `json:"details"`
		} `json:"result"`
	}
	q := map[string]any{
		"queries": []any{map[string]any{"emailQuery": map[string]any{"emailAddress": email, "method": "TEXT_QUERY_METHOD_EQUALS"}}},
	}
	if err := a.api.do(ctx, http.MethodPost, "/v2/users", "", q, &resp); err != nil {
		return nil, err
	}
	if len(resp.Result) == 0 {
		return nil, &apiError{Status: http.StatusNotFound, Method: "POST", Path: "/v2/users", Body: "no user with that email"}
	}
	return &models.User{ID: resp.Result[0].UserID, Email: email, TenantID: resp.Result[0].Details.ResourceOwner}, nil
}

func splitName(name string) (given, family string) {
	name = strings.TrimSpace(name)
	if name == "" {
		// The issuer rejects an empty given name, so a user created without one
		// fails validation rather than arriving nameless.
		return "-", "-"
	}
	parts := strings.Fields(name)
	if len(parts) == 1 {
		return parts[0], "-"
	}
	return parts[0], strings.Join(parts[1:], " ")
}

func pageLimit(pageSize string) int {
	switch pageSize {
	case "", "0":
		return 100
	}
	n := 0
	for _, c := range pageSize {
		if c < '0' || c > '9' {
			return 100
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 || n > 1000 {
		return 100
	}
	return n
}

var _ interfaces.IAuth = (*Auth)(nil)
