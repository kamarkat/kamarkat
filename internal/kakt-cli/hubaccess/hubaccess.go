// Package hubaccess is how an external command reaches a hub.
//
// ONE WAY IN. Before this, every command answered the question for itself:
// teardown read a recorded path out of .state, then k8-secrets/kubeconfig/, then
// guessed a kind context; validators read KUBECONFIG; others took a flag. All of
// them were artifacts of the machine that ran bootstrap, so a hub could only be
// operated from that laptop -- and the guesses failed open, which is how a
// teardown came to delete Hetzner servers while skipping every step that needed
// the cluster, leaving DNS records no later box can reclaim.
//
// The model is the one kubefirst arrived at: hold a long-lived credential in a
// store, derive cluster access from it on demand, use it and discard it. Their
// API runs inside the management cluster and uses its Pod ServiceAccount
// (rest.InClusterConfig) for the hub, and Kubernetes Secrets in that cluster for
// everything else; its kubeconfig endpoint takes the PROVIDER credential as input
// and returns a config, rather than reading a file. Nothing reads an operator's
// filesystem.
//
// A CLI cannot use an in-cluster ServiceAccount, and during a teardown the hub
// cannot be its own credential store, so the store here is the external Infisical
// escrow: independent of the box, written by hub-operator on reconcile, and
// already the authority `kakt kubeconfig` reads. The developer's own login is the
// credential presented to it.
//
// IN-CLUSTER CODE DOES NOT USE THIS. A controller running on the hub has a
// ServiceAccount and must keep using rest.InClusterConfig(): it needs no escrow,
// and an operator that read its own cluster's credential out of an external store
// would be strictly worse than the identity Kubernetes already gave it.
package hubaccess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/session"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// Identity is the hub being reached: the logical box, and the cluster within it.
//
// Both, because they have different lifetimes (ADR-111 §1). Escrowed artefacts
// are keyed by the box, which survives its clusters; the kubeconfig is a
// cluster's, and a successor hub's is a different one under the same box.
type Identity struct {
	BoxID       string
	ClusterName string
}

// Coordinates are what the box declares about its escrow: names, not credentials.
//
// Committed to the tenant repository on purpose. A URL and a project id identify
// where a box's artefacts live and grant nothing; keeping them only in
// k8-secrets/infisical on the bootstrap machine is what made a second workstation
// impossible, because the developer could authenticate and still not know which
// Infisical or which project to ask.
type Coordinates struct {
	Identity
	EscrowURL       string
	EscrowProjectID string
}

// Declaration reads a box's coordinates from the one file that holds them,
// registry/clusters/<cluster>/values.yaml.
//
// The canonical declaration, read the same way the chart and Day-0 read their own
// inputs from it. Nothing here is inferred from a cluster name or from the
// directory layout: a box id that was guessed would address another box's
// artefacts, and the escrow would answer correctly for the wrong hub.
func Declaration(gitopsDir, clusterName string) (Coordinates, error) {
	var c Coordinates
	if strings.TrimSpace(gitopsDir) == "" {
		return c, fmt.Errorf("no gitops directory: a box's escrow coordinates are declared in " +
			"registry/clusters/<cluster>/values.yaml, and there is nowhere to read them")
	}
	if strings.TrimSpace(clusterName) == "" {
		return c, fmt.Errorf("no cluster name: these declarations belong to one cluster")
	}
	path := filepath.Join(gitopsDir, "registry", "clusters", clusterName, "values.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read the box's declarations at %s: %w", path, err)
	}
	var decl struct {
		BoxID       string `yaml:"boxId"`
		ClusterName string `yaml:"clusterName"`
		Escrow      struct {
			URL       string `yaml:"url"`
			ProjectID string `yaml:"projectId"`
		} `yaml:"escrow"`
	}
	if err := yaml.Unmarshal(raw, &decl); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}

	c.BoxID = strings.TrimSpace(decl.BoxID)
	c.ClusterName = strings.TrimSpace(decl.ClusterName)
	if c.ClusterName == "" {
		c.ClusterName = clusterName
	}
	c.EscrowURL = strings.TrimSpace(decl.Escrow.URL)
	c.EscrowProjectID = strings.TrimSpace(decl.Escrow.ProjectID)

	if c.BoxID == "" {
		return c, fmt.Errorf("%s declares no boxId: artefacts are escrowed under the logical "+
			"box rather than the cluster (ADR-111 §1), so it cannot be inferred from the "+
			"cluster name", path)
	}
	if c.EscrowURL == "" || c.EscrowProjectID == "" {
		return c, fmt.Errorf("%s declares no escrow coordinates.\n\n"+
			"Add them -- they are names, not credentials, and they are what lets any\n"+
			"authorized workstation reach this box:\n\n"+
			"  escrow:\n"+
			"    url: https://app.infisical.com\n"+
			"    projectId: <the escrow project this box was enrolled in>\n\n"+
			"`kakt escrow init` prints both when it creates the project, and they are in\n"+
			"k8-secrets/infisical/ on the machine that ran it.", path)
	}
	return c, nil
}

