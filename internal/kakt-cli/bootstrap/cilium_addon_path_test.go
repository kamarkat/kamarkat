package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// Every driver's Cilium addon must exist where the driver says it does, and must
// be carried by a released binary.
//
// This is the check that was missing. The path used to be derived as
// `manifests/providers/<name>/k8s/cilium-addon-<name>.yaml`, which was true of
// hybrid and had never been true of hetzner -- nothing asserted the file existed,
// so a released hetzner build failed at cluster-provision with "release carries
// no ..." after provisioning kind, cert-manager and four CAPI providers. Roughly
// three minutes of a tenant's run to discover a missing file.
func TestEveryDriverCiliumAddonExistsAndIsEmbedded(t *testing.T) {
	root := repoRoot(t)
	drivers := map[string]CloudDriver{
		"hetzner": &HetznerDriver{},
		"hybrid":  &HybridDriver{},
	}

	for name, d := range drivers {
		path := d.CiliumAddonPath()
		if path == "" {
			t.Errorf("%s declares no cilium addon path", name)
			continue
		}

		// In the working tree.
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("%s names %s, which does not exist: %v", name, path, err)
		}

		// And in the tree a released binary carries. The embed list is a separate
		// declaration from the driver's, and the two silently disagreeing is what
		// makes a released build fail where a development one works.
		embedded := filepath.Join(root, "internal", "platform", "embedded", path)
		if _, err := os.Stat(embedded); err != nil {
			t.Errorf("%s names %s, which a released binary does not carry "+
				"(missing at %s). Add it to PATHS in scripts/package/embed-platform-assets.sh: %v",
				name, path, embedded, err)
		}
	}
}
