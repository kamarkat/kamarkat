package tenant

import (
	"fmt"
	"os"
	"strings"
)

// tier says why a credential is asked for, which decides what happens when it
// is absent.
//
// A tenant should supply credentials only for dependencies that are inherently
// theirs, or that the platform cannot responsibly provide on their behalf.
// Everything else is an implementation detail of ours and must not become an
// onboarding requirement -- which is what happened when these were all made
// mandatory together on the grounds that the hub was not "healthy" without
// them. That reasoning turns whichever backend a component happens to be
// configured for into an architectural obligation on the tenant.
type tier int

const (
	// tierCapability: the tenant selected something that needs it. Refused,
	// because the capability cannot work without it and enabling it anyway
	// produces a box that looks configured and is not.
	tierCapability tier = iota
	// tierDestination: a capability the platform provides that has to send
	// somewhere the tenant chooses. Reported, never refused: the capability
	// still ships and still runs, it simply has no remote destination until one
	// is given, and a tenant may legitimately want none.
	//
	// Deliberately NOT a subscription boundary. No capability is withheld from a
	// tenant who does not pay: the platform is theirs to run in full, and what a
	// subscription buys is the platform being accountable for it. Support
	// telemetry is a separate channel with its own enrolment, and conflating the
	// two here would make a tenant's private monitoring account look like a
	// licence check.
	tierDestination
)

type platformCredential struct {
	// Secret is the name in the tenant's repository, the environment variable
	// Day-0 reads, and the key the operator uploads under. One name, because
	// three spellings is another place for them to disagree.
	Secret string
	Prompt string
	Tier   tier
	// Capability is what selecting it commits the tenant to, named in whichever
	// message the tier produces.
	Capability string
	Field      func(*Secrets) *string
	// Needed reports whether this box asks for it at all. A credential nobody's
	// configuration calls for is not missing.
	Needed func(Spec, Secrets) bool
}

func platformCredentials() []platformCredential {
	// Backups are the platform's responsibility because the platform ships the
	// database, and a backup inside the failure domain it protects is not one.
	// So the target is outside the box and therefore the tenant's to provide --
	// deliberately not in-cluster object storage, which would remove the
	// credential by removing the property that makes the backup worth having.
	//
	// Not in development or ephemeral environments: those boxes are created to
	// be destroyed, and requiring durable object storage for one is asking a
	// tenant to provision real infrastructure to run a test.
	backupsWanted := func(s Spec, _ Secrets) bool {
		return s.Environment != "dev" && s.Environment != "ephemeral"
	}
	// Only when the tenant runs images the platform cannot pull anonymously.
	// ADR-066 makes workloads theirs, so this credential is irreducibly theirs
	// too -- and equally, a tenant running public images should never be asked.
	privateImages := func(s Spec, _ Secrets) bool { return s.PrivateRegistry }
	always := func(Spec, Secrets) bool { return true }

	return []platformCredential{
		// The administrator of THIS box's own secret store. Always required:
		// every box runs an Infisical and every box's is administered by someone.
		//
		// It was not asked for at all, and both the CLI and hub-operator supplied
		// a default -- a personal email address and the literal "Password@123",
		// identical on every box, on the account that can read every secret the
		// platform manages for this tenant. ADR-003 makes Infisical the system of
		// record for secret material and ADR-065 says the platform holds no
		// secret belonging to a tenant; a platform-chosen administrator is both.
		//
		// Registered here rather than taken as a flag so it travels the way every
		// other credential does: one name for the repository secret, the
		// environment variable Day-0 reads, and the key the operator uploads
		// under.
		{
			Secret: "INFISICAL_ADMIN_EMAIL", Tier: tierCapability,
			Capability: "administering this box's own secret store",
			Prompt:     "INFISICAL_ADMIN_EMAIL (the administrator of this box's Infisical): ",
			Field:      func(s *Secrets) *string { return &s.InfisicalAdminEmail },
			Needed:     always,
		},
		{
			Secret: "INFISICAL_ADMIN_PASSWORD", Tier: tierCapability,
			Capability: "administering this box's own secret store",
			Prompt:     "INFISICAL_ADMIN_PASSWORD: ",
			Field:      func(s *Secrets) *string { return &s.InfisicalAdminPassword },
			Needed:     always,
		},
		{
			Secret: "S3_ACCESS_KEY_ID", Tier: tierCapability,
			Capability: "backups of the platform database",
			Prompt:     "S3_ACCESS_KEY_ID (object storage for database backups): ",
			Field:      func(s *Secrets) *string { return &s.S3AccessKeyID },
			Needed:     backupsWanted,
		},
		{
			Secret: "S3_SECRET_ACCESS_KEY", Tier: tierCapability,
			Capability: "backups of the platform database",
			Prompt:     "S3_SECRET_ACCESS_KEY: ",
			Field:      func(s *Secrets) *string { return &s.S3SecretAccessKey },
			Needed:     backupsWanted,
		},
		{
			Secret: "GHCR_USERNAME", Tier: tierCapability,
			Capability: "pulling your own private images",
			Prompt:     "GHCR_USERNAME (to pull your private images): ",
			Field:      func(s *Secrets) *string { return &s.GHCRUsername },
			Needed:     privateImages,
		},
		{
			Secret: "GHCR_TOKEN", Tier: tierCapability,
			Capability: "pulling your own private images",
			Prompt:     "GHCR_TOKEN (a token with read:packages): ",
			Field:      func(s *Secrets) *string { return &s.GHCRToken },
			Needed:     privateImages,
		},
		{
			Secret: "GRAFANA_IRM_ALERTMANAGER_URL", Tier: tierDestination,
			Capability: "paging on this box's critical and warning alerts through your IRM",
			Prompt:     "GRAFANA_IRM_ALERTMANAGER_URL (your IRM Alertmanager integration's URL): ",
			Field:      func(s *Secrets) *string { return &s.GrafanaIRMAlertmanagerURL },
			Needed:     always,
		},
		{
			Secret: "GRAFANA_IRM_HEARTBEAT_URL", Tier: tierDestination,
			Capability: "being paged when this box's hub stops reporting (ADR-078 add.5 §5)",
			Prompt:     "GRAFANA_IRM_HEARTBEAT_URL (the same integration's heartbeat URL): ",
			Field:      func(s *Secrets) *string { return &s.GrafanaIRMHeartbeatURL },
			Needed:     always,
		},
		{
			// A destination: the issuer runs without it, and simply cannot send
			// verification or password-reset mail.
			Secret: "ZITADEL_SMTP_URL", Tier: tierDestination,
			Capability: "the identity provider sending verification and password-reset mail",
			Prompt:     "ZITADEL_SMTP_URL (smtps://resend:<api-key>@smtp.resend.com:465): ",
			Field:      func(s *Secrets) *string { return &s.ZitadelSMTPURL },
			Needed:     always,
		},
	}
}

