package bootstrap

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// recordInfrastructureName writes the management cluster's HetznerCluster name
// into the box's own record (.state/<cluster>.json, metadata.infrastructureName).
//
// CAPI generates it (<cluster>-<suffix>) and CAPH labels every resource with it,
// so it is what teardown must match. Its only other copy is inside the cluster,
// which is exactly what is gone when a teardown has to clean up after it.
// Recorded once, after pivot, when the management cluster holds its own CAPI
// objects; a resumed box past that phase gets it on its next run.
func (o *Orchestrator) recordInfrastructureName(ctx context.Context, stateMgr *state.StateManager,
	bs *state.BootstrapState, kubeconfig string) error {
	if bs.Metadata[state.MetadataInfrastructureName] != "" {
		return nil
	}
	kctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(kctx, "kubectl", "--kubeconfig", kubeconfig, "get",
		"clusters.cluster.x-k8s.io", "-A", "--field-selector", "metadata.name="+o.ClusterName,
		"-o", "jsonpath={.items[0].spec.infrastructureRef.kind}/{.items[0].spec.infrastructureRef.name}").Output()
	if err != nil {
		return fmt.Errorf("read %s's infrastructure name for the box record: %w", o.ClusterName, err)
	}
	kind, name, _ := strings.Cut(strings.TrimSpace(string(out)), "/")
	if kind != "HetznerCluster" || name == "" {
		return nil // not a Hetzner-provisioned cluster: nothing CAPH labels
	}
	if bs.Metadata == nil {
		bs.Metadata = map[string]string{}
	}
	bs.Metadata[state.MetadataInfrastructureName] = name
	fmt.Printf("[pivot-ready] recorded infrastructure name %s in the box's state\n", name)
	return stateMgr.Save(bs)
}