// Access reaches a hub on behalf of whoever is authenticated.
type Access interface {
	// Kubernetes returns a client configuration for the hub, and the path of the
	// kubeconfig it was built from for callers that shell out to kubectl.
	//
	// The path is a temporary file the caller must remove: holding cluster-admin
	// is not something to leave on a workstation, and the one place that knows
	// when the command is finished is the command.
	Kubernetes(ctx context.Context, id Identity) (cfg *rest.Config, kubeconfigPath string, cleanup func(), err error)

	// Kubeconfig returns the escrowed kubeconfig itself, for the caller that
	// hands it to a person rather than using it (`kakt kubeconfig`, break-glass).
	//
	// Separate from Kubernetes because the lifetimes differ: that one writes a
	// temporary file it expects to be deleted when the command ends, and this one
	// is deliberately handed over to be used and then removed by whoever asked.
	Kubeconfig(ctx context.Context, id Identity) (string, error)

	// Provider returns the cloud credential the box was built with.
	Provider(ctx context.Context, id Identity, provider string) (string, error)
}

// escrowAccess derives access from an escrow, however that escrow was
// authenticated.
type escrowAccess struct {
	store escrow.EscrowClient
	// who describes the authenticated identity, for errors that have to say
	// whose permissions were refused.
	who string
}

// Open authenticates and returns the one implementation.
//
// Two credentials are accepted, in this order, and no others:
//
//  1. The machine identity in the environment (INFISICAL_ESCROW_*). Automation
//     uses this -- CI has no browser and must not hold a developer's session --
//     and it is checked first so a workstation that has both behaves the way the
//     pipeline does.
//  2. The developer's own session, from `kakt login`.
//
// There is no third. A local kubeconfig, a path out of .state, a kind context:
// each of those is a guess at access rather than access, and each failed open.
// An operator who means to use a specific file passes --kubeconfig, which is a
// statement rather than a search.
func Open(ctx context.Context, c Coordinates) (Access, error) {
	if os.Getenv("INFISICAL_ESCROW_CLIENT_ID") != "" {
		store, err := escrow.NewEscrowClient(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("the escrow machine identity in the environment did not "+
				"authenticate: %w", err)
		}
		return &escrowAccess{store: store, who: "the machine identity in INFISICAL_ESCROW_CLIENT_ID"}, nil
	}

	s, err := session.Current(ctx, c.EscrowURL)
	if err != nil {
		return nil, err
	}
	store, err := escrow.NewEscrowClientForToken(ctx, c.EscrowURL, c.EscrowProjectID, s.Token)
	if err != nil {
		return nil, fmt.Errorf("%s could not open the escrow for this box: %w\n\n"+
			"If the session is good, this is authorization: ask a platform administrator "+
			"for access to escrow project %s at %s",
			s.Email, err, c.EscrowProjectID, c.EscrowURL)
	}
	return &escrowAccess{store: store, who: s.Email}, nil
}

// Kubeconfig reads the escrowed admin kubeconfig, as whoever is authenticated.
func (a *escrowAccess) Kubeconfig(ctx context.Context, id Identity) (string, error) {
	payload, err := a.store.RestoreArtifact(ctx, escrow.ForCluster(id.BoxID, id.ClusterName), escrow.ArtifactKubeconfig)
	if err != nil {
		return "", fmt.Errorf("%s could not read the escrowed kubeconfig for %s: %w",
			a.who, id.ClusterName, err)
	}
	if payload == "" {
		return "", fmt.Errorf("the escrow holds no kubeconfig for %s under box %s.\n\n"+
			"hub-operator escrows it on reconcile, so a cluster that never finished "+
			"bootstrapping has none. For that case pass --kubeconfig explicitly; there is no "+
			"credential to fall back to", id.ClusterName, id.BoxID)
	}
	return payload, nil
}

func (a *escrowAccess) Kubernetes(ctx context.Context, id Identity) (*rest.Config, string, func(), error) {
	payload, err := a.Kubeconfig(ctx, id)
	if err != nil {
		return nil, "", nil, err
	}

	// 0600, in the OS temp directory, removed by the caller. The alternative --
	// handing back bytes -- does not serve the commands that shell out to
	// kubectl, and every one of those would otherwise write its own temp file.
	f, err := os.CreateTemp("", "kakt-"+id.ClusterName+"-*.kubeconfig")
	if err != nil {
		return nil, "", nil, fmt.Errorf("write a temporary kubeconfig: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return nil, "", nil, fmt.Errorf("restrict %s: %w", path, err)
	}
	if _, err := f.WriteString(payload); err != nil {
		f.Close()
		cleanup()
		return nil, "", nil, fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("close %s: %w", path, err)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("the escrowed kubeconfig for %s is not usable: %w",
			id.ClusterName, err)
	}
	return cfg, path, cleanup, nil
}

func (a *escrowAccess) Provider(ctx context.Context, id Identity, provider string) (string, error) {
	artifact := escrow.ProviderCredentialArtifact(provider)
	payload, err := a.store.RestoreArtifact(ctx, escrow.ForCluster(id.BoxID, id.ClusterName), artifact)
	if err != nil {
		return "", fmt.Errorf("%s could not read the escrowed %s credential for %s: %w",
			a.who, provider, id.ClusterName, err)
	}
	if payload == "" {
		return "", fmt.Errorf("the escrow holds no %s credential for box %s.\n\n"+
			"hub-operator escrows the box's provider credential on reconcile; a box that has "+
			"not reconciled since this was introduced has none yet. Set the provider's token "+
			"in the environment for this run", provider, id.BoxID)
	}
	return strings.TrimSpace(payload), nil
}
