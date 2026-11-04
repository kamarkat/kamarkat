package tenant

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AddWorkloadCluster hydrates templates/workload-cluster into a tenant repository
// that already holds a management cluster.
//
// One management cluster per repository, as many workload clusters as it
// declares -- the layout kubefirst uses, where registry/clusters/<NAME>/ exists
// once per cluster and the management cluster's ArgoCD reaches into each.
//
// The name is the tenant's to choose and is written down, never computed. It
// was a literal in the published bundle (spoke-pool-eu-<env>-01, packaged as
// platform-spoke-pools-<env>-<provider>), so every box that pulled the bundle
// provisioned a workload cluster under the same name and every identity derived
// from it collided across boxes -- most damagingly the CNPG archive prefix,
// where s3://spoke-pool-backups/<name>/ held a previous box's WALs and barman
// refused the next box's with "Expected empty archive", so no backup ever
// completed on any box.
//
// It cannot be fixed by templating the packaged chart: templated-fields refuses
// to rewrite metadata.name, because renaming an object by value makes the
// resource ArgoCD prunes depend on that value -- and for a SpokePool, "prune and
// recreate" is destroying a cluster.
//
// The name is what an application names to run here.
// validate-cluster-name-contract.sh records that the SpokePool composition
// derives metadata.labels.cluster-name from the claim name, so naming the claim
// names the cluster, and the fleet ApplicationSets select on it.
type WorkloadCluster struct {
	// GitopsDir is the tenant repository, holding one management cluster.
	GitopsDir string
	// MgmtCluster is that management cluster's name: it owns the Application
	// that creates this one.
	MgmtCluster string
	// Name is the workload cluster, and what applications target.
	Name string
	// Provider, Region, Environment describe where it runs. Same field set as
	// kubefirst's WorkloadCluster{ClusterName, CloudProvider, CloudRegion,
	// Environment}.
	Provider    string
	Region      string
	Environment string
	// BundleVersion is the platform version it starts on.
	BundleVersion string
	// TenantID owns it (ADR-064).
	TenantID string
	// GitopsRepoURL is this repository, for the Applications that read it.
	GitopsRepoURL string
	// Workers is how many cloud workers it starts with.
	Workers int
	// PublicTLSIssuer is the ACME issuer its public certificates come from.
	PublicTLSIssuer string
	// ChartSource and PlatformRepoURL and BundleRegistry carry the same values
	// the management cluster was scaffolded with.
	ChartSource     string
	PlatformRepoURL string
	BundleRegistry  string
}

