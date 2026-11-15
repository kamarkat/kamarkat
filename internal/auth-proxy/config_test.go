package authproxy

import (
	"strings"
	"testing"
)

// required mirrors every variable LoadConfig demands. Kept explicit so that adding a
// setting without a manifest entry fails a test rather than a production pod.
var required = map[string]string{
	"LISTEN_ADDR":                    ":8080",
	"IDENTITY_PROVIDER_ISSUER_URL":   "https://id.dev.nutgraf.in",
	"IDENTITY_PROVIDER_INTERNAL_URL": "http://zitadel.platform-identity.svc.cluster.local:8080",
	"JWKS_CACHE_TTL":                 "1h",
	"JWKS_FETCH_TIMEOUT":             "5s",
	"JWKS_REFRESH_MIN_INTERVAL":      "10s",
	"EXPECTED_JWT_AUDIENCE":          "https://api.dev.nutgraf.in",
	"AUTH_PUBLIC_BASE_URL":           "https://auth.dev.nutgraf.in",
	"MCP_GATEWAY_BASE_URL":           "https://api.dev.nutgraf.in",
}

func setAll(t *testing.T) {
	t.Helper()
	for k, v := range required {
		t.Setenv(k, v)
	}
}

func TestLoadConfig_FailsHardWhenNothingSet(t *testing.T) {
	for k := range required {
		t.Setenv(k, "")
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected failure with no environment set, got nil — a fallback has been reintroduced")
	}
}

func TestLoadConfig_ReportsEveryMissingVarAtOnce(t *testing.T) {
	for k := range required {
		t.Setenv(k, "")
	}
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error")
	}
	// One restart should reveal the whole problem, not just the first offender.
	for k := range required {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not mention missing %s: %v", k, err)
		}
	}
}

func TestLoadConfig_EachVarIndividuallyRequired(t *testing.T) {
	for k := range required {
		t.Run(k, func(t *testing.T) {
			setAll(t)
			t.Setenv(k, "")
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("%s is not enforced — LoadConfig succeeded without it", k)
			}
		})
	}
}

func TestLoadConfig_RejectsMalformedDuration(t *testing.T) {
	setAll(t)
	t.Setenv("JWKS_CACHE_TTL", "not-a-duration")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected an invalid duration to be rejected")
	}
}

func TestLoadConfig_SucceedsWhenFullyConfigured(t *testing.T) {
	setAll(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthPublicBaseURL != "https://auth.dev.nutgraf.in" {
		t.Errorf("AuthPublicBaseURL = %q", cfg.AuthPublicBaseURL)
	}
	if cfg.JWKSCacheTTL.String() != "1h0m0s" {
		t.Errorf("JWKSCacheTTL = %v", cfg.JWKSCacheTTL)
	}
}
