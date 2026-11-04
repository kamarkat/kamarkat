package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fleet ApplicationSet must DERIVE the issuer, never read the raw value.
//
// oidcIssuer reached the chart as a seed parameter, and the seed Application is
// declared in the tenant's own repository (ADR-072). That file carries
// bundleVersion and nothing else, so the parameter was absent and the chart
// received "". universal-tenant then refused to render for every fleet:
//
//	execution error at (universal-tenant/templates/agentgateway.yaml:13:15):
//	oidcIssuer must be supplied by the environment
//
// What made it expensive to find is that the error appeared on the FLEET's
// Application, naming a template in a platform chart, for a value no tenant was
// asked for -- and its consequence appeared somewhere else again: the namespace
// that Application creates was never created, so the tenant's WORKLOAD
// Application failed on "namespaces tenant-<fleet> not found", which reads as a
// missing namespace rather than a missing issuer.
//
// ADR-051 derives every public hostname from the box's base domain, and Day-0
// derives this same issuer the same way, so the chart can too. Reading
// .Values.oidcIssuer directly reintroduces the dependency on a parameter that
// is not there.
func TestFleetAppsetDerivesTheOidcIssuerRatherThanReadingIt(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "manifests", "argocd", "environment-manager", "templates")

	appset, err := os.ReadFile(filepath.Join(dir, "05-tenant-fleet-appset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(appset)

	for _, raw := range []string{".Values.oidcIssuer", ".Values.oidcJwksUrl"} {
		if strings.Contains(s, raw) {
			t.Errorf("05-tenant-fleet-appset.yaml reads %s directly: it is empty on every box "+
				"whose seed is declared in its own repository. Use the derivation helper.", raw)
		}
	}
	for _, want := range []string{
		`include "environment-manager.oidcIssuer"`,
		`include "environment-manager.oidcJwksUrl"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("05-tenant-fleet-appset.yaml does not call %s", want)
		}
	}

	helper, err := os.ReadFile(filepath.Join(dir, "_oidc-issuer.tpl"))
	if err != nil {
		t.Fatalf("the derivation helper is missing: %v", err)
	}
	h := string(helper)

	// Zitadel serves its keys at /oauth/v2/keys and 404s the conventional path.
	// Verified against this box's own discovery document, which advertises
	// jwks_uri as https://id.<domain>/oauth/v2/keys.
	if !strings.Contains(h, "/oauth/v2/keys") {
		t.Error("the JWKS derivation must use /oauth/v2/keys: the conventional " +
			"/.well-known/jwks.json 404s on Zitadel, and the resulting failure names " +
			"neither the path nor the provider")
	}
	// An explicit value must still win, so a box federating to an IdP that is
	// not its own can say so.
	if !strings.Contains(h, "if .Values.oidcIssuer") {
		t.Error("an explicitly set oidcIssuer must take precedence over the derivation")
	}
}
