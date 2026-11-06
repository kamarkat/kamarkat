package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// box writes the smallest repository remove-cluster accepts: a management
// cluster with a platform declaration, and one workload cluster added the way
// add-cluster adds it.
func box(t *testing.T, mgmt, cluster string) string {
	t.Helper()
	dir := t.TempDir()
	mgmtDir := filepath.Join(dir, RegistryDir, "clusters", mgmt)
	clusterDir := filepath.Join(dir, RegistryDir, "clusters", cluster, "infrastructure")
	for _, d := range []string{mgmtDir, clusterDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(mgmtDir, "bundle.yaml"), "kind: Application\n")
	write(filepath.Join(clusterDir, "spokepool.yaml"), "kind: SpokePool\n")
	for _, app := range workloadClusterApps {
		write(filepath.Join(dir, app.appFile(mgmt, cluster)), "kind: Application\n")
	}
	return dir
}

// Every Application add-cluster creates is one remove-cluster knows about.
//
// They were two lists: the removal named <cluster>-platform and
// <cluster>-generated, and add-cluster created neither, so `--destroy` reported
// both "not present" on every box while its help text named them as what it
// deletes. One inventory is the only thing that keeps them in step.
func TestRemovalCoversEveryApplicationAddCreates(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	r := ClusterRemoval{GitopsDir: dir, MgmtCluster: mgmt, Name: cluster}

	for _, app := range r.Applications() {
		file := filepath.Join(dir, RegistryDir, "clusters", mgmt, app+".yaml")
		if _, err := os.Stat(file); err != nil {
			t.Errorf("remove-cluster deletes the Application %s, but nothing creates %s", app, file)
		}
	}
	if len(r.Applications()) != len(workloadClusterApps) {
		t.Errorf("the removal names %d Applications, the inventory has %d",
			len(r.Applications()), len(workloadClusterApps))
	}
}

// The declaration is the cluster's directory AND both of its Applications in the
// management cluster's. Leaving one behind leaves the root applying an
// Application whose source path has just been deleted.
func TestRemoveDeclarationTakesTheApplicationsWithTheDirectory(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	r := ClusterRemoval{GitopsDir: dir, MgmtCluster: mgmt, Name: cluster}

	removed, err := r.RemoveDeclaration()
	if err != nil {
		t.Fatalf("RemoveDeclaration: %v", err)
	}
	if len(removed) != 1+len(workloadClusterApps) {
		t.Errorf("removed %v, want the cluster directory and %d Applications",
			removed, len(workloadClusterApps))
	}
	if r.Declared() {
		t.Error("the cluster is still declared after its declaration was removed")
	}
	for _, app := range workloadClusterApps {
		if _, err := os.Stat(filepath.Join(dir, app.appFile(mgmt, cluster))); !os.IsNotExist(err) {
			t.Errorf("%s survived the removal", app.appFile(mgmt, cluster))
		}
	}
}

// A repository scaffolded before root.yaml existed can still remove a cluster.
//
// Day-0 treats root.yaml as optional and says so, so requiring it here told the
// oldest boxes they had "no management cluster" -- naming the wrong cause, and
// leaving them no supported way to remove anything.
func TestRemoveDeclarationWorksWithoutRootApplication(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	if _, err := os.Stat(filepath.Join(dir, RegistryDir, "clusters", mgmt, "root.yaml")); !os.IsNotExist(err) {
		t.Fatal("this box was written with a root.yaml; the test asserts nothing")
	}
	if _, err := (ClusterRemoval{GitopsDir: dir, MgmtCluster: mgmt, Name: cluster}).RemoveDeclaration(); err != nil {
		t.Errorf("an older repository cannot remove a cluster: %v", err)
	}
}

