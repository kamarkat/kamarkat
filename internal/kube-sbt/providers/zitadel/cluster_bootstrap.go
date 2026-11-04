// Registering the OIDC client a cluster's API server names, and destroying the
// credential used to do it.
//
// Lives beside the tenant-identity code rather than in a package of its own. It
// was written as internal/kakt-cli/identity and duplicated what is here: a
// second HTTP client against the same API, and a second definition of
// OIDC_AUTH_METHOD_TYPE_NONE twelve directories away from the first. Two
// implementations of one operation is the drift this repository has been paying
// for elsewhere, and the boundary that seemed to justify it -- Day-0 bootstrap
// versus SaaS tenant identity -- is a naming question, not a reason for two
// clients.
//
// ADR-076 separates two planes. Cluster configuration -- which issuer the API
// server trusts -- is desired state in the tenant's repository. Administering the
// identity provider is a different operation with a different, far more
// privileged credential, and the running box must never hold one: a standing
// management credential on the hub would place the identity provider's
// administration inside the system it authenticates, and let any compromise of
// the box rewrite its own login.
//
// So the credential here is borrowed, not kept. It arrives as an input, is used
// once, and is revoked before this returns -- including when registration fails,
// which is the case that matters, because that is the path where a credential is
// most likely to be left behind.
//
// The v2 services are used deliberately. The older Management API endpoints for
// machine users and personal access tokens are deprecated, and v2 makes the
// property this design depends on structural rather than remembered: on
// `AddPersonalAccessToken` the expiration date is a required field, so a token
// without an expiry cannot be created by accident.
package zitadel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OIDC application shape for a cluster client, from the v2 enums.
//
// NATIVE with auth method NONE is the public-client-with-PKCE combination. It is
// correct for kubectl for a reason worth stating: the client runs on every
// operator's laptop, so any secret it held would be copied onto each of them and
// could not be revoked for one person -- which is the property this whole ADR
// exists to obtain.
const (
	// appTypeNative is a CLI, not a browser app. Zitadel then requires PKCE and
	// permits loopback redirects, which is the flow kubelogin uses.
	//
	// authMethodNone is declared with the tenant-identity constants in tenant.go
	// and reused here rather than restated: one spelling of the enum, whoever
	// creates the application.
	appTypeNative    = "OIDC_APP_TYPE_NATIVE"
	grantAuthCode    = "OIDC_GRANT_TYPE_AUTHORIZATION_CODE"
	responseTypeCode = "OIDC_RESPONSE_TYPE_CODE"

	// The CURRENT service. The tenant-identity path above still creates
	// applications through /management/v1, which is deprecated; this one does not
	// inherit that, and migrating the other is a separate change with live
	// callers behind it.
	createApplication = "/zitadel.application.v2.ApplicationService/CreateApplication"
)

// Bootstrap holds the borrowed credential and what it is for.
type Bootstrap struct {
	// Issuer is the identity provider's base URL.
	Issuer string
	// Token is the personal access token this operation borrows. It is revoked
	// before Run returns.
	Token string
	// UserID owns Token; TokenID identifies it. Both are needed to revoke, and a
	// bootstrap that cannot revoke is refused rather than run.
	UserID  string
	TokenID string
	// ProjectID receives the application.
	ProjectID string

	// AccessTokenLifetime bounds how long an access token this instance issues
	// stays usable (ADR-094 requirement 8).
	//
	// Set HERE rather than defaulted in the setter, because it is a security
	// parameter and a default buried in an implementation file is a decision
	// nobody made. Zero leaves the instance as it is, which is what an upgrade of
	// an existing box does until someone chooses a value.
	//
	// Applied during bootstrap because this is the one moment the platform holds
	// an instance-scoped credential: the borrowed token is revoked before Run
	// returns, and no later reconcile has iam.write.
	AccessTokenLifetime time.Duration

	HTTP *http.Client
}

// client borrows the package's API client, so this path and the tenant-identity
// path speak to the issuer the same way.
func (b *Bootstrap) client() *apiClient {
	h := b.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	return &apiClient{base: strings.TrimSuffix(b.Issuer, "/"), token: b.Token, http: h}
}

// Result is what survives the operation: public metadata, and nothing else.
type Result struct {
	ClientID      string
	ApplicationID string
}

