package tenant

import (
	"strings"
	"testing"
)

func prodSpec() Spec {
	return Spec{Provider: "hetzner", PrivateRegistry: true}
}

func withEverything(spec Spec) Secrets {
	s := Secrets{
		ProviderToken:      "p",
		GitopsToken:        "g",
		EscrowURL:          "https://app.infisical.com",
		EscrowProjectID:    "proj",
		EscrowClientID:     "cid",
		EscrowClientSecret: "csec",
	}
	for _, c := range platformCredentials() {
		if c.Needed(spec, s) {
			*c.Field(&s) = "value"
		}
	}
	return s
}

// A capability the box selected cannot work without its credential, so it is
// refused. The refusal must name the capability, not just the variable: a
// tenant reading "S3_ACCESS_KEY_ID is missing" learns nothing about why a
// database backup target is their concern.
func TestSelectedCapabilitiesAreRequired(t *testing.T) {
	spec := prodSpec()
	for _, c := range platformCredentials() {
		if c.Tier != tierCapability {
			continue
		}
		// A DERIVED credential is exempt, because it is not the operator's to
		// supply: the box Infisical administrator is generated and escrowed when
		// the management cluster is declared, so at the moment this gate runs it
		// does not exist yet and refusing for its absence refused every box.
		if c.Derived {
			continue
		}
		t.Run(c.Secret, func(t *testing.T) {
			s := withEverything(spec)
			*c.Field(&s) = ""

			err := s.RequireSelectedCapabilities(spec)
			if err == nil {
				t.Fatalf("a box missing %s was accepted", c.Secret)
			}
			if !strings.Contains(err.Error(), c.Secret) {
				t.Errorf("refusal does not name %s:\n%v", c.Secret, err)
			}
			if !strings.Contains(err.Error(), c.Capability) {
				t.Errorf("refusal does not say what %s is for:\n%v", c.Secret, err)
			}
		})
	}
}

// No capability is withheld, and a tenant's own monitoring account is not a
// licence check. A box without a destination must build, and the message must
// not imply the platform is giving it less.
func TestObservabilityDestinationIsNeverRequired(t *testing.T) {
	spec := prodSpec()
	s := withEverything(spec)
	for _, c := range platformCredentials() {
		if c.Tier == tierDestination {
			*c.Field(&s) = ""
		}
	}
	if err := s.RequireSelectedCapabilities(spec); err != nil {
		t.Fatalf("a box declining telemetry was refused: %v", err)
	}
	msg := s.ObservabilityDestination(spec)
	if !strings.Contains(msg, "no remote destination") {
		t.Errorf("the state is not stated:\n%s", msg)
	}
	if !strings.Contains(msg, "nothing is withheld") {
		t.Errorf("the message must not imply a reduced platform:\n%s", msg)
	}
	// The subscription buys accountability, not access. A message that ties a
	// missing monitoring account to support would read as a licence check.
	for _, w := range []string{"unsupported", "Unsupported", "subscription", "support"} {
		if strings.Contains(msg, w) {
			t.Errorf("observability must not be framed as a support boundary (%q):\n%s", w, msg)
		}
	}
}

// Every box is asked for them (ADR-111 Revision 7 §3).
//
// This asserted the opposite -- that a "dev" or "ephemeral" box is not asked --
// on the reasoning that such a box is created to be destroyed. The hub declares no
// environment to be one of those, and the hub's PostgreSQL clusters declare a
// barmanObjectStore unconditionally, so a box that was never asked archived to a
// destination it could not authenticate against.
func TestBackupCredentialsAreAskedOfEveryBox(t *testing.T) {
	spec := Spec{Provider: "hetzner"}
	s := Secrets{ProviderToken: "p", GitopsToken: "g"}
	var asked []string
	for _, c := range s.missingInTier(spec, tierCapability) {
		if strings.HasPrefix(c.Secret, "S3_") {
			asked = append(asked, c.Secret)
		}
	}
	if len(asked) != 2 {
		t.Errorf("expected both S3 credentials to be asked for, got %v", asked)
	}
}

// A tenant running public images must never be asked for a registry credential:
// ADR-066 makes workloads theirs, and the platform cannot tell from outside
// which kind they run. Declared, not inferred.
func TestRegistryCredentialFollowsTheDeclaration(t *testing.T) {
	public := Spec{Provider: "hetzner", PrivateRegistry: false}
	s := Secrets{ProviderToken: "p", GitopsToken: "g"}
	for _, c := range s.missingInTier(public, tierCapability) {
		if strings.HasPrefix(c.Secret, "GHCR_") {
			t.Errorf("%s asked of a box running public images", c.Secret)
		}
	}

	private := prodSpec()
	var asked bool
	for _, c := range s.missingInTier(private, tierCapability) {
		if strings.HasPrefix(c.Secret, "GHCR_") {
			asked = true
		}
	}
	if !asked {
		t.Error("a box declaring private images was not asked for a registry credential")
	}
}

// The names are the contract between what scaffolding writes, what the workflow
// forwards, and what Day-0 reads. A rename that updates one is the failure.
func TestSecretNamesMatchWhatDayZeroReads(t *testing.T) {
	dayZero := map[string]bool{
		"S3_ACCESS_KEY_ID": true, "S3_SECRET_ACCESS_KEY": true,
		"GRAFANA_IRM_ALERTMANAGER_URL": true, "GRAFANA_IRM_HEARTBEAT_URL": true,
		"ZITADEL_SMTP_URL": true,
		"GHCR_USERNAME":    true, "GHCR_TOKEN": true,
		// Read by internal/kakt-cli/infisical (Day-0) and by hub-operator, which
		// both used to default them to a personal email and "Password@123" --
		// identical on every box, on the account that can read every secret the
		// platform manages for that tenant.
		"INFISICAL_ADMIN_EMAIL": true, "INFISICAL_ADMIN_PASSWORD": true,
	}
	for _, c := range platformCredentials() {
		if !dayZero[c.Secret] {
			t.Errorf("%s is collected but Day-0 reads no such variable", c.Secret)
		}
		delete(dayZero, c.Secret)
	}
	for name := range dayZero {
		t.Errorf("Day-0 reads %s but nothing collects it", name)
	}
}
