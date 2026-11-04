package zitadel

import (
	"context"
	"fmt"
	"net/http"

	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// An application calling another application authenticates as ITSELF, with OAuth
// 2.0 client credentials (ADR-097). This provisions the identity that flow needs:
// a machine user in the caller's organisation, holding a client id and secret.
//
// # WHY A MACHINE USER AND NOT ANOTHER CONFIDENTIAL APPLICATION
//
// EnsureConfidentialClient creates an OIDC application, which authenticates a
// CLIENT acting for a user -- the authorization code it completes belongs to a
// person who logged in. A service-to-service call has no person, and modelling
// one produces a token whose `sub` is a user that nobody is. A machine user's
// subject IS the service, which is what ADR-097 invariant 8 requires: nothing may
// map a machine token's subject to a person.
//
// Verified against v4.15.3, the deployed release, and not against the checkout's
// default branch -- that mistake is the one ADR-097 exists to record:
//
//	POST /management/v1/users/machine    AddMachineUserRequest{user_name, name,
//	                                     description, access_token_type}
//	PUT  /management/v1/users/{id}/secret  -> {client_id, client_secret}
//	PUT  /management/v1/users/{id}/machine  UpdateMachineRequest{..., access_token_type}
//	POST /v2/users                       search, userNameQuery + organizationIdQuery
//
// AddMachineUser is marked deprecated in the v4.15.3 proto's OpenAPI options. It
// is what that release serves, and the v2 replacement does not cover machine
// users there. Read the source of the version that is deployed.
const (
	accessTokenTypeJWT    = "ACCESS_TOKEN_TYPE_JWT"
	accessTokenTypeBearer = "ACCESS_TOKEN_TYPE_BEARER"
)

// EnsureMachineClient provisions the caller's service identity in orgID.
//
// userName is the machine user's login name and must be unique within the
// organisation; it is not the client id. Zitadel allocates the client id when the
// secret is generated, and the two are unrelated strings -- so the id the
// RECEIVER allowlists comes from this call's result and can never be spelled out
// in a manifest ahead of time (ADR-097 invariant 6).
//
// THE TOKEN TYPE IS ASSERTED, NOT ASSUMED. Zitadel's default is
// ACCESS_TOKEN_TYPE_BEARER, which is opaque: a receiver holding one cannot
// validate it locally and must introspect, putting the issuer in the path of
// every cross-application request. A machine user created before this code
// existed, or by hand, carries that default. So an existing user is read back and
// CORRECTED when it is wrong, rather than trusted -- the failure of an opaque
// token is a receiver rejecting a structurally valid credential, which reads as
// an authorization bug rather than a provisioning one.
func (a *Auth) EnsureMachineClient(ctx context.Context, orgID, userName, displayName string, regenerateIfExists bool) (*models.MachineClient, error) {
	if orgID == "" {
		return nil, fmt.Errorf("zitadel: a machine client needs the application's resolved organisation; provision its identity first")
	}
	if userName == "" {
		return nil, fmt.Errorf("zitadel: userName is required for a machine client")
	}
	if displayName == "" {
		displayName = userName
	}

	userID, tokenType, hasSecret, err := a.findMachineUser(ctx, orgID, userName)
	if err != nil {
		return nil, err
	}

	if userID == "" {
		userID, err = a.createMachineUser(ctx, orgID, userName, displayName)
		if err != nil {
			return nil, err
		}
		// A user that has just been created has no secret, whatever the caller
		// asked: without one there is no credential and the whole call was
		// pointless.
		return a.generateMachineSecret(ctx, orgID, userID)
	}

	if tokenType != accessTokenTypeJWT {
		// Correcting this does not invalidate the secret; it changes the shape of
		// the tokens the issuer mints from it next.
		if err := a.setMachineTokenTypeJWT(ctx, orgID, userID, userName, displayName); err != nil {
			return nil, err
		}
	}

	if !hasSecret {
		// A machine user with no secret cannot authenticate at all. This is not a
		// regeneration -- there is nothing to invalidate -- so it happens
		// regardless of regenerateIfExists, which governs REPLACING a credential a
		// workload may be holding.
		return a.generateMachineSecret(ctx, orgID, userID)
	}
	if regenerateIfExists {
		return a.generateMachineSecret(ctx, orgID, userID)
	}

	// The secret exists and the issuer will not disclose it. The client id is
	// still needed -- it is what the receiver allowlists -- and it is not readable
	// from the user record, so it must already be held by the caller.
	return &models.MachineClient{UserID: userID}, nil
}

// findMachineUser locates a machine user by login name within one organisation.
//
// Scoped to the organisation deliberately. Login names are unique per
// organisation, not per instance, so an unscoped search on a fleet with more than
// one tenant can return another tenant's service identity -- and the caller would
// then publish ITS client id as an allowed caller.
func (a *Auth) findMachineUser(ctx context.Context, orgID, userName string) (userID, tokenType string, hasSecret bool, err error) {
	var resp struct {
		Result []struct {
			UserID  string `json:"userId"`
			Machine *struct {
				AccessTokenType string `json:"accessTokenType"`
				HasSecret       bool   `json:"hasSecret"`
			} `json:"machine"`
		} `json:"result"`
	}
	q := map[string]any{
		"queries": []any{
			map[string]any{"userNameQuery": map[string]any{
				"userName": userName,
				"method":   "TEXT_QUERY_METHOD_EQUALS",
			}},
			map[string]any{"organizationIdQuery": map[string]any{"organizationId": orgID}},
			map[string]any{"typeQuery": map[string]any{"type": "TYPE_MACHINE"}},
		},
	}
	if err := a.api.do(ctx, http.MethodPost, "/v2/users", orgID, q, &resp); err != nil {
		if isNotFound(err) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("search machine user %q: %w", userName, err)
	}
	if len(resp.Result) == 0 {
		return "", "", false, nil
	}
	if len(resp.Result) > 1 {
		// Two machine users answering one login name in one organisation means the
		// name is not the identity it is being used as. Picking one would publish
		// an allowlist entry for a credential the caller may not hold.
		return "", "", false, fmt.Errorf(
			"zitadel: %d machine users named %q in organisation %s; the login name must identify exactly one service identity",
			len(resp.Result), userName, orgID)
	}
	r := resp.Result[0]
	if r.Machine == nil {
		// A user answering a TYPE_MACHINE query with no machine block is a human
		// account occupying the name this service needs.
		return "", "", false, fmt.Errorf(
			"zitadel: user %q in organisation %s is not a machine user; a service identity cannot reuse a person's login name",
			userName, orgID)
	}
	return r.UserID, r.Machine.AccessTokenType, r.Machine.HasSecret, nil
}

func (a *Auth) createMachineUser(ctx context.Context, orgID, userName, displayName string) (string, error) {
	var out struct {
		UserID string `json:"userId"`
	}
	body := map[string]any{
		"userName":    userName,
		"name":        displayName,
		"description": "Service identity for cross-application calls (ADR-097)",
		// Set at creation rather than patched afterwards, so there is no window in
		// which this user mints opaque tokens.
		"accessTokenType": accessTokenTypeJWT,
	}
	if err := a.api.do(ctx, http.MethodPost, "/management/v1/users/machine", orgID, body, &out); err != nil {
		return "", fmt.Errorf("create machine user %q: %w", userName, err)
	}
	if out.UserID == "" {
		return "", fmt.Errorf("zitadel: machine user %q created without an id", userName)
	}
	return out.UserID, nil
}

func (a *Auth) setMachineTokenTypeJWT(ctx context.Context, orgID, userID, userName, displayName string) error {
	body := map[string]any{
		"name":            displayName,
		"description":     "Service identity for cross-application calls (ADR-097)",
		"accessTokenType": accessTokenTypeJWT,
	}
	if err := a.api.do(ctx, http.MethodPut,
		"/management/v1/users/"+userID+"/machine", orgID, body, nil); err != nil {
		return fmt.Errorf("set machine user %q to JWT access tokens: %w", userName, err)
	}
	return nil
}

func (a *Auth) generateMachineSecret(ctx context.Context, orgID, userID string) (*models.MachineClient, error) {
	var out struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := a.api.do(ctx, http.MethodPut,
		"/management/v1/users/"+userID+"/secret", orgID, map[string]any{}, &out); err != nil {
		return nil, fmt.Errorf("generate machine secret for user %s: %w", userID, err)
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		// Returning a half-credential would be published and then fail at the token
		// endpoint, far from here.
		return nil, fmt.Errorf("zitadel: machine secret for user %s returned without a client id or secret", userID)
	}
	return &models.MachineClient{UserID: userID, ClientID: out.ClientID, ClientSecret: out.ClientSecret}, nil
}
