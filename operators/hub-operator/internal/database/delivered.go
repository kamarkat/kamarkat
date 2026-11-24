// Package database names the hub application login roles' delivered credentials.
//
// CloudNativePG owns each role's existence (DatabaseRole); OpenBao owns its
// password (a static role, ADR-117 §7.3); ESO delivers it, through a
// VaultDynamicSecret, into the consumer's own namespace. Nothing here talks to
// PostgreSQL.
package database

// DeliveredSecret is the Secret ESO writes a login role's current credential
// into, beside the application that uses it. HubEnvironment's
// ApplicationSecretsReady waits for every one of them.
func DeliveredSecret(role string) (namespace, name string) {
	switch role {
	case "hub_zitadel":
		return "platform-identity", "zitadel-db-credentials"
	case "hub_credits":
		return "platform-credits", "credit-authority-db"
	case "hub_grafana":
		return "platform-observability", "grafana-db"
	}
	// An undeclared mapping is reported as missing under a name that says so,
	// rather than waited on under a guessed one.
	return "unmapped", role
}
