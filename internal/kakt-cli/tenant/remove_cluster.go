package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClusterRemoval takes a workload cluster out of a tenant's box, in the two
// steps the management cluster's own reconciliation forces.
//
// The declaration leaves git first. The management cluster's root Application
// applies registry/clusters/<mgmt>/ with selfHeal and prune: false, so an
// infrastructure Application deleted while its file is still there is recreated
// on the next pass -- and the cluster with it. Once the file is gone the root
// stops applying it but, prune being off, leaves it in place: a bad merge must
// not be able to destroy a cluster. Destroying it is then a deliberate delete
// on the management cluster, which is the second step.
//
// The name is never reused. It names the cluster's secret path and backup
// prefix, which outlive it.
type ClusterRemoval struct {
	// GitopsDir is the tenant repository.
	GitopsDir string
	// MgmtCluster is the management cluster that created this one.
	MgmtCluster string
	// Name is the workload cluster being removed.
	Name string
}

// Applications are the management cluster's Applications for this cluster, in
// the order they are deleted: what runs on it first, the cluster itself last.
// Deleting the infrastructure Application destroys the cluster, after which the
// other has nowhere to delete from.
//
// Read from workloadClusterApps, the same list `add-cluster` creates them from.
// It was a literal here, naming <cluster>-platform and <cluster>-generated, and
// nothing created either: every `--destroy` reported them "not present" while
// this command's own help named them as what it deletes.
func (r ClusterRemoval) Applications() []string {
	apps := make([]string, 0, len(workloadClusterApps))
	for _, app := range workloadClusterApps {
		apps = append(apps, r.Name+"-"+app.suffix)
	}
	return apps
}

// RootApplication is the management cluster's Application that applies its
// registry directory, the one whose view of git gates the second step.
func (r ClusterRemoval) RootApplication() string {
	return r.MgmtCluster + "-root"
}

// declaration is what the first step deletes: the cluster's own directory, and
// every Application of its own that sits in the management cluster's directory.
//
// Both halves, from the same inventory as everything else. Deleting only the
// infrastructure Application would leave <cluster>-generated declared in git,
// and the root would go on applying an Application whose source path had just
// been deleted -- a permanent ComparisonError on the box, for a cluster that no
// longer exists.
func (r ClusterRemoval) declaration() (dir string, appFiles []string) {
	dir = filepath.Join(r.GitopsDir, RegistryDir, "clusters", r.Name)
	for _, app := range workloadClusterApps {
		appFiles = append(appFiles, filepath.Join(r.GitopsDir, app.appFile(r.MgmtCluster, r.Name)))
	}
	return dir, appFiles
}

// Declared reports whether any part of the declaration is still in the
// working tree.
func (r ClusterRemoval) Declared() bool {
	dir, appFiles := r.declaration()
	if _, err := os.Stat(dir); err == nil {
		return true
	}
	for _, p := range appFiles {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// RemoveDeclaration checks the cluster can go and deletes its declaration from
// the working tree. It returns the paths removed, relative to the repository.
func (r ClusterRemoval) RemoveDeclaration() ([]string, error) {
	if r.Name == r.MgmtCluster {
		return nil, fmt.Errorf("%q is the management cluster: it holds every other cluster's "+
			"definition and is not removed this way. To delete the whole box, use kakt teardown",
			r.Name)
	}
	// What identifies a management cluster: its platform declaration.
	//
	// bundle.yaml is the cluster's declaration of the platform it runs (ADR-064)
	// -- the file Day-0 requires before it will bootstrap anything, and the one
	// `kakt propose` reads a version out of. A workload cluster has none, because
	// the platform reaches it through the management cluster's own boundaries, so
	// in a tenant repository this file IS the management cluster.
	//
	// It was root.yaml, which Day-0 treats as optional and says so: "root.yaml
	// postdates the repositories scaffolded before it. A box without one still
	// reconciles what it has". So a repository scaffolded before root.yaml existed
	// was told it had "no management cluster" -- naming the wrong cause, and
	// leaving the only supported way to remove a cluster unusable on exactly the
	// boxes that have been running longest.
	mgmtDir := filepath.Join(r.GitopsDir, RegistryDir, "clusters", r.MgmtCluster)
	if _, err := os.Stat(filepath.Join(mgmtDir, "bundle.yaml")); err != nil {
		return nil, fmt.Errorf("%s declares no platform: --mgmt-cluster names the management "+
			"cluster of this repository, whose directory carries bundle.yaml (ADR-064). A "+
			"workload cluster has none", mgmtDir)
	}

	dir, appFiles := r.declaration()
	claims, err := filepath.Glob(filepath.Join(dir, "infrastructure", "spokepool*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("no workload cluster named %q: this box declares none at "+
			"%s/clusters/%s/infrastructure", r.Name, RegistryDir, r.Name)
	}

	if err := r.burstPoolReference(); err != nil {
		return nil, err
	}

	users, err := r.appsOnCluster()
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		return nil, fmt.Errorf("%d application(s) still run on %q:\n  %s\n\n"+
			"Removing the cluster would remove them, and their databases, with no commit "+
			"that says so. Move each to another cluster or remove it, commit, and wait "+
			"until it is gone from %s",
			len(users), r.Name, strings.Join(users, "\n  "), r.Name)
	}

	var removed []string
	for _, p := range append([]string{dir}, appFiles...) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return removed, fmt.Errorf("remove %s: %w", p, err)
		}
		rel, err := filepath.Rel(r.GitopsDir, p)
		if err != nil {
			rel = p
		}
		removed = append(removed, rel)
	}
	return removed, nil
}

