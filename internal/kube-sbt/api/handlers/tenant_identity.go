package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/soloz-io/zero-ops/internal/kube-sbt/interfaces"
	"github.com/soloz-io/zero-ops/internal/kube-sbt/models"
)

// TenantIdentityHandler provisions a tenant's identity resources at the issuer.
//
// ADR-041 assigns tenant identity lifecycle to this service and tenant
// environment provisioning to the Hub Operator, so the operator ORCHESTRATES
// and calls this; it does not create identity resources itself. That split is
// why this is an endpoint rather than a loop reading tenant records — the tenant
// registry belongs to the operator, and reading it from here would be this
// service reaching into another component's domain.
type TenantIdentityHandler struct {
	provisioner interfaces.ITenantIdentityProvisioner
	publish     func(tenantID, clientID string) error
}

func NewTenantIdentityHandler(p interfaces.ITenantIdentityProvisioner, publish func(tenantID, clientID string) error) *TenantIdentityHandler {
	return &TenantIdentityHandler{provisioner: p, publish: publish}
}

type ensureTenantIdentityRequest struct {
	// RedirectURIs are registered on the tenant's OAuth application. The issuer
	// matches them exactly, with no wildcards, so an environment's hostname must
	// appear here or its login fails at the authorization endpoint before any
	// credential is entered — which reads as a broken login page rather than a
	// missing registration.
	RedirectURIs   []string `json:"redirectUris"`
	PostLogoutURIs []string `json:"postLogoutUris"`
	// KnownOrgID is the IMMUTABLE binding from the tenant's record (ADR-088).
	// Empty means unbound: the service may then create an organisation if none
	// exists, and must REFUSE if one does, rather than adopting it by name.
	KnownOrgID string `json:"knownOrgId"`
	// ProjectName and KnownProjectID select the APPLICATION's project (ADR-094).
	// KnownProjectID is the immutable binding and wins when set; otherwise the
	// project is found or created by name inside the bound organisation. Both
	// empty means the configured legacy project, which is how an application
	// provisioned before per-application projects stays where it is.
	ProjectName    string `json:"projectName"`
	KnownProjectID string `json:"knownProjectId"`
	// OwnerEmail is granted administrative access, so the tenant has someone who
	// can sign in to it at all.
	OwnerEmail string `json:"ownerEmail"`
	// SelfRegistration lets anyone reaching this tenant's hostname create an
	// account in it. Scoped to this tenant's organisation by the gateway, so it
	// cannot create accounts anywhere else.
	SelfRegistration bool `json:"selfRegistration"`

	// ServiceClientName is the login name for this application's SERVICE
	// identity, which it calls other applications as (ADR-097).
	//
	// Named by the CALLER rather than derived here. The name has to agree with
	// the key the caller publishes the credential under, and a name invented on
	// each side is two names for one identity -- the defect the OIDC_ALLOWED_AZP
	// grammar already cost us once. Empty means the application declares no
	// cross-application calling and gets no service identity: an unused machine
	// credential is a standing credential nobody rotates.
	ServiceClientName string `json:"serviceClientName"`
	// OAuthClients are the clients this FLEET declares. Exactly this set is
	// provisioned and no more: a fleet that declares none gets none, rather than
	// a set the platform chose for it (ADR-047).
	OAuthClients []struct {
		Name          string   `json:"name"`
		Confidential  bool     `json:"confidential"`
		TokenExchange bool     `json:"tokenExchange"`
		RedirectURIs  []string `json:"redirectUris"`
	} `json:"oauthClients"`
}

