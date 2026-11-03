package v1alpha1

import "testing"

// A role belongs to exactly one cluster, and both paths that act on roles -- creation
// and rotation -- must agree which. Getting it wrong either fails on a role that is not
// there or, if a role of the same name exists on two clusters, changes a credential
// nothing is using while the real one goes stale.
func TestRoleClusterResolution(t *testing.T) {
	spec := HubEnvironmentSpec{
		Database: DatabaseConfig{
			ClusterRef: "infisical-db",
			Namespace:  "platform-data",
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

	// A role naming no cluster resolves to NOTHING, and must not inherit the box's
	// bootstrap cluster. Inheriting it is the failure this grain exists to prevent:
	// the role is created on a cluster its application never connects to, and the
	// provisioning reports success.
	if got := spec.ClusterFor(DatabaseRole{Name: "orphan", Database: "postgres"}); got != "" {
		t.Errorf("a role naming no cluster resolved to %q, want no cluster at all", got)
	}

	// Each cluster sees only its own roles, and the bootstrap cluster sees none: the
	// one role on it is Infisical's, which CNPG owns through spec.managed.roles.
	for cluster, want := range map[string][]string{
		"grafana-db":   {"hub_grafana"},
		"zitadel-db":   {"hub_zitadel"},
		"credits-db":   {"hub_credits"},
		"infisical-db": nil,
		"absent-db":    nil,
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

	// Every cluster once, the bootstrap cluster first, in declaration order after it.
	clusters := spec.DatabaseClusters()
	want := []string{"infisical-db", "grafana-db", "zitadel-db", "credits-db"}
	if len(clusters) != len(want) {
		t.Fatalf("DatabaseClusters() = %v, want %v", clusters, want)
	}
	for i := range want {
		if clusters[i] != want[i] {
			t.Errorf("cluster %d is %q, want %q", i, clusters[i], want[i])
		}
	}

	// A box with no declared roles still reports its bootstrap cluster: Infisical's
	// database is there, so the operator has somewhere to connect before any
	// application exists.
	bare := HubEnvironmentSpec{Database: DatabaseConfig{ClusterRef: "infisical-db"}}
	if c := bare.DatabaseClusters(); len(c) != 1 || c[0] != "infisical-db" {
		t.Errorf("a box with no declared roles reported %v", c)
	}

	// Duplicates collapse, and a role naming no cluster adds none.
	dup := HubEnvironmentSpec{Database: DatabaseConfig{
		ClusterRef: "infisical-db",
		Roles: []DatabaseRole{
			{Name: "a", ClusterRef: "grafana-db"},
			{Name: "b", ClusterRef: "grafana-db"},
			{Name: "c"},
		},
	}}
	if c := dup.DatabaseClusters(); len(c) != 2 {
		t.Errorf("duplicate or empty cluster references did not collapse: %v", c)
	}
}