// burstPoolReference refuses while the box's own autoscaler names this cluster.
//
// burstSpokePool is the workload cluster the hub's cluster-autoscaler adds cloud
// capacity to, and it is a name, not a reference the platform can follow. The
// chart's own note on that value records what a stale one costs: an autoscaler
// "waiting forever on a kubeconfig for a hybrid spoke it never provisioned,
// while the pool it did provision had none" -- healthy, and doing nothing.
//
// Refused at declaration for the same reason an application is: this is the last
// point where the cause is still one field in one file.
func (r ClusterRemoval) burstPoolReference() error {
	path := filepath.Join(r.GitopsDir, RegistryDir, "clusters", r.MgmtCluster, "values.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		// A box that declares no values declares no burst pool. The management
		// cluster itself was established above, so this is not the missing-box
		// case and must not be reported as one.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var box struct {
		BurstSpokePool string `yaml:"burstSpokePool"`
	}
	if err := yaml.Unmarshal(raw, &box); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if strings.TrimSpace(box.BurstSpokePool) != r.Name {
		return nil
	}
	rel, relErr := filepath.Rel(r.GitopsDir, path)
	if relErr != nil {
		rel = path
	}
	return fmt.Errorf("%s still names %q as its burst pool:\n  %s: burstSpokePool: %s\n\n"+
		"The hub's cluster-autoscaler adds cloud capacity to that cluster by name, and a name "+
		"that no longer resolves leaves it waiting for a kubeconfig no one will write -- "+
		"reporting healthy while the box has no burst capacity at all.\n\n"+
		"Point burstSpokePool at another cluster or clear it, commit, then remove this one",
		r.MgmtCluster, r.Name, rel, box.BurstSpokePool)
}

// RootResource is one entry of the management cluster's root Application status:
// an object the root is managing, and whether git has stopped declaring it.
type RootResource struct {
	Kind            string
	Name            string
	RequiresPruning bool
}

// DestroyBlocked reports why this cluster must not be destroyed yet, reading the
// root Application's own view of git. A nil error is permission to delete.
//
// prune: false is what makes this readable at all. A file removed from git stays
// applied and the root marks it requiresPruning, so an entry WITHOUT that flag is
// one the root still reads from git and would recreate -- and recreating an
// infrastructure Application provisions a new cluster under a name whose secret
// path and backup prefix belong to the old one.
//
// IT FAILS CLOSED ON AN UNKNOWN ANSWER. An empty status is not "the cluster is
// gone from git", it is "the root has not said" -- a hard refresh, a root that
// has not reconciled since the push, an Application that has never synced. The
// root always manages the management cluster's own Applications, so a status with
// nothing in it never describes a healthy box, and treating it as permission is
// how a cluster gets destroyed and immediately rebuilt. root.yaml makes the same
// argument about its own directory filter: an allowlist fails closed on the thing
// it forgot, a denylist fails open.
func (r ClusterRemoval) DestroyBlocked(root []RootResource) error {
	if len(root) == 0 {
		return fmt.Errorf("%s reports no resources, so its view of git cannot be read: a root "+
			"that has not reconciled is not the same as a declaration that is gone.\n\n"+
			"Refresh it (argocd app get %s --refresh, or kubectl annotate it with "+
			"argocd.argoproj.io/refresh=normal), wait for it to report, then re-run",
			r.RootApplication(), r.RootApplication())
	}
	managed := map[string]bool{}
	for _, res := range root {
		if res.Kind == "Application" && !res.RequiresPruning {
			managed[res.Name] = true
		}
	}
	for _, app := range r.Applications() {
		if managed[app] {
			return fmt.Errorf("%s still applies %s from git: the removal is not pushed, or not "+
				"synced yet.\n\nPush the commit that removes it, wait for %s to sync, then re-run",
				r.RootApplication(), app, r.RootApplication())
		}
	}
	return nil
}

// appsOnCluster lists every application declaration that names this cluster,
// across every environment.
func (r ClusterRemoval) appsOnCluster() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(r.GitopsDir, "environments", "*", "*", "values.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read environments: %w", err)
	}
	var users []string
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", m, err)
		}
		var decl struct {
			ClusterName string `yaml:"clusterName"`
		}
		if err := yaml.Unmarshal(raw, &decl); err != nil {
			return nil, fmt.Errorf("parse %s: %w", m, err)
		}
		if decl.ClusterName == r.Name {
			rel, err := filepath.Rel(r.GitopsDir, filepath.Dir(m))
			if err != nil {
				rel = filepath.Dir(m)
			}
			users = append(users, rel)
		}
	}
	sort.Strings(users)
	return users, nil
}