// Run registers the application and revokes the credential.
//
// Revocation is deferred, so it happens on every path including a failed
// registration and a cancelled context. Its error is reported even when
// registration succeeded: a run that leaves a live management token behind has
// not done what it was asked, however good the application looks.
func (b *Bootstrap) Run(ctx context.Context, appName string, redirectURIs []string) (res Result, err error) {
	if err := b.validate(); err != nil {
		return Result{}, err
	}

	var lifetimeErr error

	defer func() {
		// Revoked with a context of its own. The caller's may already be
		// cancelled -- which is exactly when a token would otherwise survive.
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if rerr := b.revoke(revokeCtx); rerr != nil {
			if err == nil {
				err = fmt.Errorf("the application was registered but its bootstrap token could not be revoked: %w.\n\n"+
					"Revoke it in the identity provider before this box is considered built:\n"+
					"  user %s, token %s", rerr, b.UserID, b.TokenID)
				return
			}
			err = fmt.Errorf("%w (and the bootstrap token could not be revoked either: %v)", err, rerr)
		}
	}()

	// Before the application, and NOT fatal on failure.
	//
	// Before, because the borrowed token is revoked when Run returns and this is
	// the only instance-scoped credential the platform ever holds. Not fatal,
	// because a box whose tokens live too long is a weaker box, while a box with
	// no OIDC client at all does not work: refusing to register the cluster's
	// client over a lifetime setting would trade the whole login for a hardening
	// step. The error is returned only when registration also fails.
	if b.AccessTokenLifetime > 0 {
		if err := b.EnsureAccessTokenLifetime(ctx, b.AccessTokenLifetime); err != nil {
			lifetimeErr = err
		}
	}

	res, regErr := b.register(ctx, appName, redirectURIs)
	if regErr != nil {
		return res, regErr
	}
	if lifetimeErr != nil {
		return res, fmt.Errorf("the cluster's OIDC client was registered but the instance "+
			"access token lifetime was not set: %w.\n\n"+
			"Tokens keep the issuer's default lifetime until this is applied", lifetimeErr)
	}
	return res, nil
}

func (b *Bootstrap) validate() error {
	var missing []string
	for name, v := range map[string]string{
		"issuer": b.Issuer, "token": b.Token,
		"user id": b.UserID, "token id": b.TokenID, "project id": b.ProjectID,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// The token's identifiers are as required as the token. Without them the
		// credential cannot be revoked, and an unrevokable management token is
		// the one outcome this package exists to prevent -- so it refuses to
		// start rather than discovering it at the end.
		return fmt.Errorf("cannot bootstrap the cluster's OIDC client: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func (b *Bootstrap) register(ctx context.Context, appName string, redirectURIs []string) (Result, error) {
	body := map[string]any{
		"projectId": b.ProjectID,
		"name":      appName,
		"oidcRequest": map[string]any{
			"redirectUris":             redirectURIs,
			"responseTypes":            []string{responseTypeCode},
			"grantTypes":               []string{grantAuthCode},
			"appType":                  appTypeNative,
			"authMethodType":           authMethodNone,
			"devMode":                  false,
			"skipNativeAppSuccessPage": true,
		},
	}

	var out struct {
		ApplicationID     string `json:"applicationId"`
		OIDCConfiguration struct {
			ClientID string `json:"clientId"`
		} `json:"oidcConfiguration"`
	}
	// Through the package's own client: one place that sets the bearer header,
	// applies the timeout and turns a non-2xx into an error naming the body --
	// which is where a missing permission is explained, and a bare 403 is not.
	if err := b.client().do(ctx, http.MethodPost, createApplication, "", body, &out); err != nil {
		return Result{}, fmt.Errorf("register the cluster's OIDC client: %w", err)
	}
	if out.OIDCConfiguration.ClientID == "" {
		// The client id is the only thing this operation produces. Returning
		// success without one would record an empty value in the topology, and
		// the API server would come up trusting an issuer for a client that does
		// not exist -- which fails every login rather than failing here.
		return Result{}, fmt.Errorf("the identity provider created application %q and returned no client id",
			out.ApplicationID)
	}
	return Result{ClientID: out.OIDCConfiguration.ClientID, ApplicationID: out.ApplicationID}, nil
}

// revoke removes the borrowed token. The service account is deliberately left in
// place: it is inert without a credential, and deleting it would also remove the
// record of what registered the application.
func (b *Bootstrap) revoke(ctx context.Context) error {
	path := fmt.Sprintf("/v2/users/%s/pats/%s", b.UserID, b.TokenID)
	return b.client().do(ctx, http.MethodDelete, path, "", nil, nil)
}
