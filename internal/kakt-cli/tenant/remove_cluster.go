package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClusterRemoval takes a workload cluster out of a tenant's box by deleting its
// declaration. THAT IS THE WHOLE REMOVAL.
//
// The cluster exists because registry/clusters/<name>/infrastructure/spokepool.yaml
// exists: the management cluster's two ApplicationSets are generated from that
// file. Commit its absence and the generated Applications stop being produced,
// the infrastructure one is deleted with Argo CD's resources-finalizer, the claim
// goes with it, and Crossplane and Cluster API unwind the cluster.
//
// So this command edits the repository and nothing else. There is no second,
// credentialled step: the state it used to close -- declaration gone, cluster
// running -- cannot be reached, because the commit is what deletes the cluster.
// What the commit cannot do is report progress, which `kakt tenant cluster-status`
// does by observation (ADR-114 §5).
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
// the order the platform deletes them: what runs on it first, the cluster itself
// last.
//
// Observed, not created and not deleted by this package: the ApplicationSets
// generate them from the claim file. `cluster-status` asks about exactly these
// two, which is why the suffixes are one list (workloadClusterAppSuffixes) rather
// than a literal here -- a literal drifted once already, naming
// <cluster>-platform and <cluster>-generated when nothing created either.
func (r ClusterRemoval) Applications() []string {
	apps := make([]string, 0, len(workloadClusterAppSuffixes))
	for _, suffix := range workloadClusterAppSuffixes {
		apps = append(apps, r.Name+"-"+suffix)
	}
	return apps
}

// declaration is what this command deletes: the cluster's own directory, which
// holds the claim that declares it.
//
// ONE PATH, where there used to be three. The two Applications were flat files in
// the management cluster's directory and had to be deleted alongside it; they are
// generated from the claim now, so the directory is the whole declaration and
// nothing outside it names this cluster.
func (r ClusterRemoval) declaration() string {
	return filepath.Join(r.GitopsDir, RegistryDir, "clusters", r.Name)
}

// Declared reports whether the declaration is still in the working tree.
func (r ClusterRemoval) Declared() bool {
	_, err := os.Stat(r.declaration())
	return err == nil
}

// RemoveDeclaration checks the cluster can go and deletes its declaration from
// the working tree. It returns the paths removed, relative to the repository.
func (r ClusterRemoval) RemoveDeclaration() ([]string, error) {
	if r.Name == r.MgmtCluster {
		return nil, fmt.Errorf("%q is the management cluster: it holds every other cluster's "+
			"definition and is not removed this way. To delete the whole box, use kakt teardown",
			r.Name)
	}
	// What identifies a management cluster: nothing in this repository creates it.
	//
	// A workload cluster exists because a SpokePool claim in its own directory
	// says so, and the management cluster's ArgoCD reconciles that claim. The
	// management cluster has no claim -- Day-0 brought it into being -- so the
	// absence of one is the fact that tells them apart, and it is true in every
	// layout this platform has shipped.
	//
	// Two readings were tried and both were wrong. root.yaml is optional, and Day-0
	// says so: "root.yaml postdates the repositories scaffolded before it. A box
	// without one still reconciles what it has" -- so requiring it told the oldest
	// boxes they had "no management cluster". bundle.yaml does not discriminate: the
	// workload-cluster template shipped one until the platform Application was
	// removed from it, so every cluster added before that carries a bundle.yaml too,
	// and a repository with workload clusters would accept any of them as its
	// management cluster.
	mgmtDir := filepath.Join(r.GitopsDir, RegistryDir, "clusters", r.MgmtCluster)
	if _, err := os.Stat(mgmtDir); err != nil {
		return nil, fmt.Errorf("no cluster at %s: --mgmt-cluster names the management cluster "+
			"of this repository, the one holding the other clusters' "+
			"<name>-infrastructure.yaml", mgmtDir)
	}
	mgmtClaims, err := filepath.Glob(filepath.Join(mgmtDir, "infrastructure", "spokepool*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", mgmtDir, err)
	}
	if len(mgmtClaims) > 0 {
		return nil, fmt.Errorf("%q is a workload cluster, not the management cluster: it "+
			"declares its own SpokePool claim at %s/clusters/%s/infrastructure.\n\n"+
			"The management cluster is the one with no claim of its own -- it holds the other "+
			"clusters' <name>-infrastructure.yaml instead",
			r.MgmtCluster, RegistryDir, r.MgmtCluster)
	}

	dir := r.declaration()
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

	// NO APPLICATION REFUSAL, and that is the architecture rather than a relaxed
	// check.
	//
	// It refused while any application's `clusterName` named this cluster, and
	// told the operator to "move each to another cluster or remove it". An
	// application declares no cluster now: it is deployed to every cluster serving
	// its environment, so there is nothing to move and nothing in
	// environments/<env>/ that removing a cluster should make anyone edit. The
	// definitions are the environment's and outlive every cluster in it.
	//
	// What the operator does need is to know what stops running, which
	// AffectedDeployments reports and the command prints before it removes
	// anything.

	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("remove %s: %w", dir, err)
	}
	rel, err := filepath.Rel(r.GitopsDir, dir)
	if err != nil {
		rel = dir
	}
	return []string{rel}, nil
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

