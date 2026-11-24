package v1alpha1

import "testing"

// A role belongs to exactly one cluster, and both paths that act on roles -- creation
// and rotation -- must agree which. Getting it wrong either fails on a role that is not
// there or, if a role of the same name exists on two clusters, changes a credential
// nothing is using while the real one goes stale.
func TestRoleClusterResolution(t *testing.T) {
	spec := HubEnvironmentSpec{
		Database: DatabaseConfig{
			Namespace: "platform-data",
			Roles: []DatabaseRole{
				{Name: "hub_grafana", Database: "grafana", ClusterRef: "grafana-db"},
				{Name: "hub_zitadel", Database: "zitadel", ClusterRef: "zitadel-db"},
				{Name: "hub_credits", Database: "credits", ClusterRef: "credits-db"},
			},
		},
	}

	if got := spec.ClusterFor(spec.Database.Roles[0]); got != "grafana-db" {
		t.Errorf("a role naming its cluster resolved to %q", got)
	}

	// A role naming no cluster resolves to NOTHING, and must not inherit any
	// default cluster. Inheriting one is the failure this grain exists to prevent:
	// the role is created on a cluster its application never connects to, and the
	// provisioning reports success.
	if got := spec.ClusterFor(DatabaseRole{Name: "orphan", Database: "postgres"}); got != "" {
		t.Errorf("a role naming no cluster resolved to %q, want no cluster at all", got)
	}

	// Each cluster sees only its own roles.
	for cluster, want := range map[string][]string{
		"grafana-db": {"hub_grafana"},
		"zitadel-db": {"hub_zitadel"},
		"credits-db": {"hub_credits"},
		"absent-db":  nil,
	} {
		got := spec.RolesFor(cluster)
		if len(got) != len(want) {
			t.Errorf("%s got %d role(s), want %d", cluster, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i].Name != want[i] {
				t.Errorf("%s role %d is %q, want %q", cluster, i, got[i].Name, want[i])
			}
		}
	}

	// Every cluster once, in declaration order.
	clusters := spec.DatabaseClusters()
	want := []string{"grafana-db", "zitadel-db", "credits-db"}
	if len(clusters) != len(want) {
		t.Fatalf("DatabaseClusters() = %v, want %v", clusters, want)
	}
	for i := range want {
		if clusters[i] != want[i] {
			t.Errorf("cluster %d is %q, want %q", i, clusters[i], want[i])
		}
	}

	// A box with no declared roles has no clusters to watch.
	if c := (HubEnvironmentSpec{}).DatabaseClusters(); len(c) != 0 {
		t.Errorf("a box with no declared roles reported %v", c)
	}

	// Duplicates collapse, and a role naming no cluster adds none.
	dup := HubEnvironmentSpec{Database: DatabaseConfig{
		Roles: []DatabaseRole{
			{Name: "a", ClusterRef: "grafana-db"},
			{Name: "b", ClusterRef: "grafana-db"},
			{Name: "c"},
		},
	}}
	if c := dup.DatabaseClusters(); len(c) != 1 {
		t.Errorf("duplicate or empty cluster references did not collapse: %v", c)
	}
}
