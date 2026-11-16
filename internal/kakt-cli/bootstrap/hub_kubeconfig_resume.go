package bootstrap

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/opstore"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/state"
)

// reconcileHubKubeconfig makes the hub kubeconfig this run's state record names
// true on THIS machine, by reading the escrow -- which is the only durable copy
// of it that exists.
//
// pivot-move writes the hub's admin kubeconfig into the operator's own store,
// $KAMARKAT_HOME or ~/.kamarkat (opstore.KubeconfigPath), and records the path.
// The path is right for the machine that wrote it and meaningless anywhere else,
// and the record is committed to the tenant's repository -- so a resumed run on
// a fresh runner reads a path to a file that was never on this disk. Every phase
// from clusterclass-deploy onwards talks to the hub through it.
//
// The escrow is where this is recovered from, and the only place: the hub's
// kubeconfig is cluster-admin, so it is not committed, not an artifact and not
// re-derivable -- after the pivot, the CAPI Secret that holds it lives inside
// the very cluster it is needed to reach.
//
// Not a fallback. The recorded path is used when it is there, and when it is not
// this reads the escrow and REPLACES the record with the path it just wrote, so
// what the next phase opens is a file that exists.
func (o *Orchestrator) reconcileHubKubeconfig(ctx context.Context, stateMgr *state.StateManager, bs *state.BootstrapState) error {
	recorded := strings.TrimSpace(bs.MgmtKubeconfig)
	if recorded == "" {
		// Nothing claimed. A run that has not pivoted has no hub to reach, and
		// runFresh's own check handles a pivot recorded without a path.
		return nil
	}
	if _, err := os.Stat(recorded); err == nil {
		return nil
	}

	if strings.TrimSpace(o.GitopsDir) == "" {
		return fmt.Errorf("the hub kubeconfig recorded for this bootstrap is not on this machine:\n"+
			"  %s\n\n"+
			"It is recovered from the box's escrow, and the box's own declaration is what\n"+
			"names that escrow -- so this needs --gitops-dir to point at the box's repository.",
			recorded)
	}

	// The box's declaration, never the cluster name: a successor hub reads its
	// artefacts under the box it shares with its predecessor (ADR-111 §1).
	coords, err := hubaccess.Declaration(o.GitopsDir, o.ClusterName)
	if err != nil {
		return fmt.Errorf("the hub kubeconfig recorded at %s is not on this machine, and the box's "+
			"declaration does not say where to recover it from: %w", recorded, err)
	}
	access, err := hubaccess.Open(ctx, coords)
	if err != nil {
		return fmt.Errorf("the hub kubeconfig recorded at %s is not on this machine, and this run "+
			"cannot open the escrow that holds it: %w", recorded, err)
	}
	payload, err := access.Kubeconfig(ctx,
		hubaccess.Identity{BoxID: coords.BoxID, ClusterName: coords.ClusterName})
	if err != nil {
		// Says the window out loud, because its name is the whole remedy.
		// hub-operator escrows this on reconcile, and it is not running until
		// boundary-01 -- so a run that pivoted and then stopped before that
		// boundary has no escrowed copy, and the only kubeconfig that ever
		// existed is on the machine that made it.
		return fmt.Errorf("%w\n\n"+
			"This bootstrap recorded %s and this machine does not have it. If the\n"+
			"previous attempt stopped between pivot-move and boundary-01, hub-operator\n"+
			"had not yet escrowed a kubeconfig, and the only copy is on the machine that\n"+
			"ran it -- resume there, or tear this cluster down and build it again.",
			err, recorded)
	}

	path, err := opstore.KubeconfigPath(o.ClusterName)
	if err != nil {
		return fmt.Errorf("no runtime path to restore %s's kubeconfig into: %w", o.ClusterName, err)
	}
	// 0600, like every other writer of this file: it is cluster-admin.
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	fmt.Printf("[recovery] The hub kubeconfig recorded at %s is not on this machine.\n", recorded)
	fmt.Printf("[recovery]   Restored from the box's escrow to %s.\n", path)
	bs.MgmtKubeconfig = path
	return stateMgr.Save(bs)
}
