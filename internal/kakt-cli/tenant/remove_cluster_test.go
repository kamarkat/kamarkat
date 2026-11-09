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
	// A workload cluster added before the platform Application was removed from
	// the template carries a bundle.yaml of its own, which is why its presence
	// cannot be what identifies the management cluster. nutgraf-gitops has two.
	write(filepath.Join(filepath.Dir(clusterDir), "bundle.yaml"), "kind: Application\n")
	return dir
}

// The names this package reports are the names the ApplicationSets generate.
//
// They are two statements of one fact in two languages -- Go suffixes here, a
// goTemplate expression in templates/mgmt/workload-clusters.yaml -- and the cost
// of their disagreeing is a status command that reports a cluster "removed"
// because it asked about Applications nobody creates. That is not hypothetical:
// the removal once named <cluster>-platform and <cluster>-generated while
// add-cluster created neither, and every --destroy reported them "not present".
func TestReportedApplicationNamesMatchTheApplicationSets(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	r := ClusterRemoval{GitopsDir: box(t, mgmt, cluster), MgmtCluster: mgmt, Name: cluster}

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "manifests", "tenants",
		"gitops-template", "templates", "mgmt", "workload-clusters.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, app := range r.Applications() {
		suffix := app[len(cluster):] // "-infrastructure", "-generated"
		expr := `name: '{{ index (splitList "/" .path.path) 2 }}` + suffix + `'`
		if !strings.Contains(body, expr) {
			t.Errorf("this package reports %s, but no ApplicationSet template generates %q",
				app, expr)
		}
	}
	if n := strings.Count(body, "kind: ApplicationSet"); n != len(r.Applications()) {
		t.Errorf("%d ApplicationSets for %d reported Applications", n, len(r.Applications()))
	}
}

// The declaration is the cluster's directory, and removing it is the whole
// removal: the Applications are generated from the claim inside it.
func TestRemoveDeclarationRemovesTheClaimDirectory(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	r := ClusterRemoval{GitopsDir: dir, MgmtCluster: mgmt, Name: cluster}

	removed, err := r.RemoveDeclaration()
	if err != nil {
		t.Fatalf("RemoveDeclaration: %v", err)
	}
	if len(removed) != 1 || removed[0] != filepath.Join(RegistryDir, "clusters", cluster) {
		t.Errorf("removed %v, want only the cluster's directory", removed)
	}
	if r.Declared() {
		t.Error("the cluster is still declared after its declaration was removed")
	}
	if _, err := os.Stat(filepath.Join(dir, RegistryDir, "clusters", cluster,
		"infrastructure", "spokepool.yaml")); !os.IsNotExist(err) {
		t.Error("the claim survived the removal, so nothing would delete the cluster")
	}
	// The management cluster's own directory is untouched: it is outside this
	// lifecycle, and a removal that reached into it would take the box.
	for _, keep := range []string{"bundle.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, RegistryDir, "clusters", mgmt, keep)); err != nil {
			t.Errorf("the management cluster lost %s: %v", keep, err)
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

// A cluster that declares its own claim is a workload cluster, whatever else its
// directory carries.
//
// It carries a bundle.yaml in any repository that predates this change, so
// accepting it as the management cluster would let a removal be addressed to the
// wrong directory entirely -- and in a repository with workload clusters, that is
// every one of them.
func TestRemoveDeclarationRefusesAClusterThatIsNotTheManagementOne(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)
	if _, err := os.Stat(filepath.Join(dir, RegistryDir, "clusters", cluster, "bundle.yaml")); err != nil {
		t.Fatal("the workload cluster has no bundle.yaml, so this asserts nothing")
	}
	_, err := (ClusterRemoval{GitopsDir: dir, MgmtCluster: cluster, Name: mgmt}).RemoveDeclaration()
	if err == nil {
		t.Fatal("a workload cluster was accepted as the management cluster")
	}
	if !strings.Contains(err.Error(), "is a workload cluster") {
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

// Removing a cluster NEVER requires touching an environment's applications.
//
// This replaces a test asserting the opposite: it refused while any
// application's clusterName named the cluster, and told the operator to move or
// delete each one. An application declares no cluster now -- it is deployed to
// every cluster serving its environment -- so its definition is the
// environment's and outlives every cluster in it. What the operator gets instead
// is a report of what stops running.
func TestRemoveDeclarationLeavesEnvironmentApplicationsAlone(t *testing.T) {
	const mgmt, cluster = "acme-hub", "acme-stg-01"
	dir := box(t, mgmt, cluster)

	// The cluster serves stg, and stg declares two applications.
	values := filepath.Join(dir, RegistryDir, "clusters", cluster, "values.yaml")
	if err := os.WriteFile(values,
		[]byte("clusterName: "+cluster+"\nenvironmentSlug: stg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"waypoint", "oranger"} {
		appDir := filepath.Join(dir, "environments", "stg", name)
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(appDir, "values.yaml"),
			[]byte("tenantId: acme\nappId: "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := ClusterRemoval{GitopsDir: dir, MgmtCluster: mgmt, Name: cluster}

	// Reported, so the operator sees what stops running.
	env, apps, err := r.AffectedDeployments()
	if err != nil {
		t.Fatalf("AffectedDeployments: %v", err)
	}
	if env != "stg" || strings.Join(apps, ",") != "oranger,waypoint" {
		t.Errorf("affected = %q %v, want stg [oranger waypoint]", env, apps)
	}

	// And not refused.
	if _, err := r.RemoveDeclaration(); err != nil {
		t.Fatalf("the removal was refused because the environment has applications: %v", err)
	}

	// Their definitions are untouched: they belong to the environment.
	for _, name := range []string{"waypoint", "oranger"} {
		if _, err := os.Stat(filepath.Join(dir, "environments", "stg", name, "values.yaml")); err != nil {
			t.Errorf("%s's definition was removed with the cluster: %v", name, err)
		}
	}
}