// AffectedDeployments names what stops running when this cluster goes: the
// environment it serves, and that environment's applications.
//
// Reported, never refused. Each of those applications is deployed to every
// cluster serving the environment, so removing this one removes its instances
// and leaves the others -- and leaves every definition in environments/<env>/
// exactly where it is. An operator is entitled to see the list before the
// instances go; they are not being asked to change anything in it.
func (r ClusterRemoval) AffectedDeployments() (environment string, apps []string, err error) {
	raw, err := os.ReadFile(filepath.Join(r.GitopsDir, RegistryDir, "clusters", r.Name, "values.yaml"))
	if err != nil {
		// A cluster with no declaration left has no environment to read, which is
		// the normal state on the second step of a removal.
		return "", nil, nil
	}
	var decl struct {
		EnvironmentSlug string `yaml:"environmentSlug"`
	}
	if err := yaml.Unmarshal(raw, &decl); err != nil {
		return "", nil, fmt.Errorf("parse %s's declaration: %w", r.Name, err)
	}
	environment = strings.TrimSpace(decl.EnvironmentSlug)
	if environment == "" {
		return "", nil, nil
	}

	entries, err := os.ReadDir(filepath.Join(r.GitopsDir, "environments", environment))
	if err != nil {
		return environment, nil, nil
	}
	for _, e := range entries {
		if e.IsDir() {
			apps = append(apps, e.Name())
		}
	}
	sort.Strings(apps)
	return environment, apps, nil
}

// DeclaredWorkloadClusters names every workload cluster this box declares, which
// is the set its management cluster has to outlive.
//
// Read from the repository rather than from the management cluster, because the
// caller that needs it most cannot reach the management cluster: a teardown of a
// box whose API is gone still has to know which clusters it created, and the
// declaration is the only source that survives the cluster.
//
// A workload cluster is a directory under registry/clusters/ with a SpokePool
// claim of its own -- the same discriminator RemoveDeclaration uses, for the same
// reason: the management cluster's directory holds the others'
// <name>-infrastructure.yaml and has no claim, so the absence of one is what
// tells them apart in every layout this platform has shipped.
func DeclaredWorkloadClusters(gitopsDir, mgmtCluster string) ([]string, error) {
	root := filepath.Join(gitopsDir, RegistryDir, "clusters")
	entries, err := os.ReadDir(root)
	if err != nil {
		// A repository with no registry declares no cluster. That is not an error
		// here: --gitops-dir is optional on a recovery teardown, and reporting a
		// missing directory as a failure would make the absence of a declaration
		// look like a reason not to continue.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	var clusters []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == mgmtCluster {
			continue
		}
		claims, err := filepath.Glob(filepath.Join(root, entry.Name(), "infrastructure", "spokepool*.yaml"))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filepath.Join(root, entry.Name()), err)
		}
		if len(claims) == 0 {
			continue
		}
		clusters = append(clusters, entry.Name())
	}
	sort.Strings(clusters)
	return clusters, nil
}
