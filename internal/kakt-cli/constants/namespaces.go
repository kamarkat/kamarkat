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

// GatewayClassName is the GatewayClass every cluster in the fleet declares, hub and
// spoke alike (gateway/files/gatewayclass.yaml, spoke-catalog/infra/gatewayclass.yaml).
//
// Named here because Day-0 asserts it: Cilium accepting this class is the only
// observable proof that its Gateway API controller is running, and the controller is
// what gives a Gateway, an HTTPRoute or a GRPCRoute a status at all.
const GatewayClassName = "cilium"
