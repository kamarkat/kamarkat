package constants

// Platform Namespaces - unified across Hub and Spoke (aligned with docs/adr/namespace-alignment.md)
const (
	// CAPI and cluster management
	NamespaceCAPI = "platform-capi"
	
	// Messaging infrastructure
	
	// Identity and authentication
	NamespaceIdentity = "platform-identity"
	
	// Data layer (PostgreSQL, Redis, CNPG)
	NamespaceData = "platform-data"
	
	// Platform operations (ArgoCD, Crossplane, ESO, etc)
	NamespaceOps = "platform-ops"
	
	// Security (Infisical)
	NamespaceSecurity = "platform-security"
	
	// Edge services (AgentGateway, auth-proxy, Ingress)
	NamespaceEdge = "platform-edge"
	
	// Network (Cilium)
	NamespaceNetwork = "platform-network"
	
	// Observability (VictoriaMetrics, Grafana Alloy)
	NamespaceObservability = "platform-observability"
	
	// Control Plane (MCP Server, AgentRegistry) - renamed from Apps
	NamespaceControlPlane = "platform-controlplane"
	
	// Billing (OpenMeter)
	NamespaceBilling = "platform-billing"
	
	// Cloud provider integrations (CCM, CSI) - consolidated to kube-system
	NamespaceCloud = "kube-system"
	
	// Upstream namespaces
	NamespaceCertManager = "cert-manager"
	NamespaceCNPG        = "cnpg-system"
	NamespaceKubeSystem  = "kube-system"
)
