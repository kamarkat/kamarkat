package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Helm template and the Go constant state the same rule for different
// installation moments. Nothing but this test stops them drifting, and drift
// means the platform disagrees with itself about where the platform runs --
// visible only as pods Pending on some boxes and not others.
func TestGoPlacementMatchesTheHelmTemplate(t *testing.T) {
	tpl, err := os.ReadFile(filepath.Join("..", "..", "..",
		"manifests", "argocd", "environment-manager", "templates", "_placement.tpl"))
	if err != nil {
		t.Skipf("template not readable: %v", err)
	}

	// The body of the define, which is plain YAML once the guard is stripped.
	body := regexp.MustCompile(
		`(?s)define "environment-manager\.cloudControlPlacement".*?\{\{-\s*if[^}]*\}\}(.*?)\{\{-\s*end\s*\}\}`,
	).FindSubmatch(tpl)
	if body == nil {
		t.Fatal("cloudControlPlacement is not in _placement.tpl; the Go constant now has no counterpart")
	}

	var fromTemplate map[string]any
	if err := yaml.Unmarshal(body[1], &fromTemplate); err != nil {
		t.Fatalf("the template's placement is not valid YAML: %v", err)
	}

	var patch map[string]any
	if err := json.Unmarshal([]byte(cloudControlPlacementPatch), &patch); err != nil {
		t.Fatalf("cloudControlPlacementPatch is not valid JSON: %v", err)
	}
	deployment := patch["spec"].(map[string]any)["deployment"].(map[string]any)

	for _, field := range []string{"affinity", "tolerations"} {
		want, ok := fromTemplate[field]
		if !ok {
			t.Errorf("the template does not set %q but the Go patch does", field)
			continue
		}
		got := deployment[field]
		if !reflect.DeepEqual(normalise(want), normalise(got)) {
			t.Errorf("%s differs between _placement.tpl and placement.go\n  template: %#v\n  go:       %#v",
				field, want, got)
		}
	}
}

// The patch must clear nodeSelector, not merely omit it. Every box bootstrapped
// before this carries nodeSelector: {workload-location: on-prem} from the
// decision this reverses; a merge patch leaves untouched keys in place, so
// omitting it leaves "must be on-prem" beside "must not be on-prem" and the
// controllers never schedule.
func TestPlacementClearsTheOldNodeSelector(t *testing.T) {
	var patch map[string]any
	if err := json.Unmarshal([]byte(cloudControlPlacementPatch), &patch); err != nil {
		t.Fatal(err)
	}
	deployment := patch["spec"].(map[string]any)["deployment"].(map[string]any)

	v, present := deployment["nodeSelector"]
	if !present {
		t.Fatal("nodeSelector is absent from the patch; an existing on-prem selector would survive and contradict the affinity")
	}
	if v != nil {
		t.Errorf("nodeSelector = %#v, want null (null is what removes it)", v)
	}
}

// NotIn, never a control-plane nodeSelector: a node lacking the label satisfies
// NotIn, which is what makes this a no-op on an all-cloud box.
func TestPlacementUsesNotInSoItIsANoOpWithoutOnPremNodes(t *testing.T) {
	var patch map[string]any
	json.Unmarshal([]byte(cloudControlPlacementPatch), &patch)
	raw, _ := json.Marshal(patch)
	s := string(raw)

	for _, want := range []string{`"operator":"NotIn"`, `"workload-location"`, `"on-prem"`} {
		if !strings.Contains(s, want) {
			t.Errorf("patch does not contain %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"node-role.kubernetes.io/control-plane":""`) {
		t.Error("patch pins to the control-plane node by selector; a hybrid box with Hetzner workers should be able to use those")
	}
}

// The hybrid driver must implement the interface the applier looks for. A
// renamed method silently makes placeCAPIControllers a no-op, and the
// controllers go back to the tenant's premises with nothing reporting it.
func TestHybridDriverSatisfiesThePlacementInterface(t *testing.T) {
	var d any = &HybridDriver{}
	placer, ok := d.(interface{ CAPIPlacement() string })
	if !ok {
		t.Fatal("HybridDriver no longer implements CAPIPlacement; placeCAPIControllers would skip silently")
	}
	if placer.CAPIPlacement() != cloudControlPlacementPatch {
		t.Error("the hybrid driver returns something other than the shared rule")
	}
}

func normalise(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}
