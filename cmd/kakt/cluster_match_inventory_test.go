package main

import "testing"

func srv(id int64, machineName, machineType string) hcloudServer {
	labels := map[string]string{}
	if machineName != "" {
		labels[caphMachineNameLabel] = machineName
	}
	if machineType != "" {
		labels[caphMachineTypeLabel] = machineType
	}
	return hcloudServer{ID: id, Name: machineName, Labels: labels}
}

// Every classification, because each one is a decision about whether reconciliation
// may resume, and the wrong answer in either direction is destructive: releasing on
// an unmatched object provisions a parallel fleet, and releasing on a server with no
// object remediates a machine that is serving.
func TestClassifyInventoryCoversEveryRelationship(t *testing.T) {
	machines := []hcloudMachine{
		{Name: "wl-cp-aaa", ProviderID: "hcloud://101"}, // matched, id agrees
		{Name: "wl-md-bbb"},                             // matched, no id yet
		{Name: "wl-md-ccc", ProviderID: "hcloud://999"}, // conflicting
		{Name: "wl-md-ddd", ProviderID: "hcloud://104"}, // ambiguous: two servers
		{Name: "wl-md-eee"},                             // object without server
	}
	servers := []hcloudServer{
		srv(101, "wl-cp-aaa", "control_plane"),
		srv(102, "wl-md-bbb", "worker"),
		srv(103, "wl-md-ccc", "worker"), // object says 999
		srv(104, "wl-md-ddd", "worker"),
		srv(105, "wl-md-ddd", "worker"), // duplicate name
		srv(106, "wl-md-fff", "worker"), // server without object
		srv(107, "", "worker"),          // owner label but no machine name
	}

	rows, matched, unresolved := classifyInventory(machines, servers)

	states := map[string]string{}
	for _, r := range rows {
		k := r.Object
		if k == "-" {
			k = r.Server
		}
		states[k] = r.State
	}

	for k, want := range map[string]string{
		"wl-cp-aaa": stateMatched,
		"wl-md-bbb": stateMatched,
		"wl-md-ccc": stateConflicting,
		"wl-md-ddd": stateAmbiguous,
		"wl-md-eee": stateObjectWithoutServer,
		"106":       stateServerWithoutObject,
		"107":       stateUnattributable,
	} {
		if states[k] != want {
			t.Errorf("%s classified %q, want %q", k, states[k], want)
		}
	}
	if matched != 2 {
		t.Errorf("matched=%d, want 2", matched)
	}
	if unresolved != 5 {
		t.Errorf("unresolved=%d, want 5 (conflict, ambiguous, object-without-server, "+
			"server-without-object, unattributable)", unresolved)
	}
}

// A fleet with nothing recovered and nothing at the provider is not an error, and a
// fleet that matches completely must report zero unresolved — that is the only state
// in which controllers may be released.
func TestClassifyInventoryEdges(t *testing.T) {
	// An empty comparison classifies as nothing, which is correct here and is
	// precisely why the CALLER must refuse it rather than read "no unresolved rows"
	// as "matched". runClusterMatchInventory rejects an empty object set for that
	// reason; see the comment there.
	if rows, m, u := classifyInventory(nil, nil); len(rows) != 0 || m != 0 || u != 0 {
		t.Errorf("an empty inventory gave %d rows, %d matched, %d unresolved", len(rows), m, u)
	}

	// Servers with no objects at all must never classify as matched: that is the
	// shape of a hub whose management state was never restored.
	onlyServers := []hcloudServer{srv(1, "a", "worker"), srv(2, "b", "worker")}
	if _, m, u := classifyInventory(nil, onlyServers); m != 0 || u != 2 {
		t.Errorf("servers with no objects gave matched=%d unresolved=%d, want 0 and 2", m, u)
	}

	machines := []hcloudMachine{{Name: "a", ProviderID: "hcloud://1"}, {Name: "b", ProviderID: "hcloud://2"}}
	servers := []hcloudServer{srv(1, "a", "control_plane"), srv(2, "b", "worker")}
	if _, m, u := classifyInventory(machines, servers); m != 2 || u != 0 {
		t.Errorf("a fully matched fleet gave matched=%d unresolved=%d, want 2 and 0", m, u)
	}
}

// The provider id is how the deployed provider correlates first, so a shape it cannot
// parse must read as "no id" rather than as zero — which would compare equal to
// nothing and silently look like a conflict.
func TestServerIDFromProviderID(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   int64
		wantOK bool
	}{
		{"hcloud://12345", 12345, true},
		{"", 0, false},
		{"hcloud://", 0, false},
		{"hcloud://abc", 0, false},
		{"aws:///eu-central-1a/i-123", 0, false},
		{"12345", 0, false},
	} {
		got, ok := serverIDFromProviderID(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("serverIDFromProviderID(%q) = %d,%v; want %d,%v",
				tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
