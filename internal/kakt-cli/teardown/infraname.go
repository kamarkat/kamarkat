package teardown

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// ownershipKeys returns, for each logical cluster, the names CAPH may have put in
// its caph-cluster-<name> label: the cluster's own name and the name of the
// HetznerCluster its CAPI Cluster references (spec.infrastructureRef). A cluster
// built from a ClusterClass gets a generated HetznerCluster name
// (nutgraf-mgmt-1794929668-n9zg4), and CAPH labels resources with THAT name.
//
// Still exact keys, never a prefix or substring: the generated name is read from
// the cluster that owns it, so another cluster's resources cannot match. When
// the API cannot be read, only the logical names are used and the operator is
// told -- resources the provider then reports as owned by an unlisted name are
// excluded, as before.
func (o *Orchestrator) ownershipKeys(_ context.Context, clusters []string) []string {
	keys := append([]string(nil), clusters...)
	for _, c := range clusters {
		if n := o.infraName[c]; n != "" && n != c {
			keys = append(keys, n)
		}
	}
	return keys
}

// recordedInfraName reads the name Day-0 recorded in the box's own state,
// .state/<cluster>.json metadata.infrastructureName. A missing record is an
// error. The value can only be THIS cluster's generated name,
// <cluster>-<letters and digits>, so a wrong record cannot name another
// cluster's resources.
func (o *Orchestrator) recordedInfraName() (string, error) {
	path := filepath.Join(o.GitopsDir, state.TenantStateDir, o.ClusterName+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the box's record %s: %w", path, err)
	}
	var bs state.BootstrapState
	if err := json.Unmarshal(raw, &bs); err != nil {
		return "", fmt.Errorf("the box's record %s is not readable: %w", path, err)
	}
	return validInfraName(o.ClusterName, bs.Metadata[state.MetadataInfrastructureName])
}

func validInfraName(cluster, recorded string) (string, error) {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return "", nil
	}
	suffix, ok := strings.CutPrefix(recorded, cluster+"-")
	if !ok || suffix == "" || strings.ContainsFunc(suffix, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		return "", fmt.Errorf("the box's record names infrastructure %q, which is not %s's generated name "+
			"(%s-<letters and digits>)", recorded, cluster, cluster)
	}
	return recorded, nil
}

// resolveInfraNames resolves each cluster's HetznerCluster name once, before
// anything is deleted. The cluster's own answer wins; the box's record stands in
// when the API cannot answer and must agree with it when it can. With neither,
// teardown stops and deletes nothing.
func (o *Orchestrator) resolveInfraNames(ctx context.Context, clusters []string) error {
	o.infraName = map[string]string{}
	recorded, err := o.recordedInfraName()
	if err != nil {
		return err
	}
	resolved, rerr := o.readInfraNames(ctx, clusters)
	if rerr != nil {
		if recorded == "" {
			return fmt.Errorf("the cluster's API cannot be read (%v) and the box's record holds no "+
				"metadata.%s, so its resources cannot be attributed; nothing was deleted",
				rerr, state.MetadataInfrastructureName)
		}
		fmt.Printf("[teardown] the cluster's API cannot be read (%v); using the infrastructure name the box recorded, %s\n", rerr, recorded)
		o.infraName[o.ClusterName] = recorded
		return nil
	}
	if have := resolved[o.ClusterName]; recorded != "" && have != "" && have != recorded {
		return fmt.Errorf("the box records infrastructure %s but the cluster says %s; nothing was deleted", recorded, have)
	}
	for c, n := range resolved {
		if n != c {
			fmt.Printf("[teardown] %s's infrastructure is HetznerCluster %s (its CAPI Cluster's infrastructureRef)\n", c, n)
		}
		o.infraName[c] = n
	}
	if o.infraName[o.ClusterName] == "" && recorded != "" {
		o.infraName[o.ClusterName] = recorded
	}
	return nil
}

func (o *Orchestrator) readInfraNames(ctx context.Context, clusters []string) (map[string]string, error) {
	baseArgs, ok := o.resolveKubectlBaseArgs()
	if !ok {
		return nil, fmt.Errorf("no kubeconfig")
	}
	kctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(kctx, "kubectl", append(append([]string(nil), baseArgs...),
		"get", "clusters.cluster.x-k8s.io", "-A", "-o", "json")...).Output()
	if err != nil {
		return nil, err
	}
	return infraNames(out, clusters)
}

// infraNames maps each wanted CAPI Cluster name to its Hetzner infrastructureRef.
func infraNames(clusterListJSON []byte, wanted []string) (map[string]string, error) {
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				InfrastructureRef struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"infrastructureRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(clusterListJSON, &l); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, w := range wanted {
		want[strings.TrimSpace(w)] = true
	}
	out := map[string]string{}
	for _, c := range l.Items {
		ref := c.Spec.InfrastructureRef
		if want[c.Metadata.Name] && ref.Kind == "HetznerCluster" && ref.Name != "" {
			out[c.Metadata.Name] = ref.Name
		}
	}
	return out, nil
}

// remainingOwned re-reads the provider after the sweeps: every resource still
// owned by this run's clusters, under any of their ownership keys.
func (o *Orchestrator) remainingOwned(ctx context.Context) ([]Attribution, error) {
	token := strings.TrimSpace(o.ProviderToken)
	if token == "" {
		return nil, fmt.Errorf("no provider token to confirm the infrastructure is gone")
	}
	inv, err := ProviderInventory(ctx, token, o.ownershipKeys(ctx, append([]string{o.ClusterName}, o.Scope...)))
	if err != nil {
		return nil, fmt.Errorf("re-read the provider to confirm the infrastructure is gone: %w", err)
	}
	return inv.Eligible(), nil
}