// EnsureIdentity is idempotent: it reports the tenant's identity whether it
// created it or found it. The caller reconciles, so it will be called
// repeatedly, and an "already exists" answer must be success rather than a
// conflict the caller has to interpret.
func (h *TenantIdentityHandler) EnsureIdentity(c *gin.Context) {
	tenantID := c.Param("tenantId")
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenantId is required"})
		return
	}

	if h.provisioner == nil {
		// Not an error the caller can fix by retrying: the configured provider
		// has no notion of a tenant to provision. Said plainly so a reconcile
		// loop stops rather than spins.
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "the configured identity provider does not provision tenant identities",
		})
		return
	}

	var req ensureTenantIdentityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	clients := make([]models.OAuthClient, 0, len(req.OAuthClients))
	for _, oc := range req.OAuthClients {
		clients = append(clients, models.OAuthClient{
			Name:          oc.Name,
			Confidential:  oc.Confidential,
			RedirectURIs:  oc.RedirectURIs,
			TokenExchange: oc.TokenExchange,
		})
	}

	identity, err := h.provisioner.EnsureTenantIdentity(c.Request.Context(), tenantID, req.KnownOrgID,
		models.ProjectBinding{Name: req.ProjectName, KnownID: req.KnownProjectID}, req.OwnerEmail, req.SelfRegistration, req.RedirectURIs, req.PostLogoutURIs, clients)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	// The clients the fleet DECLARED, provisioned after the tenant's own
	// identity exists.
	//
	// This loop is the whole reason a fleet may declare a confidential client.
	// It was written on ControlPlane.EnsureTenantIdentity, which nothing
	// reaches: this service constructs no ControlPlane, and the route is wired
	// straight to the provider. So a fleet declaring a client got a 200, no
	// credential, and no error -- its ExternalSecret then sat in
	// SecretSyncedError naming keys nothing had ever written.
	//
	// Credentials are RETURNED, not stored. This service provisions at the
	// issuer; the caller owns where secrets live (ADR-003), which is the same
	// division OwnerPassword and OIDC_CLIENT_ID already follow.
	for _, decl := range clients {
		if !decl.Confidential || decl.Name == "" {
			continue
		}
		appName := tenantID + "-" + decl.Name
		clientID, clientSecret, cerr := h.provisioner.EnsureConfidentialClient(
			c.Request.Context(), identity.TenantRef, identity.ProjectRef, appName, decl.RedirectURIs, false, decl.TokenExchange)
		if cerr != nil {
			// Reported, not fatal. The tenant identity itself succeeded and is
			// worth returning; a client that failed is named in Incomplete so
			// the caller learns which capability is missing rather than
			// discovering it as a workload that will not start.
			identity.Incomplete = append(identity.Incomplete,
				fmt.Sprintf("provision declared client %q: %v", decl.Name, cerr))
			continue
		}
		identity.Clients = append(identity.Clients, models.DeclaredClient{
			Name:         decl.Name,
			ClientID:     clientID,
			ClientSecret: clientSecret,
		})
	}

	// The SERVICE identity, for calling other applications (ADR-097).
	//
	// Provisioned after the tenant's identity for the same reason the declared
	// clients are: it belongs to an organisation this call has just resolved, and
	// creating it against an organisation resolved some other way is how a
	// credential ends up somewhere the tenant cannot see and nothing revokes with
	// the tenant.
	//
	// Failure is reported in Incomplete, not fatal. The browser path does not
	// depend on this; an application whose service identity is missing loses
	// cross-application calling and keeps its login, and the caller learns which
	// capability is absent instead of finding a workload that will not start.
	if req.ServiceClientName != "" {
		svc, serr := h.provisioner.EnsureMachineClient(
			c.Request.Context(), identity.TenantRef, req.ServiceClientName, req.ServiceClientName, false)
		switch {
		case serr != nil:
			identity.Incomplete = append(identity.Incomplete,
				fmt.Sprintf("provision service identity %q: %v", req.ServiceClientName, serr))
		default:
			identity.ServiceClient = svc
		}
	}

	// Published before the response. A caller that saw success and then found no
	// client id could not tell "not provisioned" from "provisioned, publish
	// failed", and only the second needs a retry.
	if h.publish != nil {
		if err := h.publish(tenantID, identity.ClientID); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "identity provisioned but not published: " + err.Error()})
			return
		}
	}

	resp := gin.H{
		"tenantRef":  identity.TenantRef,
		"projectRef": identity.ProjectRef,
		"clientId":   identity.ClientID,
	}
	// What did not finish, when anything did not. Carried in a SUCCESSFUL
	// response on purpose: the identifiers above are real and must be published,
	// and the tenant is nonetheless not yet usable. Sending a failure status
	// instead would suppress the identifiers to report a problem with something
	// else.
	if len(identity.Incomplete) > 0 {
		resp["incomplete"] = identity.Incomplete
	}
	// Present ONLY on the call that created the owner's account. Its absence on
	// later calls is the signal that no new credential exists, so a caller that
	// persists it unconditionally would overwrite a stored password with an
	// empty value.
	// The service identity's client id, and its secret ONLY when this call minted
	// one. The id is what every TARGET allowlists, so it must reach the caller
	// even on a reconcile that disclosed no secret -- the receiver's allowlist is
	// rebuilt from it, and a reconcile that omitted it would look like a caller
	// that no longer exists.
	if identity.ServiceClient != nil && identity.ServiceClient.ClientID != "" {
		svc := gin.H{"clientId": identity.ServiceClient.ClientID}
		if identity.ServiceClient.ClientSecret != "" {
			svc["clientSecret"] = identity.ServiceClient.ClientSecret
		}
		resp["serviceClient"] = svc
	}
	if identity.OwnerPassword != "" {
		resp["ownerPassword"] = identity.OwnerPassword
	}
	if len(identity.Clients) > 0 {
		out := make([]map[string]string, 0, len(identity.Clients))
		for _, dc := range identity.Clients {
			out = append(out, map[string]string{
				"name":         dc.Name,
				"clientId":     dc.ClientID,
				"clientSecret": dc.ClientSecret,
			})
		}
		resp["clients"] = out
	}
	c.JSON(http.StatusOK, resp)
}

