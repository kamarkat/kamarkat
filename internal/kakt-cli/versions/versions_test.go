package versions

import (
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/kamarkat/kamarkat/internal/assets"
)

// The ClusterClass that provisions the management cluster, and so the authority
// for the Kubernetes version its nodes install.
const managementClassPath = "classes/hetzner-mgmt-ubuntu-v1.yaml"

type providerManifestSpec struct {
	Spec struct {
		Version string `yaml:"version"`
	} `yaml:"spec"`
}

func readProviderManifestVersion(t *testing.T, path string) string {
	t.Helper()
	raw, err := assets.ReadManifest(path)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", path, err)
	}
	var spec providerManifestSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parsing manifest %s: %v", path, err)
	}
	if spec.Spec.Version == "" {
		t.Fatalf("manifest %s has empty spec.version", path)
	}
	return spec.Spec.Version
}

func TestProviderManifestVersionParity(t *testing.T) {
	// THE PARITY CONTRACT (ADR-100 Workstream 3).
	//
	// Constants in versions.go must match the versions specified in the CAPI operator
	// provider manifests exactly. A mismatch means the Go driver and the applied
	// manifests disagree about what is running on the box.
	cases := []struct {
		name     string
		manifest string
		constant string
	}{
		{
			name:     "CAPI Core Provider",
			manifest: "core/capi-operator/providers/core-provider.yaml",
			constant: CAPIVersion,
		},
		{
			name:     "Kubeadm Bootstrap Provider",
			manifest: "core/capi-operator/providers/bootstrap-provider-kubeadm.yaml",
			constant: KubeadmBootstrapProviderVersion,
		},
		{
			name:     "Kubeadm ControlPlane Provider",
			manifest: "core/capi-operator/providers/controlplane-provider-kubeadm.yaml",
			constant: KubeadmControlPlaneProviderVersion,
		},
		{
			name:     "Hetzner Infrastructure Provider (CAPH)",
			manifest: "core/capi-operator/providers/infrastructure-provider-hetzner.yaml",
			constant: HetznerInfraProviderVersion,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readProviderManifestVersion(t, tc.manifest)
			if got != tc.constant {
				t.Fatalf("divergence detected between %s and versions.go: manifest declares %q, versions.go declares %q",
					tc.manifest, got, tc.constant)
			}
		})
	}
}

// TestProviderVersionsAreWellFormed checks the shape of the provider constants,
// not their values.
//
// It replaces a test that asserted the literal versions then deployed. That
// froze the versions in a fourth place -- the provider CR, the constant, and the
// test -- so every legitimate upgrade failed it, and its other half compared the
// constant against a made-up literal that could never match, which asserted
// nothing. Parity is already held by TestProviderManifestVersionParity, which
// compares each constant against the manifest that installs it; a test that also
// pins the value turns that check into a second thing to edit rather than a
// guard.
func TestProviderVersionsAreWellFormed(t *testing.T) {
	semver := regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	for name, got := range map[string]string{
		"CAPIOperatorVersion":                CAPIOperatorVersion,
		"CAPIVersion":                        CAPIVersion,
		"KubernetesVersion":                  KubernetesVersion,
		"KubeadmBootstrapProviderVersion":    KubeadmBootstrapProviderVersion,
		"KubeadmControlPlaneProviderVersion": KubeadmControlPlaneProviderVersion,
		"HetznerInfraProviderVersion":        HetznerInfraProviderVersion,
		"CertManagerVersion":                 CertManagerVersion,
	} {
		if !semver.MatchString(got) {
			t.Errorf("%s is %q; want a v-prefixed three-part version", name, got)
		}
	}

	// The two kubeadm providers are released together as part of Cluster API, so
	// they are never independently chosen.
	if KubeadmBootstrapProviderVersion != KubeadmControlPlaneProviderVersion {
		t.Errorf("kubeadm providers disagree: bootstrap %q, control-plane %q",
			KubeadmBootstrapProviderVersion, KubeadmControlPlaneProviderVersion)
	}
}

// TestKubernetesVersionHasOneDeclaration replaces a parity test, and inverts it.
//
// The management ClusterClass used to export a literal KUBERNETES_VERSION, and
// the old test asserted it equalled versions.KubernetesVersion: two declarations
// held level by a check. The spoke ClusterClass had the same arrangement and
// drifted anyway, because nothing compared either copy to spec.topology.version
// -- which is what CAPI actually tells kubeadm to join as. On 2026-11-05 the
// templates said 1.35.10 while the cluster was v1.31.6, and the replacement
// control-plane node installed kubelet 1.35.10, could not join a 1.31.6 control
// plane, and never registered.
//
// There is one declaration now. provisioner.go renders versions.KubernetesVersion
// into spec.topology.version, and the class derives the package version from
// builtin.controlPlane.version and builtin.machineDeployment.version. What needs
// guarding is therefore not equality but the absence of a second declaration.
func TestKubernetesVersionHasOneDeclaration(t *testing.T) {
	raw, err := assets.ReadManifest(managementClassPath)
	if err != nil {
		t.Fatalf("reading %s: %v", managementClassPath, err)
	}
	class := string(raw)

	// A literal is an export whose value starts with a digit, optionally v-prefixed.
	// The derivations the class does keep -- "${KUBERNETES_VERSION_RAW#v}" and
	// "${KUBERNETES_VERSION%.*}" -- are not literals and must stay.
	if m := regexp.MustCompile(`export (?:TRIMMED_)?KUBERNETES_VERSION=v?[0-9]`).FindAllString(class, -1); len(m) > 0 {
		t.Fatalf("%s declares a literal Kubernetes version (%v): the version reaches a node "+
			"from spec.topology.version through builtin.controlPlane.version and "+
			"builtin.machineDeployment.version, and a literal here is the second "+
			"declaration that drift lives in", managementClassPath, m)
	}

	for _, builtin := range []string{"builtin.controlPlane.version", "builtin.machineDeployment.version"} {
		if !strings.Contains(class, builtin) {
			t.Fatalf("%s does not inject the Kubernetes version from %s: without it a node "+
				"installs no kubelet at all, which is the intended failure, but it also means "+
				"the version authority is gone", managementClassPath, builtin)
		}
	}
}
