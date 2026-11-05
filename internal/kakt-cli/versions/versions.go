package versions

const (
	// CAPI Operator
	CAPIOperatorVersion = "v0.29.0"

	// Kubernetes
	//
	// THIS IS THE DECLARATION, for the management cluster. cluster/provisioner.go
	// renders it into spec.topology.version of the Cluster object, CAPI hands that
	// version to the control plane and to each MachineDeployment, and the
	// ClusterClass writes it onto the node from builtin.controlPlane.version and
	// builtin.machineDeployment.version -- which is the apt version kubelet and
	// kubeadm are installed at. One declaration, carried to the thing that acts on
	// it. (A workload cluster declares its own in the provider composition's
	// spec.topology.version; the same derivation carries it from there.)
	//
	// The class used to export KUBERNETES_VERSION itself, with this constant held
	// equal to it by a test. That is the arrangement the spoke class still had when
	// it drifted on 2026-11-05: templates at 1.35.10, cluster at v1.31.6, and a
	// replacement control-plane node that installed kubelet 1.35.10, could not join
	// a 1.31.6 control plane, and never registered. Two literals compared to each
	// other, and neither compared to the version CAPI was reconciling to.
	//
	// TestKubernetesVersionHasOneDeclaration asserts the class declares none.
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
	// clusterctl and packer are pinned in internal/kakt-cli/binaries/<tool>.go,
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