// PlatformAppProvisioner is implemented by providers that can create an
// application in the PLATFORM's own organisation, as distinct from a tenant's.
//
// Optional and asserted at the call site, for the same reason the tenant
// capability is: a provider with no notion of an organisation has no such
// distinction to make and would have to invent an answer.
type PlatformAppProvisioner interface {
	EnsurePlatformApp(ctx context.Context, appName string, redirectURIs, postLogoutURIs []string) (string, error)
	EnsurePlatformProject(ctx context.Context, name string) (orgID, projectID string, err error)
	// EnsurePlatformProjectAccess admits the platform's own people to a
	// platform project: the named role exists on it, and every human member of
	// the platform organisation holding no grant there is given that role.
	//
	// Asked for explicitly, per project, because every project this platform
	// creates refuses a token to a user holding no role in it. That is the
	// point of a project people SIGN IN to -- the observability one, whose
	// audience is what a spoke's VMAuth matches (ADR-078 add.5 §7) -- and wrong
	// for one that exists only as an audience for a single machine caller, like
	// the credit authority's (ADR-109).
	EnsurePlatformProjectAccess(ctx context.Context, orgID, projectID, roleKey, roleDisplay string) error
	// EnsureConfidentialClient provisions a client that authenticates with a
	// SECRET, in a project the caller has already resolved.
	//
	// Grafana is the first platform capability that needs one: it runs a browser
	// flow from a server it controls, so a public client buys nothing a secret
	// does not, and its OIDC library expects a secret. The project is passed in
	// rather than taken from configuration because a capability's project is its
	// own -- that is what makes projectRoleCheck mean "an observability viewer"
	// instead of "any platform user" (ADR-078 add.5 §7).
	EnsureConfidentialClient(ctx context.Context, orgID, projectID, appName string, redirectURIs []string, regenerateIfExists, tokenExchange bool) (clientID, clientSecret string, err error)
}

type ensurePlatformProjectRequest struct {
	Name string `json:"name" binding:"required"`
}