// A directory with no platform declaration is not the management cluster.
func TestRemoveDeclarationRefusesAClusterThatIsNotTheManagementOne(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	_, err := (ClusterRemoval{GitopsDir: dir, MgmtCluster: cluster, Name: mgmt}).RemoveDeclaration()
	if err == nil {
		t.Fatal("a workload cluster was accepted as the management cluster")
	}
	if !strings.Contains(err.Error(), "declares no platform") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// The box's autoscaler names its burst pool by name, and a name that stops
// resolving leaves it waiting for a kubeconfig nobody writes -- healthy, with no
// burst capacity at all.
func TestRemoveDeclarationRefusesWhileTheAutoscalerNamesTheCluster(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	values := filepath.Join(dir, RegistryDir, "clusters", mgmt, "values.yaml")
	if err := os.WriteFile(values, []byte("burstSpokePool: "+cluster+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := ClusterRemoval{GitopsDir: dir, MgmtCluster: mgmt, Name: cluster}

	_, err := r.RemoveDeclaration()
	if err == nil {
		t.Fatal("the cluster was removed while the autoscaler still named it")
	}
	for _, want := range []string{"burstSpokePool", cluster} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
	if !r.Declared() {
		t.Error("the declaration was removed anyway")
	}

	// Another cluster's name is not this one's.
	if err := os.WriteFile(values, []byte("burstSpokePool: acme-prod-01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RemoveDeclaration(); err != nil {
		t.Errorf("a burst pool naming another cluster blocked this one: %v", err)
	}
}

// An application still declared on the cluster blocks the removal, and is named.
func TestRemoveDeclarationRefusesWhileAnAppRunsOnTheCluster(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	appDir := filepath.Join(dir, "environments", "stg", "waypoint")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "values.yaml"),
		[]byte("clusterName: "+cluster+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (ClusterRemoval{GitopsDir: dir, MgmtCluster: mgmt, Name: cluster}).RemoveDeclaration()
	if err == nil {
		t.Fatal("the cluster was removed while an app declared it")
	}
	if !strings.Contains(err.Error(), filepath.Join("environments", "stg", "waypoint")) {
		t.Errorf("error does not name the app that blocks it: %v", err)
	}
}

// The destroy gate reads the root's own view of git, and an unknown answer is
// not permission.
func TestDestroyBlocked(t *testing.T) {
	r := ClusterRemoval{GitopsDir: t.TempDir(), MgmtCluster: "acme-hub", Name: "acme-stg-01"}
	infra := r.Name + "-infrastructure"
	generated := r.Name + "-generated"

	cases := []struct {
		name     string
		root     []RootResource
		blocked  bool
		contains string
	}{{
		// The root always manages the management cluster's own Applications, so
		// nothing at all is a root that has not reported -- a hard refresh, or an
		// Application that has never synced. Destroying on that answer and having
		// the root recreate the Application builds a new cluster under a name whose
		// secrets and backups belong to the old one.
		name:     "a root that has reported nothing",
		root:     nil,
		blocked:  true,
		contains: "reports no resources",
	}, {
		name: "the declaration is still in git",
		root: []RootResource{
			{Kind: "Application", Name: "acme-hub-platform"},
			{Kind: "Application", Name: infra},
		},
		blocked:  true,
		contains: infra,
	}, {
		name: "another of the cluster's Applications is still in git",
		root: []RootResource{
			{Kind: "Application", Name: "acme-hub-platform"},
			{Kind: "Application", Name: generated},
			{Kind: "Application", Name: infra, RequiresPruning: true},
		},
		blocked:  true,
		contains: generated,
	}, {
		name: "the root has stopped applying both",
		root: []RootResource{
			{Kind: "Application", Name: "acme-hub-platform"},
			{Kind: "Application", Name: generated, RequiresPruning: true},
			{Kind: "Application", Name: infra, RequiresPruning: true},
		},
		blocked: false,
	}, {
		// A destroy that already ran: the Applications are gone from the root's
		// status entirely, and the root is reporting its own.
		name:    "the cluster is gone from the root",
		root:    []RootResource{{Kind: "Application", Name: "acme-hub-platform"}},
		blocked: false,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := r.DestroyBlocked(c.root)
			if c.blocked && err == nil {
				t.Fatal("destroy was permitted")
			}
			if !c.blocked && err != nil {
				t.Fatalf("destroy was refused: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), c.contains) {
				t.Errorf("error does not name %q: %v", c.contains, err)
			}
		})
	}
}
