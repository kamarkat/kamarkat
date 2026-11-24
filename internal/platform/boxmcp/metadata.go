package boxmcp

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ResourceMetadata is OAuth 2.0 Protected Resource Metadata (RFC 9728) for one
// endpoint, plus a `kamarkat` member naming what a client needs to sign in:
// the project whose audience to request, and the clients the box declared.
// Ids and names, never credentials (ADR-116 §3).
type ResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ScopesSupported        []string `json:"scopes_supported"`
	ResourceName           string   `json:"resource_name"`
	Kamarkat               struct {
		ProjectID    string            `json:"project_id"`
		Clients      map[string]string `json:"clients"`
		Capabilities []string          `json:"capabilities"`
		// Agent is the kakt-agent machine user and the id of the public key the
		// box registered for it, when the box declares one: what kakt names as
		// issuer and key id in the assertion it signs.
		Agent *AgentMetadata `json:"agent,omitempty"`
	} `json:"kamarkat"`
}

// MetadataConfig is what the metadata says.
type MetadataConfig struct {
	PublicURL    string
	Issuer       string
	ProjectID    string
	Clients      map[string]string // name -> client id; an empty id is left out
	Capabilities []string          // "platform", "grafana"
	Agent        *AgentMetadata
}

// AgentMetadata identifies the kakt-agent machine user (ADR-116 §3).
type AgentMetadata struct {
	UserID string `json:"user_id"`
	KeyID  string `json:"key_id"`
}

// Scopes a client requests for this resource: the project audience and its roles.
func Scopes(projectID string) []string {
	return []string{"openid", "profile", "email", "offline_access",
		"urn:zitadel:iam:org:project:id:" + projectID + ":aud", "urn:zitadel:iam:org:projects:roles"}
}

// MetadataHandler serves /.well-known/oauth-protected-resource[/<endpoint>].
func MetadataHandler(cfg MetadataConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := strings.TrimPrefix(r.URL.Path, "/.well-known/oauth-protected-resource")
		if endpoint == "" {
			endpoint = "/platform"
		}
		var m ResourceMetadata
		m.Resource = strings.TrimSuffix(cfg.PublicURL, "/") + endpoint
		m.AuthorizationServers = []string{strings.TrimSuffix(cfg.Issuer, "/")}
		m.BearerMethodsSupported = []string{"header"}
		m.ScopesSupported = Scopes(cfg.ProjectID)
		m.ResourceName = "kamarkat box " + strings.TrimPrefix(endpoint, "/")
		m.Kamarkat.ProjectID = cfg.ProjectID
		m.Kamarkat.Clients = map[string]string{}
		for name, id := range cfg.Clients {
			if id != "" {
				m.Kamarkat.Clients[name] = id
			}
		}
		m.Kamarkat.Capabilities = cfg.Capabilities
		m.Kamarkat.Agent = cfg.Agent
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=60")
		_ = json.NewEncoder(w).Encode(m)
	})
}

// Challenge writes a 401 that points the client at the metadata (RFC 9728 §5.1).
func Challenge(w http.ResponseWriter, publicURL, endpoint, reason string) {
	w.Header().Set("WWW-Authenticate",
		`Bearer resource_metadata="`+strings.TrimSuffix(publicURL, "/")+"/.well-known/oauth-protected-resource"+endpoint+`"`)
	http.Error(w, reason, http.StatusUnauthorized)
}
