package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Application over clusters/<name>/generated/ must never prune.
//
// That directory holds hub-cluster.yaml, the CAPI Cluster describing the machines
// the box runs on. Pruning it tells ArgoCD to delete the Cluster, and CAPI answers
// by destroying every node including the control plane -- so a deleted file, a bad
// rebase or a renamed directory would take out the estate. Nothing else in a
// tenant's repository has that reach, and the cost of leaving prune off is a stale
// generated artifact.
//
// selfHeal must stay on: it is what makes the file authoritative over a `kubectl
// edit`, which is the whole point of recording the topology in Git.
func TestGeneratedApplicationNeverPrunes(t *testing.T) {
	root := repoRootForTest(t)
	base := filepath.Join(root, "manifests", "tenants", "gitops-template", "templates")

	// The workload cluster's is generated-app.yaml: like infrastructure-app.yaml
	// beside it, it is hydrated into the MANAGEMENT cluster's directory, which is
	// the only directory anything applies.
	for kind, file := range map[string]string{
		"mgmt":             "generated.yaml",
		"workload-cluster": "generated-app.yaml",
	} {
		path := filepath.Join(base, kind, file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		// The template carries scaffolding placeholders, which are not YAML values
		// but are lexically fine as scalars, so it parses as-is.
		var app struct {
			Spec struct {
				SyncPolicy struct {
					Automated struct {
						Prune    bool `yaml:"prune"`
						SelfHeal bool `yaml:"selfHeal"`
					} `yaml:"automated"`
				} `yaml:"syncPolicy"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(raw, &app); err != nil {
			t.Errorf("%s: parse: %v", path, err)
			continue
		}
		if app.Spec.SyncPolicy.Automated.Prune {
			t.Errorf("%s prunes. This directory can hold a CAPI Cluster, and pruning "+
				"one destroys every node it describes, control plane included.", kind)
		}
		if !app.Spec.SyncPolicy.Automated.SelfHeal {
			t.Errorf("%s does not self-heal, so a kubectl edit of the topology would "+
				"outlive what Git says and capacity would have two sources.", kind)
		}
	}
}

// The file Day-0 writes must warn against the one edit that wedges a cluster.
func TestRecordedTopologyWarnsAboutAutoscalerAnnotations(t *testing.T) {
	root := repoRootForTest(t)
	raw, err := os.ReadFile(filepath.Join(root,
		"internal", "kakt-cli", "cluster", "provisioner.go"))
	if err != nil {
		t.Fatalf("read provisioner: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		"cluster-api-autoscaler-node-group-",
		"replicas",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the recorded topology header no longer mentions %q; CAPI rejects "+
				"a topology carrying both replicas and the autoscaler bounds, and that "+
				"rejection wedges the object", want)
		}
	}
}
