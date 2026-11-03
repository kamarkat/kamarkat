package versions

const (
	// CAPI Operator
	CAPIOperatorVersion = "v0.29.0"

	// Kubernetes
	//
	// Rendered into spec.topology.version of the management cluster's Cluster
	// object (cluster/provisioner.go), which asks CAPI for a control plane at
	// this version.
	//
	// The AUTHORITY is the management ClusterClass, whose cloud-init exports
	// KUBERNETES_VERSION and a trimmed major.minor companion -- those install
	// kubelet and kubeadm from the matching apt repository. This constant must
	// equal them, because a mismatch creates a cluster whose topology declares
	// one version while its nodes install another: skew on a brand-new control
	// plane, at Day-0, on the cluster everything else is built from.
	//
	// TestKubernetesVersionParity asserts the equality.
	KubernetesVersion = "v1.35.10"

	// CAPI Core
	CAPIVersion = "v1.13.7"

	// Bootstrap & Control Plane Providers
	//
	// Authoritative for the versions deployed by the CAPI operator manifests in
	// internal/assets/manifests/core/capi-operator/providers/.
	// Divergence between these constants and the provider manifests is caught by
	// TestProviderManifestVersionParity.
	KubeadmBootstrapProviderVersion    = "v1.13.7"
	KubeadmControlPlaneProviderVersion = "v1.13.7"

	// Infrastructure Providers
	HetznerInfraProviderVersion = "v1.1.10"

	// Binaries the CLI downloads are NOT declared here.
	//
	// clusterctl and packer are pinned in internal/soloz-cli/binaries/<tool>.go,
	// next to the per-platform checksums their download is verified against, and
	// that is what scripts/package/inventory.py reads (scan_bootstrap). Copies
	// lived here and were read by nothing, so editing them looked like a version
	// bump and changed nothing -- do not re-add them.

	// Prerequisites
	CertManagerVersion = "v1.21.3"

	// ArgoCDChartVersion is the argo-proj/argo-cd chart the Day-0 seed installs.
	//
	// It MUST equal the targetRevision of the 'platform-argocd' element in
	// manifests/argocd/components/01/platform-argocd.yaml.
	// That Application adopts the release this constant installs, at sync wave 1 --
	// so a mismatch makes ArgoCD upgrade itself while the rest of boundary 01 is
	// still syncing, restarting the application-controller and repo-server at the
	// worst possible moment. It was 7.7.12 here against 7.8.0 in Git until
	// 2026-09-05; 7.8.0 against 7.9.1 until 2026-09-20.
	//
	// scripts/validate-argocd-seed-parity.sh enforces the equality in CI.
	ArgoCDChartVersion = "10.11.7"
)
