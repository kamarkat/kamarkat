package teardown

import "testing"

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
