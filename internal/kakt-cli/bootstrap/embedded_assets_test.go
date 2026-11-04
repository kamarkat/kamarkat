package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kamarkat/kamarkat/internal/platform/embedded"
)

// The embed set is an allowlist (scripts/package/embed-platform-assets.sh), and a
// development build never consults it: ADR-068 sends a development binary to the
// working tree, where every manifest exists. So a Day-0 read of a path outside the
// set passes every local test and fails at a tenant's first bootstrap, with a
// "no such file or directory" naming a file the repository plainly has.
//
// These tests ask the embedded tree for what Day-0 actually reads. They fail at
// pull-request time rather than at release time, which is the difference between
// noticing and shipping.
//
// The dimensions are read from the working tree rather than listed here, so a new
// provider, environment or boundary is covered by existing tests on the commit
// that adds it -- a list would have to be remembered, and the thing being guarded
// against is precisely a set that someone forgot to extend.

func embeddedHas(t *testing.T, path string) {
	t.Helper()
	if _, err := embedded.FS.ReadFile(path); err != nil {
		t.Errorf("a released CLI carries no %s, so Day-0 cannot read it.\n"+
			"Add the path to PATHS in scripts/package/embed-platform-assets.sh "+
			"and re-run it: %v", path, err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "..", dir))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("%s is empty; this test would pass vacuously", dir)
	}
	return names
}

// Each driver names a Cilium artifact and reads its provider's config base
// (provider_cloud.go:130, :157). Both are per-provider and neither is optional:
// without the first a hub installs no CNI, without the second cilium-operator
// hangs on a missing cilium-config.
func TestEmbeddedCarriesEveryDriverDatapath(t *testing.T) {
	drivers := map[string]interface {
		Name() string
		CiliumAddonPath() string
	}{
		"hetzner": &HetznerDriver{},
		"hybrid":  &HybridDriver{Driver: &HetznerDriver{}},
	}

	for name, d := range drivers {
		t.Run(name, func(t *testing.T) {
			embeddedHas(t, d.CiliumAddonPath())
			embeddedHas(t, filepath.Join(
				"manifests", "providers", d.Name(), "k8s", "cilium-config-base.yaml"))
		})
	}
}

// Read by both cloud drivers (driver_hetzner.go:397, driver_hybrid.go:191). The
// path is hetzner's under either provider, because a hybrid cell's cloud half is
// still Hetzner.
func TestEmbeddedCarriesTheCCMAddon(t *testing.T) {
	embeddedHas(t, filepath.Join("manifests", "providers", "hetzner",
		"base", "spoke-addons", "ccm-addon-template.yaml"))
}

// hubdomain.go:116-117 reads the environment's patch and the base it patches.
//
// The patch is optional -- hubdomain.go:121-125 skips one that does not exist and
// falls back to the base, and `ephemeral` has none -- so what is asserted is that
// the two trees agree, not that every environment carries a patch. A patch the
// repository has and the binary does not is the failure: it means a released box
// silently takes base defaults where a local one takes the environment's.
func TestEmbeddedCarriesEveryEnvironment(t *testing.T) {
	embeddedHas(t, filepath.Join("manifests", "environments", "base", "hubenvironment.yaml"))

	for _, env := range dirNames(t, filepath.Join("manifests", "environments")) {
		if env == "base" {
			continue
		}
		t.Run(env, func(t *testing.T) {
			rel := filepath.Join("manifests", "environments", env, "patch-hubenvironment.yaml")
			if _, err := os.Stat(filepath.Join("..", "..", "..", rel)); err != nil {
				t.Skipf("%s has no patch; hubdomain falls back to the base", env)
			}
			embeddedHas(t, rel)
		})
	}
}

// boundary_inventory.go:88 lists a boundary's descriptors to know what it must
// generate. An absent directory is indistinguishable from an empty boundary
// there, so a missing embed reports "0 components" rather than an error -- which
// is why this is checked here and not left to the caller.
func TestEmbeddedCarriesEveryBoundaryInventory(t *testing.T) {
	for _, boundary := range dirNames(t, filepath.Join("manifests", "argocd", "components")) {
		t.Run(boundary, func(t *testing.T) {
			dir := filepath.Join("manifests", "argocd", "components", boundary)
			entries, err := embedded.FS.ReadDir(dir)
			if err != nil {
				t.Fatalf("a released CLI carries no %s: %v", dir, err)
			}
			if len(entries) == 0 {
				t.Errorf("%s is embedded but empty; the boundary would report "+
					"zero components instead of failing", dir)
			}
		})
	}
}

// orchestrator.go:1547 reads the ADR-045 artifact registry; scaffold.go:447 walks
// the tenant template. The registry is a file and the template a tree, and a
// released CLI needs both before it has anything else.
func TestEmbeddedCarriesTheArtifactRegistryAndTenantTemplate(t *testing.T) {
	embeddedHas(t, filepath.Join("manifests", "generated", "artifacts.yaml"))

	const tmpl = "manifests/tenants/gitops-template"
	entries, err := embedded.FS.ReadDir(tmpl)
	if err != nil {
		t.Fatalf("a released CLI carries no %s, so it can scaffold nothing: %v", tmpl, err)
	}
	if len(entries) == 0 {
		t.Errorf("%s is embedded but empty; scaffolding would silently render "+
			"an empty repository", tmpl)
	}
}

// `kakt support scope` and `kakt support preview` exist so a tenant can read
// what would be sent BEFORE it is sent (ADR-077). A released binary could not:
// the allowlist is the only file those commands read and it was not in the embed
// set, so both failed with "release 0.1.16-rc.6 carries no
// manifests/hub-core-services/support-agent/allowlist.yaml" -- naming a file the
// repository plainly has.
//
// Not a Day-0 read, which is why the tests above did not cover it. The rule is
// wider than Day-0: anything a RELEASED binary reads has to be carried, and a
// promise a tenant can only keep inside a checkout is not one this platform
// makes.
func TestEmbeddedCarriesTheSupportAllowlist(t *testing.T) {
	embeddedHas(t, filepath.Join("manifests", "hub-core-services",
		"support-agent", "allowlist.yaml"))
}
