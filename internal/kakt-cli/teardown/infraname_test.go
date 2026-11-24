package teardown

import (
	"os"
	"path/filepath"
	"testing"
)

// box7: the resources were labelled with the generated HetznerCluster name and
// teardown excluded all five. The ref is read from the cluster's own object, so
// a look-alike cluster's resources still never match.
func TestOwnershipKeysIncludeTheClustersOwnInfrastructureName(t *testing.T) {
	list := []byte(`{"items":[
	 {"metadata":{"name":"nutgraf-mgmt-1794929668"},"spec":{"infrastructureRef":{"kind":"HetznerCluster","name":"nutgraf-mgmt-1794929668-n9zg4"}}},
	 {"metadata":{"name":"nutgraf-mgmt-1794929668-x"},"spec":{"infrastructureRef":{"kind":"HetznerCluster","name":"nutgraf-mgmt-1794929668-x-aaaaa"}}}]}`)
	got, err := infraNames(list, []string{"nutgraf-mgmt-1794929668"})
	if err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{infraName: got}
	keys := o.ownershipKeys(nil, []string{"nutgraf-mgmt-1794929668"})
	if len(keys) != 2 || keys[1] != "nutgraf-mgmt-1794929668-n9zg4" {
		t.Fatalf("keys %v", keys)
	}
	r := []Resource{
		{Kind: "server", Name: "a", Labels: map[string]string{"caph-cluster-nutgraf-mgmt-1794929668-n9zg4": "owned"}},
		{Kind: "server", Name: "b", Labels: map[string]string{"caph-cluster-nutgraf-mgmt-1794929668-x-aaaaa": "owned"}},
	}
	at := Attribute(r, keys)
	if !at[0].Eligible || at[1].Eligible {
		t.Fatalf("eligible: own=%v look-alike=%v", at[0].Eligible, at[1].Eligible)
	}
}

func TestTheBoxRecordIsRequiredAndCanOnlyNameThisCluster(t *testing.T) {
	dir := t.TempDir()
	o := &Orchestrator{ClusterName: "nutgraf-mgmt-1794929668", GitopsDir: dir}
	if _, err := o.recordedInfraName(); err == nil {
		t.Fatal("a missing box record was not an error")
	}
	write := func(name string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Join(dir, ".state"), 0o755)
		body := `{"clusterName":"nutgraf-mgmt-1794929668","metadata":{"infrastructureName":"` + name + `"}}`
		if err := os.WriteFile(filepath.Join(dir, ".state", "nutgraf-mgmt-1794929668.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("nutgraf-mgmt-1794929668-n9zg4")
	if got, err := o.recordedInfraName(); err != nil || got != "nutgraf-mgmt-1794929668-n9zg4" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"nutgraf-01-abcde", "nutgraf-mgmt-1794929668-x-aaaaa", "nutgraf-mgmt-1794929668-"} {
		write(bad)
		if _, err := o.recordedInfraName(); err == nil {
			t.Errorf("a record naming %q was accepted", bad)
		}
	}
}
