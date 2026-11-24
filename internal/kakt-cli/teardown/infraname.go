package teardown

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

// resolveInfraNames reads the CAPI Clusters once, before anything is deleted.
func (o *Orchestrator) resolveInfraNames(ctx context.Context, clusters []string) {
	o.infraName = map[string]string{}
	baseArgs, ok := o.resolveKubectlBaseArgs()
	if !ok {
		return
	}
	kctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(kctx, "kubectl", append(append([]string(nil), baseArgs...),
		"get", "clusters.cluster.x-k8s.io", "-A", "-o", "json")...).Output()
	if err != nil {
		fmt.Printf("[teardown] could not read CAPI Clusters to resolve infrastructure names (%v); "+
			"matching on cluster names only\n", err)
		return
	}
	resolved, err := infraNames(out, clusters)
	if err != nil {
		fmt.Printf("[teardown] could not parse CAPI Clusters (%v); matching on cluster names only\n", err)
		return
	}
	for c, n := range resolved {
		if n != c {
			fmt.Printf("[teardown] %s's infrastructure is HetznerCluster %s (its CAPI Cluster's infrastructureRef)\n", c, n)
		}
		o.infraName[c] = n
	}
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
