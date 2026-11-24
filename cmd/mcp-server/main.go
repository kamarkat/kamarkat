// Command mcp-server is kamarkat-mcp: the box's platform state over MCP, read as
// the person who asks (ADR-116).
//
// Configuration is the environment the capability renders:
//
//	KAMARKAT_MCP_PUBLIC_URL        https://mcp.<hub-domain>, as clients reach it
//	KAMARKAT_MCP_ISSUER            the box's identity provider, as tokens name it
//	KAMARKAT_MCP_PROJECT_ID        the kamarkat-mcp project: every token's audience
//	KAMARKAT_MCP_KAKT_CLIENT_ID    the declared kakt client (published in metadata)
//	KAMARKAT_MCP_KAKT_CLI_CLIENT_ID the declared kakt-cli client
//	KAMARKAT_MCP_AGENT_DECLARED    non-empty while the box declares a kakt-agent key
//	KAMARKAT_MCP_AGENT_USER_ID     the kakt-agent machine user
//	KAMARKAT_MCP_AGENT_KEY_ID      the id its public key was registered as
//	KAMARKAT_MCP_DISABLE           comma-separated categories to leave out
//	KAMARKAT_MCP_ENABLE_VERIFY     "true" to serve the verify category
//	KAMARKAT_MCP_GRAFANA_URL       mcp-grafana's /mcp, when capabilities.grafanaMcp is on
//	KAMARKAT_MCP_GRAFANA_TOKEN_SECRET namespace/name of mcp-grafana's caller token
//	PORT                           listen port (default 8080)
//
// The identity values come from a Secret the hub-operator publishes once the
// project and clients exist. Until they do, the server answers its health
// check and refuses every MCP request, saying why.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/platform/boxmcp"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("kamarkat-mcp stopped", "error", err.Error())
		os.Exit(1)
	}
}

func env(k string) string { return strings.TrimSpace(os.Getenv(k)) }

func run(log *slog.Logger) error {
	base, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster configuration: %w", err)
	}
	own, err := kubernetes.NewForConfig(base)
	if err != nil {
		return err
	}

	publicURL, issuer, projectID := env("KAMARKAT_MCP_PUBLIC_URL"), env("KAMARKAT_MCP_ISSUER"), env("KAMARKAT_MCP_PROJECT_ID")
	clients := map[string]string{"kakt": env("KAMARKAT_MCP_KAKT_CLIENT_ID"), "kakt-cli": env("KAMARKAT_MCP_KAKT_CLI_CLIENT_ID")}
	var allowed []string
	for _, id := range clients {
		if id != "" {
			allowed = append(allowed, id)
		}
	}

	// The agent is accepted only while the box declares its key: removing the
	// declaration restarts this pod without it, whatever tokens are outstanding.
	var agent *boxmcp.AgentMetadata
	agentUser := ""
	if env("KAMARKAT_MCP_AGENT_DECLARED") != "" && env("KAMARKAT_MCP_AGENT_USER_ID") != "" {
		agentUser = env("KAMARKAT_MCP_AGENT_USER_ID")
		agent = &boxmcp.AgentMetadata{UserID: agentUser, KeyID: env("KAMARKAT_MCP_AGENT_KEY_ID")}
	}

	var disabled []string
	for _, c := range strings.Split(env("KAMARKAT_MCP_DISABLE"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			disabled = append(disabled, c)
		}
	}
	capabilities := []string{"platform"}

	var grafana http.Handler
	if u := env("KAMARKAT_MCP_GRAFANA_URL"); u != "" {
		g, gerr := boxmcp.NewGrafanaProxy(u, own, env("KAMARKAT_MCP_GRAFANA_TOKEN_SECRET"))
		if gerr != nil {
			return gerr
		}
		grafana = g
		capabilities = append(capabilities, "grafana")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	auth, authErr := boxmcp.NewAuthenticator(boxmcp.AuthConfig{Issuer: issuer, ProjectID: projectID, AllowedClients: allowed, AgentUserID: agentUser})
	if authErr != nil {
		// Not provisioned yet: say so on every request rather than crash-looping,
		// which would read as a broken component instead of a pending identity.
		log.Warn("identity not provisioned; refusing MCP requests until it is", "reason", authErr.Error())
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "this box's MCP identity is not provisioned yet: "+authErr.Error(), http.StatusServiceUnavailable)
		})
	} else {
		mux.Handle("/", boxmcp.NewHTTPHandler(boxmcp.HTTPConfig{
			Auth: auth,
			Metadata: boxmcp.MetadataConfig{
				PublicURL: publicURL, Issuer: issuer, ProjectID: projectID,
				Clients: clients, Capabilities: capabilities, Agent: agent,
			},
			Options: boxmcp.Options{Disabled: disabled, EnableVerify: env("KAMARKAT_MCP_ENABLE_VERIFY") == "true"},
			ClientsFor: func(id *boxmcp.Identity) (*boxmcp.Clients, error) {
				return impersonating(base, id)
			},
			Grafana: grafana,
			Log:     log,
		}))
	}

	port := env("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("kamarkat-mcp listening", "port", port, "version", boxmcp.Version, "capabilities", capabilities, "disabled", disabled)
	return srv.ListenAndServe()
}

// impersonating builds clients that act as the person: their email, which is
// what the API server names an OIDC user by, and one group per project role.
// This server's own ServiceAccount may impersonate and nothing else, so what
// the person may read is decided by their RBAC, not by this process.
func impersonating(base *rest.Config, id *boxmcp.Identity) (*boxmcp.Clients, error) {
	cfg := rest.CopyConfig(base)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: id.Email, Groups: id.Groups()}
	cfg.UserAgent = "kamarkat-mcp/" + boxmcp.Version
	return clientsFor(cfg)
}

func clientsFor(cfg *rest.Config) (*boxmcp.Clients, error) {
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &boxmcp.Clients{Core: core, Dynamic: dyn, Discovery: disc}, nil
}