func (s Secrets) missingInTier(spec Spec, t tier) []platformCredential {
	var missing []platformCredential
	for _, c := range platformCredentials() {
		if c.Tier != t || !c.Needed(spec, s) {
			continue
		}
		if strings.TrimSpace(*c.Field(&s)) == "" {
			missing = append(missing, c)
		}
	}
	return missing
}

// RequireSelectedCapabilities refuses a box that selected something it cannot run.
//
// Only what this box's own configuration calls for: a development box is not
// asked for object storage, and a tenant running public images is never asked
// for a registry credential. A credential nobody's configuration needs is not
// missing, and asking for it anyway is how an implementation detail becomes an
// onboarding requirement.
func (s Secrets) RequireSelectedCapabilities(spec Spec) error {
	missing := s.missingInTier(spec, tierCapability)
	if len(missing) == 0 {
		return nil
	}
	var names, why []string
	seen := map[string]bool{}
	for _, c := range missing {
		names = append(names, c.Secret)
		if !seen[c.Capability] {
			why = append(why, "  - "+c.Capability)
			seen[c.Capability] = true
		}
	}
	return fmt.Errorf("this box selected capabilities it has no credentials for:\n\n%s\n\n"+
		"Missing: %s\n\n"+
		"These are not platform defaults you can ignore -- they are the\n"+
		"dependencies of something this box is configured to do. Either supply\n"+
		"them, or turn the capability off and scaffold without it.",
		strings.Join(why, "\n"), strings.Join(names, ", "))
}

// ObservabilityDestination says where this box's alerts page, if anywhere.
//
// Reported, never refused, and carrying no consequence beyond itself. The
// observability stack runs in the box either way (ADR-078 add.5); without an
// IRM destination alerts are emailed only, which a tenant may well want and is
// not a degraded platform.
//
// It says nothing about support. A tenant pays for the platform to be
// accountable for their box, not for permission to use it, so no capability is
// conditioned on that relationship and this message must not imply one.
func (s Secrets) ObservabilityDestination(spec Spec) string {
	if len(s.missingInTier(spec, tierDestination)) == 0 {
		return "Observability: runs in this box; alerts page through your IRM."
	}
	return "Observability: runs in this box; no remote destination configured for paging.\n" +
		"Metrics, logs, dashboards and alerts all run; nothing is withheld. Alerts are\n" +
		"emailed only, and nothing pages when the hub stops reporting, until the IRM\n" +
		"URLs are added."
}

// fillFromEnvironment supplies any credential still empty from the environment.
//
// Only empties: a flag is an explicit statement by whoever ran the command and
// must win over an ambient variable. Trimmed, and blanks are ignored, so an
// exported-but-empty variable is the same as an unset one rather than a value
// that silently satisfies a required credential.
func (s *Secrets) fillFromEnvironment() {
	for _, c := range platformCredentials() {
		f := c.Field(s)
		if strings.TrimSpace(*f) != "" {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(c.Secret)); v != "" {
			*f = v
		}
	}
}