// dnsLabel is what a cluster name has to be: it becomes a Kubernetes namespace,
// a CAPI Cluster, an ArgoCD Application and a DNS-addressable cell id.
var dnsLabel = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// Add writes the workload cluster's directory and the Application that creates
// it, and returns the paths it wrote.
//
// Refuses rather than overwrites. A second cluster given an existing name would
// silently replace that cluster's declaration, and the first indication would be
// two CAPI Clusters contending for one set of resources.
func (w WorkloadCluster) Add() ([]string, error) {
	if !dnsLabel.MatchString(w.Name) {
		return nil, fmt.Errorf("cluster name %q is not a DNS label: it becomes a namespace, "+
			"a CAPI Cluster and this cell's id, so it must be lowercase alphanumeric "+
			"with hyphens, starting with a letter", w.Name)
	}
	if w.Name == w.MgmtCluster {
		return nil, fmt.Errorf("cluster name %q is the management cluster's: a repository "+
			"holds one management cluster and its workload clusters, and they cannot share "+
			"a name", w.Name)
	}

	// Capacity is refused at declaration, and what counts as capacity differs by
	// provider -- so this asks the right question of each rather than one
	// question of both.
	//
	// hetzner buys its workers: 0 means nothing will ever schedule, and there is
	// no second source. hybrid buys none at all (ADR-075) -- its capacity is the
	// home-workers list, which is '[]' in the template and is the operator's to
	// fill in, so a count here is not what makes it valid. cmd/kakt refuses
	// --workers on hybrid and prints what to edit instead.
	//
	// Getting this wrong is expensive because nothing downstream reports it.
	// nutgraf-01, 2026-09-18: the cluster provisioned, the node went Ready,
	// ArgoCD registered it and began syncing -- and its one node was a tainted
	// control plane. Thirteen pods Pending, sync waves never Healthy, the wave
	// carrying shared-cnpg never applied, and a bootstrap that failed an hour
	// later on "the spoke's shared-cnpg has no ready instance". Declaration is
	// the last point where the cause is still one field.
	if w.Provider != "hybrid" && w.Workers <= 0 {
		return nil, fmt.Errorf("cluster %q would have no capacity: --workers is %d and %s has "+
			"no on-premises nodes to fall back on, so nothing could ever schedule there.\n\n"+
			"Pass --workers >= 1", w.Name, w.Workers, w.Provider)
	}

	tmpl := filepath.Join(w.GitopsDir, "templates", "workload-cluster")
	if _, err := os.Stat(tmpl); err != nil {
		return nil, fmt.Errorf("no workload-cluster template at %s: this repository was "+
			"not scaffolded by a version that ships one", tmpl)
	}
	mgmtDir := filepath.Join(w.GitopsDir, RegistryDir, "clusters", w.MgmtCluster)
	if _, err := os.Stat(mgmtDir); err != nil {
		return nil, fmt.Errorf("no management cluster at %s: a workload cluster is created "+
			"by one, so it must exist first", mgmtDir)
	}

	dst := filepath.Join(w.GitopsDir, RegistryDir, "clusters", w.Name)
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("registry/clusters/%s already exists: refusing to overwrite a "+
			"declaration that may describe a running cluster", w.Name)
	}

	if err := copyDir(tmpl, dst); err != nil {
		return nil, fmt.Errorf("hydrate workload cluster: %w", err)
	}

	// The Application that creates it belongs to the MANAGEMENT cluster, whose
	// root Application applies registry/clusters/<mgmt>/ with recurse: false. Moved rather
	// than copied: left in place it would also be applied by the workload
	// cluster's own directory, and an Application that creates a cluster,
	// reconciled by that cluster, is a loop.
	appSrc := filepath.Join(dst, "infrastructure-app.yaml")
	appDst := filepath.Join(mgmtDir, w.Name+"-infrastructure.yaml")
	if err := os.Rename(appSrc, appDst); err != nil {
		return nil, fmt.Errorf("place the infrastructure Application: %w", err)
	}

	// One provider's claim, and the other removed -- the way kubefirst's
	// adjustDriver trims templates/workload-cluster/ to the provider in use. Both
	// left in place, ArgoCD would apply two SpokePools under one name.
	infra := filepath.Join(dst, "infrastructure")
	keep := filepath.Join(infra, "spokepool-"+w.Provider+".yaml")
	if _, err := os.Stat(keep); err != nil {
		return nil, fmt.Errorf("no claim template for provider %q: the template ships "+
			"hetzner and hybrid", w.Provider)
	}
	entries, err := os.ReadDir(infra)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", infra, err)
	}
	for _, e := range entries {
		p := filepath.Join(infra, e.Name())
		if p == keep {
			continue
		}
		if strings.HasPrefix(e.Name(), "spokepool-") {
			if err := os.Remove(p); err != nil {
				return nil, fmt.Errorf("remove unused claim %s: %w", e.Name(), err)
			}
		}
	}
	if err := os.Rename(keep, filepath.Join(infra, "spokepool.yaml")); err != nil {
		return nil, fmt.Errorf("name the claim: %w", err)
	}

	// Tokens, strictly: a leftover in a hydrated cluster is a cluster that names
	// something it does not have.
	if err := substitute(dst, w.tokens(), true); err != nil {
		return nil, err
	}
	if err := substituteFile(appDst, w.tokens()); err != nil {
		return nil, err
	}

	return []string{
		filepath.Join(RegistryDir, "clusters", w.Name),
		filepath.Join(RegistryDir, "clusters", w.MgmtCluster, w.Name+"-infrastructure.yaml"),
	}, nil
}

// tokens are the same set scaffolding substitutes into a cluster, so a workload
// cluster and the management cluster are hydrated from one vocabulary.
func (w WorkloadCluster) tokens() map[string]string {
	return map[string]string{
		"<CLUSTER_NAME>":      w.Name,
		"<ENVIRONMENT>":       w.Environment,
		"<CLOUD_PROVIDER>":    w.Provider,
		"<CLOUD_REGION>":      w.Region,
		"<BUNDLE_VERSION>":    w.BundleVersion,
		"<TENANT_ID>":         w.TenantID,
		"<GITOPS_REPO_URL>":   w.GitopsRepoURL,
		"<PLATFORM_REPO_URL>": w.PlatformRepoURL,
		"<BUNDLE_REGISTRY>":   w.BundleRegistry,
		"<CHART_SOURCE>":      w.ChartSource,
		"<PUBLIC_TLS_ISSUER>": w.PublicTLSIssuer,
		"<WORKER_COUNT>":      fmt.Sprintf("%d", w.Workers),
	}
}

// substituteFile applies tokens to one file, for the Application that lives
// outside the hydrated directory.
func substituteFile(path string, tokens map[string]string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	text := string(raw)
	for token, value := range tokens {
		text = strings.ReplaceAll(text, token, value)
	}
	if left := leftoverToken(text); left != "" {
		return fmt.Errorf("%s still carries %s after substitution", path, left)
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

var tokenPattern = regexp.MustCompile(`<[A-Z][A-Z0-9_]*>`)

func leftoverToken(text string) string {
	return tokenPattern.FindString(text)
}
