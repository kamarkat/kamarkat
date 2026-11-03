package versions

import (
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/soloz-io/zero-ops/internal/assets"
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

// managementClassKubernetesVersion returns the KUBERNETES_VERSION and
// TRIMMED_KUBERNETES_VERSION the management ClusterClass installs on its nodes.
func managementClassKubernetesVersion(t *testing.T) (full, trimmed string) {
	t.Helper()
	raw, err := assets.ReadManifest(managementClassPath)
	if err != nil {
		t.Fatalf("reading %s: %v", managementClassPath, err)
	}
	for _, m := range regexp.MustCompile(`export KUBERNETES_VERSION=(\S+)`).FindAllStringSubmatch(string(raw), -1) {
		if full != "" && full != m[1] {
			t.Fatalf("%s exports two different KUBERNETES_VERSION values: %q and %q",
				managementClassPath, full, m[1])
		}
		full = m[1]
	}
	for _, m := range regexp.MustCompile(`export TRIMMED_KUBERNETES_VERSION=(\S+)`).FindAllStringSubmatch(string(raw), -1) {
		if trimmed != "" && trimmed != m[1] {
			t.Fatalf("%s exports two different TRIMMED_KUBERNETES_VERSION values: %q and %q",
				managementClassPath, trimmed, m[1])
		}
		trimmed = m[1]
	}
	if full == "" || trimmed == "" {
		t.Fatalf("%s exports no KUBERNETES_VERSION or TRIMMED_KUBERNETES_VERSION", managementClassPath)
	}
	return full, trimmed
}

// TestKubernetesVersionParity guards the one version with no single home: the
// constant below is rendered into the Cluster's spec.topology.version, while the
// ClusterClass cloud-init installs kubelet and kubeadm from the apt repository
// its own exports name. If they disagree, CAPI asks for one version and the
// nodes come up on another -- version skew on a control plane that is being
// created, which is both the worst time for it and the hardest place to see it.
func TestKubernetesVersionParity(t *testing.T) {
	full, trimmed := managementClassKubernetesVersion(t)

	if got, want := KubernetesVersion, "v"+full; got != want {
		t.Fatalf("divergence between %s and versions.go: the class installs %s, versions.go declares %s",
			managementClassPath, want, got)
	}

	// The trimmed value builds the package repository URL, so a stale copy sends
	// nodes to the wrong apt repository rather than failing outright.
	if want := strings.Join(strings.Split(full, ".")[:2], "."); trimmed != want {
		t.Fatalf("%s exports TRIMMED_KUBERNETES_VERSION=%s, which is not the major.minor of %s (%s)",
			managementClassPath, trimmed, full, want)
	}
}
