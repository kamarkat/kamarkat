package authproxy

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string
	// ZitadelIssuerURL is the PUBLIC issuer, e.g. https://id.dev.nutgraf.in.
	//
	// It is what a token's `iss` claim equals and what a relying party compares
	// byte-for-byte, so it is NOT the in-cluster address and the two cannot be
	// collapsed. Environment-zoned (ADR-051).
	ZitadelIssuerURL string
	// ZitadelInternalURL is the in-cluster Service address documents are fetched
	// from, e.g. http://zitadel.platform-identity.svc.cluster.local:8080.
	//
	// Fetching over the public name would leave this service depending on its own
	// gateway, DNS and certificate to answer a readiness probe. Not
	// environment-zoned: it resolves only inside this cluster, and each
	// environment is a physically separate one (ADR-037).
	ZitadelInternalURL     string
	JWKSCacheTTL           time.Duration
	JWKSFetchTimeout       time.Duration
	JWKSRefreshMinInterval time.Duration
	ExpectedJWTAudience    string
	// AuthPublicBaseURL is the public hostname this service is served on
	// (auth.<zone>). Used only to describe itself; it is NOT advertised as the
	// issuer, because the tokens are Zitadel's and say so.
	AuthPublicBaseURL string
	// MCPGatewayBaseURL is the public base URL of the MCP gateway, used to derive
	// the resource audience in gateway-served metadata. Environment-zoned.
	MCPGatewayBaseURL string
}

// missingEnv accumulates every unset variable so a misconfigured deployment reports
// all of them at once rather than one restart at a time.
type missingEnv struct{ names []string }

func (m *missingEnv) get(key string) string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		m.names = append(m.names, key)
		return ""
	}
	return v
}

func (m *missingEnv) duration(key string) time.Duration {
	raw := m.get(key)
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		m.names = append(m.names, fmt.Sprintf("%s (invalid duration %q)", key, raw))
		return 0
	}
	return d
}

// LoadConfig reads every setting from the environment and fails hard if any is
// missing. There are deliberately NO defaults: a hardcoded fallback lets a
// misconfigured deployment start and silently talk to the wrong endpoint — which is
// exactly how AUTH_PUBLIC_BASE_URL and MCP_GATEWAY_BASE_URL came to be absent from
// the Deployment while the process still ran against compiled-in values.
func LoadConfig() (*Config, error) {
	m := &missingEnv{}

	cfg := &Config{
		ListenAddr:             m.get("LISTEN_ADDR"),
		ZitadelIssuerURL:       strings.TrimSuffix(m.get("IDENTITY_PROVIDER_ISSUER_URL"), "/"),
		ZitadelInternalURL:     strings.TrimSuffix(m.get("IDENTITY_PROVIDER_INTERNAL_URL"), "/"),
		JWKSCacheTTL:           m.duration("JWKS_CACHE_TTL"),
		JWKSFetchTimeout:       m.duration("JWKS_FETCH_TIMEOUT"),
		JWKSRefreshMinInterval: m.duration("JWKS_REFRESH_MIN_INTERVAL"),
		ExpectedJWTAudience:    m.get("EXPECTED_JWT_AUDIENCE"),
		AuthPublicBaseURL:      strings.TrimSuffix(m.get("AUTH_PUBLIC_BASE_URL"), "/"),
		MCPGatewayBaseURL:      strings.TrimSuffix(m.get("MCP_GATEWAY_BASE_URL"), "/"),
	}

	if len(m.names) > 0 {
		sort.Strings(m.names)
		return nil, fmt.Errorf(
			"auth-proxy configuration incomplete: %d required environment variable(s) missing or invalid: %s",
			len(m.names), strings.Join(m.names, ", "))
	}
	return cfg, nil
}
