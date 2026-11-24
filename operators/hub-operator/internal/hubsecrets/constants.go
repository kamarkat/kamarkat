package hubsecrets

// Namespaces the Day-0 source Secrets live in, and the two key names the
// tailscale mappings share with their consumers.
const (
	NamespaceCAPI          = "platform-capi"
	NamespaceOps           = "platform-ops"
	NamespaceCloudSystem   = "kube-system"
	NamespaceData          = "platform-data"
	NamespaceIdentity      = "platform-identity"
	NamespaceObservability = "platform-observability"

	KeyTailscaleAuthkey  = "tailscale-authkey"
	KeyTailscaleHostname = "tailscale-hostname"
)