// EnsureProject provisions a project in the platform's own organisation: the
// audience a platform capability admits its callers by (ADR-109).
func (h *PlatformAppHandler) EnsureProject(c *gin.Context) {
	if h.provisioner == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "the configured identity provider does not provision platform projects",
		})
		return
	}
	var req ensurePlatformProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	orgID, projectID, err := h.provisioner.EnsurePlatformProject(c.Request.Context(), req.Name)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"orgId": orgID, "projectId": projectID})
}

type ensurePlatformProjectAccessRequest struct {
	OrgID       string `json:"orgId" binding:"required"`
	ProjectID   string `json:"projectId" binding:"required"`
	RoleKey     string `json:"roleKey" binding:"required"`
	RoleDisplay string `json:"roleDisplay"`
}

// EnsureProjectAccess admits the platform's own people to a platform project.
//
// Separate from provisioning the project because the two answer different
// questions: that one asks for an audience to exist, this one asks who may be
// issued a token for an application inside it. Grafana's viewer client is the
// first case where the answer is "the box's operators" rather than "one machine
// caller", and without it the issuer refuses every sign-in with
// Errors.User.GrantRequired -- a refusal nothing between Grafana and the issuer
// explains.
func (h *PlatformAppHandler) EnsureProjectAccess(c *gin.Context) {
	if h.provisioner == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "the configured identity provider does not provision platform projects",
		})
		return
	}
	var req ensurePlatformProjectAccessRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.provisioner.EnsurePlatformProjectAccess(
		c.Request.Context(), req.OrgID, req.ProjectID, req.RoleKey, req.RoleDisplay); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projectId": req.ProjectID, "roleKey": req.RoleKey})
}

// PlatformAppHandler provisions a platform-level OIDC client.
//
// The Kubernetes API server is the motivating consumer: its client id is an
// API-server FLAG, so it cannot be read at runtime the way a tenant's gateway
// reads one, and it has to exist before the flag can name it.
type PlatformAppHandler struct{ provisioner PlatformAppProvisioner }

func NewPlatformAppHandler(p PlatformAppProvisioner) *PlatformAppHandler {
	return &PlatformAppHandler{provisioner: p}
}

type ensurePlatformAppRequest struct {
	Name           string   `json:"name"`
	RedirectURIs   []string `json:"redirectUris"`
	PostLogoutURIs []string `json:"postLogoutUris"`
}

type ensureConfidentialClientRequest struct {
	OrgID        string   `json:"orgId" binding:"required"`
	ProjectID    string   `json:"projectId" binding:"required"`
	Name         string   `json:"name" binding:"required"`
	RedirectURIs []string `json:"redirectUris"`
}

// EnsureConfidentialClient provisions a platform capability's confidential OIDC
// client and returns its credentials.
//
// The secret is returned ONLY when the issuer just minted it -- it discloses a
// secret once -- so an existing client comes back with an empty secret and the
// caller keeps the one it already published. Regenerating on every reconcile
// would invalidate the credential the running workload holds, and the failure
// would land on the next sign-in rather than on the reconcile that caused it.
func (h *PlatformAppHandler) EnsureConfidentialClient(c *gin.Context) {
	if h.provisioner == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "the configured identity provider does not provision platform clients",
		})
		return
	}
	var req ensureConfidentialClientRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	clientID, secret, err := h.provisioner.EnsureConfidentialClient(
		c.Request.Context(), req.OrgID, req.ProjectID, req.Name, req.RedirectURIs, false, false)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": req.Name, "clientId": clientID, "clientSecret": secret})
}

func (h *PlatformAppHandler) EnsureApp(c *gin.Context) {
	if h.provisioner == nil {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": "the configured identity provider does not provision platform applications",
		})
		return
	}
	var req ensurePlatformAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	clientID, err := h.provisioner.EnsurePlatformApp(c.Request.Context(), req.Name, req.RedirectURIs, req.PostLogoutURIs)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": req.Name, "clientId": clientID})
}
