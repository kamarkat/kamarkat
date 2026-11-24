package zitadel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// EnsurePublicClient provisions a PUBLIC client -- authorization code with PKCE
// and refresh, no secret -- in a project the caller has already resolved
// (kamarkat ADR-116 §3).
//
// The box's kakt clients are public on purpose. A confidential client's secret
// is minted here, in the box, and would have to be carried to wherever kakt
// runs: a box secret leaving the box (ADR-065). A public client has nothing to
// carry, and its id is not a credential.
//
// `native` is for a client that runs on a person's workstation (`kakt mcp`):
// its redirect is a loopback address, which the issuer admits only for a native
// application. A web client whose redirect URIs include a plain-http address --
// kakt run on a developer's machine -- is put in development mode, which is the
// issuer's switch for accepting one; every https-only client stays out of it.
//
// The redirect URIs are reconciled on every call. They are the tenant's
// declaration of which kakt deployments may receive a code, so a URI removed
// from the declaration stops being one at the issuer on the next reconcile.
func (a *Auth) EnsurePublicClient(ctx context.Context, orgID, projectID, appName string, redirectURIs []string, native bool) (string, error) {
	if orgID == "" || projectID == "" {
		return "", fmt.Errorf("zitadel: a public client needs its resolved organisation and project; ensure the project first")
	}
	if appName == "" {
		return "", fmt.Errorf("zitadel: appName is required")
	}
	if len(redirectURIs) == 0 {
		return "", fmt.Errorf("zitadel: public client %q needs at least one redirect URI: a code flow with nowhere to deliver the code admits nobody", appName)
	}

	appType := "OIDC_APP_TYPE_WEB"
	if native {
		appType = "OIDC_APP_TYPE_NATIVE"
	}
	devMode := false
	for _, u := range redirectURIs {
		if strings.HasPrefix(u, "http://") && !native {
			devMode = true
		}
	}
	grantTypes := []string{"OIDC_GRANT_TYPE_AUTHORIZATION_CODE", "OIDC_GRANT_TYPE_REFRESH_TOKEN"}
	config := map[string]any{
		"redirectUris":             redirectURIs,
		"postLogoutRedirectUris":   []string{},
		"responseTypes":            []string{"OIDC_RESPONSE_TYPE_CODE"},
		"grantTypes":               grantTypes,
		"appType":                  appType,
		"authMethodType":           authMethodNone,
		"accessTokenType":          accessTokenTypeOIDCJWT,
		"accessTokenRoleAssertion": true,
		"idTokenRoleAssertion":     true,
		"idTokenUserinfoAssertion": true,
		"devMode":                  devMode,
	}

	var existing struct {
		Result []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			State      string `json:"state"`
			OIDCConfig struct {
				ClientID     string   `json:"clientId"`
				RedirectURIs []string `json:"redirectUris"`
				AppType      string   `json:"appType"`
				DevMode      bool     `json:"devMode"`
			} `json:"oidcConfig"`
		} `json:"result"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/apps/_search", orgID,
		map[string]any{"query": map[string]any{"limit": 100}}, &existing); err != nil {
		return "", err
	}
	for _, app := range existing.Result {
		if app.Name != appName || app.OIDCConfig.ClientID == "" {
			continue
		}
		if app.State == "APP_STATE_INACTIVE" {
			if err := a.api.do(ctx, http.MethodPost,
				"/management/v1/projects/"+projectID+"/apps/"+app.ID+"/_reactivate",
				orgID, map[string]any{}, nil); err != nil {
				return "", fmt.Errorf("reactivate application %q (%s): %w", appName, app.ID, err)
			}
		}
		if !sameSet(app.OIDCConfig.RedirectURIs, redirectURIs) || app.OIDCConfig.AppType != appType || app.OIDCConfig.DevMode != devMode {
			err := a.api.do(ctx, http.MethodPut,
				"/management/v1/projects/"+projectID+"/apps/"+app.ID+"/oidc_config",
				orgID, config, nil)
			if err != nil && !isUnchanged(err) {
				return "", fmt.Errorf("reconcile configuration of application %q (%s): %w", appName, app.ID, err)
			}
		}
		return app.OIDCConfig.ClientID, nil
	}

	body := map[string]any{"name": appName}
	for k, v := range config {
		body[k] = v
	}
	var created struct {
		ClientID string `json:"clientId"`
	}
	if err := a.api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/apps/oidc", orgID, body, &created); err != nil {
		return "", err
	}
	if created.ClientID == "" {
		return "", fmt.Errorf("zitadel: application %q created without a client id", appName)
	}
	return created.ClientID, nil
}
