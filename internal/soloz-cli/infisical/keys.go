package infisical

// Infisical secret key names for ExternalSecret remoteRefs.
// Must match operators/hub-operator/internal/infisical/constants.go.
const (
	KeyInfisicalDBUsername = "infisical-db-username"
	KeyInfisicalDBPassword = "infisical-db-password"
	KeyTailscaleAuthkey    = "tailscale-authkey"
	KeyTailscaleHostname   = "tailscale-hostname"
)
