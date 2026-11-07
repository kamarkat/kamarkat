package bootstrap

import (
	"fmt"
	"github.com/kamarkat/kamarkat/internal/platform"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// WS4 — hub public domain authority (ADR-051).
//
// ADR-051 names the environment overlay the system of record for the base domain
// and calls the hostname literals scattered through the hub manifests debt to be
// retired against it. This is the derivation half of that retirement: one value in,
// the complete endpoint set out, no per-hostname knobs.
//
// The hub-CLI derives rather than hub-operator because the artifacts land in a Git
// working tree and the operator has no Git write path (WS4 §1.2). The CLI already
// writes and commits ADR-045 artifacts, so this is the component already doing the
// job, not a new capability.

// HubEndpoints is the complete set of public hostnames for one hub environment.
//
// Every field derives from Zone. Exposing the set as a struct rather than a map is
// deliberate: a caller cannot ask for a hostname that the derivation does not
// define, so a new endpoint has to be added here and reviewed once rather than
// invented at each call site — which is how the literals accumulated.
type HubEndpoints struct {
	// Zone is the base domain: dev.nutgraf.in, stg.nutgraf.in, or the apex for
	// production. ADR-051's env-as-zone rule makes non-production zones carry the
	// environment as their leftmost label; production uses the apex unlabelled, so
	// the derivation below is identical for all of them.
	Zone string

	API       string // AgentGateway / MCP surface
	Auth      string // Hydra issuer, login and consent
	ID        string // Zitadel issuer, hosted login and console
	Console   string // Kratos self-service UI
	ArgoCD    string // ArgoCD server
	Infisical string // Infisical public endpoint
	MCP       string // OAuth redirect target (C7)
	Dashboard string // Headlamp

	// ZitadelOrg is the domain Zitadel generates for its default organisation,
	// and it is derived rather than skipped because the admin machine user's
	// address is built from it: changing the zone changes that address, and a
	// literal would let the two drift into an account nobody can authenticate as.
	ZitadelOrg string
}

// DeriveHubEndpoints produces the endpoint set for a base domain.
//
// It is total and side-effect free so the migration can be diffed before anything
// is written: WS4-1 emits from this and compares against the literals currently in
// the manifests, and a difference is a bug or an undocumented exception rather than
// a surprise discovered at sync time.
func DeriveHubEndpoints(zone string) HubEndpoints {
	zone = strings.TrimSpace(strings.TrimSuffix(zone, "."))
	host := func(label string) string {
		if label == "" {
			return zone
		}
		return label + "." + zone
	}
	return HubEndpoints{
		Zone:       zone,
		API:        host("api"),
		Auth:       host("auth"),
		ID:         host("id"),
		Console:    host("console"),
		ArgoCD:     host("argocd"),
		Infisical:  host("infisical"),
		MCP:        host("mcp"),
		Dashboard:  host("dashboard"),
		ZitadelOrg: host("zitadel.id"),
	}
}

// hubEnvironmentOverlay is the subset of the authority object this derivation reads.
type hubEnvironmentOverlay struct {
	Spec struct {
		Domain      string `json:"domain"`
		Environment string `json:"environment"`
	} `json:"spec"`
}

// ReadHubZone returns the base domain the hub declares.
//
// The overlay is the system of record (ADR-051), so the zone is read from Git
// rather than from a running cluster: the derivation must produce the same result
// before the cluster exists as after, or bootstrap and steady state disagree.
//
// ONE OVERLAY, not one per environment (ADR-111 Revision 7). It took an
// environmentSlug and read manifests/environments/<slug>/, which is how a box got
// a zone per environment -- the arrangement ADR-051's Amendment 2026-09-18 already
// replaced with a declared subdomain. The hub has one zone because it is one
// cluster; the label in it is a subdomain, not a stage.
//
// An absent domain in the overlay falls back to the base object rather than being
// an error, because the base already carries the apex.
func ReadHubZone(projectRoot string) (string, error) {
	// Repository-relative, resolved by the platform package: a released build
	// reads what it carries, an unreleased one the working tree (ADR-063,
	// ADR-068). projectRoot is retained for callers that pass one, and ignored
	// by a released build, which has no tree to root against.
	rel := func(parts ...string) string {
		p := filepath.Join(parts...)
		if projectRoot != "" {
			return filepath.Join(projectRoot, p)
		}
		return p
	}
	candidates := []string{
		rel("manifests", "environments", "management", "patch-hubenvironment.yaml"),
		rel("manifests", "environments", "base", "hubenvironment.yaml"),
	}
	for _, path := range candidates {
		data, err := platform.ReadFile(path)
		if err != nil {
			if !platform.Exists(path) {
				continue
			}
			return "", fmt.Errorf("failed to read %s: %w", path, err)
		}
		var overlay hubEnvironmentOverlay
		if err := yaml.Unmarshal(data, &overlay); err != nil {
			return "", fmt.Errorf("failed to parse %s: %w", path, err)
		}
		if d := strings.TrimSpace(overlay.Spec.Domain); d != "" {
			return d, nil
		}
	}
	return "", fmt.Errorf("no spec.domain declared in any hub overlay; the overlay is "+
		"the system of record for the base domain (ADR-051).\n"+
		"  Looked in: %s\n"+
		"  Source: %s",
		strings.Join(candidates, ", "), platform.Origin())
}
