package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every resource in the spoke catalogue whose CRD is delivered outside the
// catalogue must carry SkipDryRunOnMissingResource=true.
//
// ArgoCD validates a whole sync before applying any of it. A resource whose
// kind the destination API does not yet serve fails that validation, and the
// failure is not scoped to the resource -- the entire Application is rejected
// with "one or more synchronization tasks are not valid", having applied
// nothing.
//
// The Cilium CRDs arrive with Cilium itself, in the CAPI ClusterResourceSet
// addon (ADR-041): outside this Application, outside ArgoCD, on the spoke's own
// schedule. So on a new spoke the catalogue and the CRDs are in a race the
// catalogue can lose.
//
// It lost on 2026-09-18. platform-spoke-catalog-nutgraf-01's five retries ran
// 15:29:11 -> 15:41:18; ciliumclusterwidenetworkpolicies.cilium.io was
// established at 15:43:24, two minutes and six seconds later. One resource
// blocked all 351, and nothing retried afterwards -- ArgoCD's automated sync
// refuses a revision whose previous attempt failed, selfHeal included, so a
// two-minute race left a permanently empty spoke. It surfaced 63 minutes into
// the run as "the spoke's shared-cnpg has no ready instance", naming a database
// that had never been asked to exist.
//
// Asserted against the manifests rather than left to review: the annotation is
// invisible when it works, the failure it prevents appears an hour away and
// describes something else entirely, and two of these files already existed
// without it.
func TestSpokeCatalogExternallyProvidedCRDsSkipDryRun(t *testing.T) {
	root := repoRoot(t)
	catalog := filepath.Join(root, "manifests", "spoke", "spoke-catalog")

	// Kinds whose CRDs no manifest in this catalogue installs. Cilium's come
	// from the CAPI addon; a kind added here without its CRD being added to the
	// catalogue belongs in this list and needs the annotation.
	external := map[string]string{
		"CiliumClusterwideNetworkPolicy": "the CAPI ClusterResourceSet addon (ADR-041)",
		"CiliumNetworkPolicy":            "the CAPI ClusterResourceSet addon (ADR-041)",
	}

	var checked int
	err := filepath.WalkDir(catalog, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		// templated-fields.yaml NAMES kinds, it does not declare objects: its
		// entries are {kind, name, fields} telling the packager which rendered
		// object to template. A `kind:` there is a selector, and the annotation
		// this test looks for belongs on the manifest it selects -- which is
		// checked on its own file.
		if filepath.Base(path) == "templated-fields.yaml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(raw)

		for kind, source := range external {
			if !strings.Contains(body, "kind: "+kind) {
				continue
			}
			checked++
			if !strings.Contains(body, "SkipDryRunOnMissingResource=true") {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s declares a %s but does not carry\n"+
					"    argocd.argoproj.io/sync-options: SkipDryRunOnMissingResource=true\n"+
					"Its CRD comes from %s, so on a fresh spoke ArgoCD's dry-run rejects\n"+
					"the ENTIRE catalogue -- every other object included -- and automated\n"+
					"sync will not retry the same revision afterwards.",
					rel, kind, source)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", catalog, err)
	}

	// A catalogue that no longer contains any of these kinds would pass
	// vacuously, and the next one added would be unguarded.
	if checked == 0 {
		t.Fatalf("no externally-provided-CRD resources found under %s; "+
			"either the catalogue moved or the kind list is stale", catalog)
	}
}
