package bootstrap

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// WS4-1 — derivation dry-run.
//
// The migration's stated expectation is that deriving from spec.domain reproduces
// the hostname literals currently in the manifests byte-for-byte on dev, and that
// any difference is either a bug or an undocumented exception. That check is worth
// more as a test than as a one-off run: it fails the same way during the migration
// if a derived host stops matching what the manifests actually serve.

func TestDeriveHubEndpointsFromZone(t *testing.T) {
	cases := []struct{ zone, api, auth string }{
		{"dev.nutgraf.in", "api.dev.nutgraf.in", "auth.dev.nutgraf.in"},
		{"stg.nutgraf.in", "api.stg.nutgraf.in", "auth.stg.nutgraf.in"},
		// Production uses the apex unlabelled (ADR-051 env-as-zone), so the same
		// derivation applies with no special case.
		{"nutgraf.in", "api.nutgraf.in", "auth.nutgraf.in"},
	}
	for _, c := range cases {
		got := DeriveHubEndpoints(c.zone)
		if got.API != c.api || got.Auth != c.auth {
			t.Errorf("zone %q derived api=%q auth=%q; want %q %q",
				c.zone, got.API, got.Auth, c.api, c.auth)
		}
	}
}

// The overlay is the system of record, so the derivation must read its zone from
// there rather than from a constant that would become a fifty-first literal.
func TestReadHubZoneFromEnvironmentOverlay(t *testing.T) {
	root := repoRoot(t)
	for env, want := range map[string]string{"dev": "dev.nutgraf.in", "stg": "stg.nutgraf.in"} {
		got, err := ReadHubZone(root, env)
		if err != nil {
			t.Fatalf("ReadHubZone(%q): %v", env, err)
		}
		if got != want {
			t.Errorf("ReadHubZone(%q) = %q, want %q", env, got, want)
		}
	}
}

// The dry-run diff. Every hub hostname literal still in the manifests must be one
// the derivation produces; anything else means the migration would silently change
// a hostname when it replaces that literal.
func TestDerivationCoversEveryHubHostnameLiteral(t *testing.T) {
	root := repoRoot(t)

	// Each environment is checked against its OWN zone. Checking every overlay
	// against dev's would report stg's and prod's correct hostnames as defects.
	derived := map[string]bool{}
	for _, env := range []string{"dev", "stg", "prod"} {
		zone, err := ReadHubZone(root, env)
		if err != nil {
			t.Fatalf("ReadHubZone(%q): %v", env, err)
		}
		e := DeriveHubEndpoints(zone)
		for _, h := range []string{e.API, e.Auth, e.ID, e.Console, e.ArgoCD, e.Infisical,
			e.MCP, e.Dashboard, e.ZitadelOrg, e.Zone} {
			derived[h] = true
		}
	}

	// argocd-principal is fleet/mTLS on the Infisical PKI — a different trust class
	// that WS4 §C2 says explicitly not to fold into the public ACME contract. Spoke
	// and tenant trees belong to the spoke under ADR-051.
	skipPath := func(p string) bool {
		return strings.Contains(p, "/manifests/spoke/") ||
			strings.Contains(p, "/manifests/argocd-principal/") ||
			strings.Contains(p, "/manifests/tenants/")
	}
	skipHost := func(h string) bool {
		return strings.HasPrefix(h, "argocd-principal.") ||
			strings.HasPrefix(h, "argocd-principal-internal.") ||
			// acme.<zone> is the cert-manager webhook's Kubernetes API group, not
			// a host: it appears as groupName, as an APIService name, and as a
			// resource group. The prefix checks above catch it where it is written
			// as `group:` or `name:`, and miss it as `groupName:`, as a bare list
			// item and as an env `value:`. Deriving over any of those would rename
			// an API group and leave cert-manager unable to resolve its solver.
			strings.HasPrefix(h, "acme.")
	}

	host := regexp.MustCompile(`[a-z0-9][a-z0-9.-]*\.nutgraf\.in`)
	unknown := map[string][]string{}
	for _, dir := range []string{"manifests/hub-core-services", "manifests/environments", "manifests/providers"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") || skipPath(path) {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					continue // prose, not configuration
				}
				// Kubernetes API identifiers share the domain but are not hostnames:
				// an apiVersion group, a CRD's <plural>.<group> name, and a
				// domain-prefixed label key all match a hostname regex and none of
				// them is a host. Deriving over them would rename an API group.
				if strings.HasPrefix(trimmed, "apiVersion:") || strings.HasPrefix(trimmed, "name:") ||
					strings.HasPrefix(trimmed, "group:") {
					continue
				}
				for _, m := range host.FindAllStringIndex(line, -1) {
					h := line[m[0]:m[1]]
					if after := m[1]; after < len(line) && line[after] == '/' {
						continue // label key or apiVersion group, not a host
					}
					if derived[h] || skipHost(h) {
						continue
					}
					rel, _ := filepath.Rel(root, path)
					unknown[h] = append(unknown[h], rel)
				}
			}
			return nil
		})
	}
	if len(unknown) > 0 {
		keys := make([]string, 0, len(unknown))
		for k := range unknown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Errorf("hostname %q is present in the manifests but is not produced by the "+
				"derivation, so WS4 would change it when it replaces the literal: %v", k, unknown[k])
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate repository root")
	return ""
}
