package boxmcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPConfig is the box's MCP front door (ADR-116 §1).
type HTTPConfig struct {
	Auth     *Authenticator
	Metadata MetadataConfig
	// Options are the server's categories; Clients is set per request.
	Options Options
	// ClientsFor builds the clients for one person: impersonating them.
	ClientsFor func(*Identity) (*Clients, error)
	// Grafana relays /grafana, or is nil when the capability is off.
	Grafana http.Handler
	Log     *slog.Logger
}

// NewHTTPHandler serves /platform (MCP), /grafana (relayed), the resource
// metadata and /healthz.
//
// Every MCP request is authenticated on its own and served by a server built
// for that person, statelessly: no session outlives the request, so revoking a
// person's role in the box takes effect on their next token, not at the end of
// a session.
func NewHTTPHandler(cfg HTTPConfig) http.Handler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.Handle("/.well-known/oauth-protected-resource", MetadataHandler(cfg.Metadata))
	mux.Handle("/.well-known/oauth-protected-resource/", MetadataHandler(cfg.Metadata))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	platform := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		id := IdentityFrom(r.Context())
		opts := cfg.Options
		opts.EnableVerify = opts.EnableVerify && id.Has(RoleVerifier)
		opts.Clients = func(context.Context) (*Clients, error) { return cfg.ClientsFor(id) }
		return NewServer(opts)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: cfg.Log})

	mux.Handle("/platform", cfg.guard("/platform", RoleViewer, platform))
	if cfg.Grafana != nil {
		mux.Handle("/grafana", cfg.guard("/grafana", RoleObserver, cfg.Grafana))
	} else {
		mux.HandleFunc("/grafana", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Grafana is not enabled on this box (capabilities.grafanaMcp)", http.StatusNotFound)
		})
	}
	return mux
}

// guard authenticates the request and requires role, answering a missing or
// unacceptable token with the RFC 9728 challenge and a missing role with 403.
func (cfg HTTPConfig) guard(endpoint, role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := cfg.Auth.Authenticate(r)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				cfg.Log.Info("mcp request refused", "endpoint", endpoint, "reason", err.Error())
				Challenge(w, cfg.Metadata.PublicURL, endpoint, "a token from this box's identity provider is required")
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if !id.Has(role) {
			cfg.Log.Info("mcp request forbidden", "endpoint", endpoint, "subject", id.Subject, "missing", role)
			http.Error(w, "your access in this box does not include "+role+"; an administrator grants it in the box's identity provider", http.StatusForbidden)
			return
		}
		cfg.Log.Info("mcp request", "endpoint", endpoint, "subject", id.Subject, "client", id.ClientID,
			"session", r.Header.Get("X-Kakt-Session"))
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}
