package zitadel

import (
	"reflect"
	"sort"
	"testing"
)

const (
	orgHere  = "389091373187334477"
	orgOther = "389090776237277520"
)

// Claim shape copied from a real id_token captured 2026-09-03, with the
// identifying values replaced. The organisation name and email are inert here --
// nothing asserts them -- and ADR-047 forbids naming a tenant in platform code,
// so they carry placeholders. orgHere stays an opaque Zitadel org id because the
// assertions turn on it.
func tokenClaims() map[string]interface{} {
	return map[string]interface{}{
		"sub":                                   "389091433551757645",
		"email":                                 "user@example.com",
		"urn:zitadel:iam:user:resourceowner:id": orgHere,
		"urn:zitadel:iam:user:resourceowner:name": "example-org",
	}
}

func TestTenantComesFromTheOwningOrganisation(t *testing.T) {
	if got := tenantFromClaims(tokenClaims()); got != orgHere {
		t.Fatalf("tenant = %q, want %q", got, orgHere)
	}
}

func TestContractClaimWins(t *testing.T) {
	c := tokenClaims()
	c["tenant_id"] = "explicit"
	if got := tenantFromClaims(c); got != "explicit" {
		t.Fatalf("tenant = %q, want the contract's own claim to win", got)
	}
}

func TestAbsentTenantIsEmptyNotDefaulted(t *testing.T) {
	if got := tenantFromClaims(map[string]interface{}{"sub": "u"}); got != "" {
		t.Fatalf("tenant = %q, want empty so the caller rejects rather than defaults", got)
	}
}

func TestRolesGrantedHereAreKept(t *testing.T) {
	c := tokenClaims()
	c[scopedRolesClaim] = map[string]interface{}{
		"admin": map[string]interface{}{orgHere: "example-org.example.com"},
	}
	if got := rolesGrantedInTenant(c, orgHere); !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("roles = %v, want [admin]", got)
	}
}

// The reason the nested organisation id is not discarded. A role granted in
// another organisation must never read as a role here.
func TestRoleGrantedElsewhereIsIgnored(t *testing.T) {
	c := tokenClaims()
	c[scopedRolesClaim] = map[string]interface{}{
		"admin": map[string]interface{}{orgOther: "other.example.com"},
	}
	if got := rolesGrantedInTenant(c, orgHere); len(got) != 0 {
		t.Fatalf("roles = %v, want none — that grant belongs to another tenant", got)
	}
}

func TestCrossTenantGrantKeepsOnlyTheLocalHalf(t *testing.T) {
	c := tokenClaims()
	c[scopedRolesClaim] = map[string]interface{}{
		"admin": map[string]interface{}{orgHere: "example-org.example.com"},
		"owner": map[string]interface{}{orgOther: "other.example.com"},
	}
	got := rolesGrantedInTenant(c, orgHere)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("roles = %v, want [admin] only", got)
	}
}

func TestAbsentRolesMeansNoneNotAll(t *testing.T) {
	if got := rolesGrantedInTenant(tokenClaims(), orgHere); len(got) != 0 {
		t.Fatalf("roles = %v, want none", got)
	}
}

// Without a tenant there is nothing to scope roles to, so returning any would be
// returning unscoped authority.
func TestNoTenantYieldsNoRoles(t *testing.T) {
	c := tokenClaims()
	c[scopedRolesClaim] = map[string]interface{}{
		"admin": map[string]interface{}{orgHere: "example-org.example.com"},
	}
	if got := rolesGrantedInTenant(c, ""); len(got) != 0 {
		t.Fatalf("roles = %v, want none when the tenant is unknown", got)
	}
}

func TestSplitNameNeverYieldsAnEmptyGivenName(t *testing.T) {
	for _, in := range []string{"", "   ", "Arun", "Arun Subramanian", "A B C"} {
		g, f := splitName(in)
		if g == "" || f == "" {
			t.Fatalf("splitName(%q) = (%q,%q); the issuer rejects an empty name part", in, g, f)
		}
	}
}

// A credential delivered through a file or a Secret commonly carries a trailing
// newline, and Go refuses to build a request with one — reporting an invalid
// Authorization header, which names neither the credential nor its source.
func TestServiceTokenIsTrimmed(t *testing.T) {
	c := Config{Issuer: "https://id.example.com/", ServiceToken: "tok-123\n", PlatformOrgID: " 42 \n"}
	c.defaults()
	if c.ServiceToken != "tok-123" {
		t.Fatalf("ServiceToken = %q, want it trimmed", c.ServiceToken)
	}
	if c.PlatformOrgID != "42" {
		t.Fatalf("PlatformOrgID = %q, want it trimmed", c.PlatformOrgID)
	}
	if c.Issuer != "https://id.example.com" {
		t.Fatalf("Issuer = %q, want no trailing slash", c.Issuer)
	}
	if err := c.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}

// The issuer refuses a write that would change nothing, so a reconcile
// asserting already-correct state receives a 400. Treating that as failure makes
// every steady-state pass log a warning, and a log that cries wolf on each
// restart is one nobody reads when something is genuinely wrong.
func TestUnchangedRejectionIsNotAFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"policy no-op", &apiError{Status: 400, Body: `{"code":9,"message":"Default Login Policy has not been changed (INSTANCE-5M9vdd)"}`}, true},
		{"grant no-op", &apiError{Status: 400, Body: `{"code":9,"message":"User grant has not been changed (COMMAND-2M0fs)"}`}, true},
		{"app oidc no-op", &apiError{Status: 400, Body: `{"code":9,"message":"No changes (COMMAND-1m88i)"}`}, true},
		// A real rejection must still be a failure — matching on status alone
		// would swallow this.
		{"real bad request", &apiError{Status: 400, Body: `{"message":"invalid redirect uri"}`}, false},
		{"not found", &apiError{Status: 404, Body: "no such org"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isUnchanged(c.err); got != c.want {
			t.Errorf("%s: isUnchanged = %v, want %v", c.name, got, c.want)
		}
	}
}
