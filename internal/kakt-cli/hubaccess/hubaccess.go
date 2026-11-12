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
	// EscrowOrgID is the Infisical organization the escrow project belongs to.
	EscrowOrgID string
	// PlatformAdmin is Infisical's own id for the one human this box accepts.
	//
	// An id, never an address: an address can be changed and re-assigned, and a
	// box that named its administrator by email would hand that authority to
	// whoever next holds it.
	PlatformAdmin string
}

// registry/clusters/<cluster>/values.yaml is where a box declares itself.
func declarationPath(gitopsDir, clusterName string) string {
	return filepath.Join(gitopsDir, "registry", "clusters", clusterName, "values.yaml")
}

// readDeclaration parses the box's declaration without judging what is missing;
// the callers decide which fields they require.
func readDeclaration(gitopsDir, clusterName string) (Coordinates, error) {
	var c Coordinates
	if strings.TrimSpace(gitopsDir) == "" {
		return c, fmt.Errorf("no gitops directory: a box's declarations live in " +
			"registry/clusters/<cluster>/values.yaml, and there is nowhere to read them")
	}
	if strings.TrimSpace(clusterName) == "" {
		return c, fmt.Errorf("no cluster name: these declarations belong to one cluster")
	}
	path := declarationPath(gitopsDir, clusterName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read the box's declarations at %s: %w", path, err)
	}
	var decl struct {
		BoxID       string `yaml:"boxId"`
		ClusterName string `yaml:"clusterName"`
		Escrow      struct {
			URL            string `yaml:"url"`
			ProjectID      string `yaml:"projectId"`
			OrganizationID string `yaml:"organizationId"`
		} `yaml:"escrow"`
		PlatformAdminIdentity string `yaml:"platformAdminIdentity"`
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
	c.EscrowOrgID = strings.TrimSpace(decl.Escrow.OrganizationID)
	c.PlatformAdmin = strings.TrimSpace(decl.PlatformAdminIdentity)

	return c, nil
}

// Declaration reads a box's coordinates from the one file that holds them,
// registry/clusters/<cluster>/values.yaml.
//
// The canonical declaration, read the same way the chart and Day-0 read their own
// inputs from it. Nothing here is inferred from a cluster name or from the
// directory layout: a box id that was guessed would address another box's
// artefacts, and the escrow would answer correctly for the wrong hub.
func Declaration(gitopsDir, clusterName string) (Coordinates, error) {
	c, err := readDeclaration(gitopsDir, clusterName)
	if err != nil {
		return c, err
	}
	path := declarationPath(gitopsDir, clusterName)
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
			"the escrow store of whoever ran it.", path)
	}
	return c, nil
}

// DeclaredBoxID is the box id alone, for a caller that needs the identity and not
// the escrow.
//
// Separate from Declaration because the two have different preconditions: the box
// id is declared from the moment a repository is scaffolded, while the escrow
// coordinates may be filled in later, and a bootstrap must not fail for want of
// coordinates it is not about to use.
func DeclaredBoxID(gitopsDir, clusterName string) (string, error) {
	c, err := readDeclaration(gitopsDir, clusterName)
	if err != nil {
		return "", err
	}
	if c.BoxID == "" {
		return "", fmt.Errorf("%s declares no boxId: artefacts are escrowed under the logical "+
			"box rather than the cluster (ADR-111 §1), so it cannot be inferred from the "+
			"cluster name", declarationPath(gitopsDir, clusterName))
	}
	return c.BoxID, nil
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

	// Artifact reads one of the box's escrowed artefacts, returning "" when the
	// escrow holds none. PutArtifact writes one.
	//
	// The pair exists for enrolment, which has to tell ABSENT from UNREADABLE: a
	// write that treats an unreadable escrow as empty is how the copy a teardown
	// depends on gets overwritten. Readers that only need the value use the named
	// methods above.
	Artifact(ctx context.Context, id Identity, name string) (string, error)
	PutArtifact(ctx context.Context, id Identity, name, payload string) error
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
// Two credentials are accepted, and no others. Which one is used is a DECISION,
// never a coincidence of what happens to be exported:
//
//  1. The human session from `kakt login`, whenever there is one. It is tried
//     first, because an operator sitting at a terminal that also carries
//     automation credentials must act as themselves -- a machine identity that
//     silently won would attribute their work to the pipeline and skip the
//     administrator check below.
//  2. The machine identity in INFISICAL_ESCROW_*, used when there is no session,
//     or when KAKT_IDENTITY=machine says so. CI has no browser and must not hold
//     a developer's session; `hub-operator` is the same case inside the box.
//
// There is no third. A local kubeconfig, a path out of .state, a kind context:
// each of those is a guess at access rather than access, and each failed open.
// An operator who means to use a specific file passes --kubeconfig, which is a
// statement rather than a search.
//
// WHAT THIS ENFORCES, AND WHAT IT DOES NOT. The administrator check below is a
// guard rail: it stops this CLI acting as an identity the box has not declared,
// which is what makes a mistaken session a refusal instead of a surprise. It is
// not a security boundary. The boundary is membership of the escrow project, held
// by Infisical and enforced by its server -- another member of that project, or
// an organization administrator, can read these artefacts through Infisical's own
// API whatever this file says.
func Open(ctx context.Context, c Coordinates) (Access, error) {
	a, err := OpenUnbound(ctx, c)
	if err != nil {
		return nil, err
	}
	return verifiedAccess(ctx, a, c)
}

// OpenUnbound authenticates without checking the box's escrow binding.
//
// For `kakt escrow bind-destination` alone, which is the command that establishes
// that binding. Every other caller uses Open, so a declaration that disagrees
// with its own escrow is refused before anything is read or written.
func OpenUnbound(ctx context.Context, c Coordinates) (Access, error) {
	machineID := strings.TrimSpace(os.Getenv("INFISICAL_ESCROW_CLIENT_ID"))
	selected := strings.ToLower(strings.TrimSpace(os.Getenv("KAKT_IDENTITY")))

	switch selected {
	case "machine":
		if machineID == "" {
			return nil, fmt.Errorf("KAKT_IDENTITY=machine selects the machine identity, and " +
				"INFISICAL_ESCROW_CLIENT_ID is not set. Automation authenticates with its own " +
				"identity; it never borrows a human session")
		}
	case "human", "":
	default:
		return nil, fmt.Errorf("KAKT_IDENTITY=%q is not an identity: pass human or machine", selected)
	}

	// The human session first, unless the machine identity was asked for.
	if selected != "machine" {
		s, err := session.Current(ctx, c.EscrowURL)
		switch {
		case err == nil:
			if err := authorizeAdministrator(c, s); err != nil {
				return nil, err
			}
			store, openErr := escrow.NewEscrowClientForToken(ctx, c.EscrowURL, c.EscrowProjectID, s.Token)
			if openErr != nil {
				return nil, fmt.Errorf("%s could not open the escrow for this box: %w\n\n"+
					"If the session is good, this is authorization: ask a platform administrator "+
					"for access to escrow project %s at %s",
					s.Email, openErr, c.EscrowProjectID, c.EscrowURL)
			}
			return &escrowAccess{store: store, who: s.Email}, nil
		case machineID == "":
			// No session and no machine identity: report the human case, which is
			// the one an operator can act on.
			return nil, err
		}
	}

	store, err := escrow.NewEscrowClient(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("the escrow machine identity in the environment did not "+
			"authenticate: %w", err)
	}
	return &escrowAccess{
		store: store,
		who:   "the machine identity in INFISICAL_ESCROW_CLIENT_ID",
	}, nil
}

// authorizeAdministrator refuses a human session the box has not declared.
//
// Checked on the id and the organization, both stated by the server at login and
// neither chosen by the caller. A flag or an address would make this a claim
// rather than a check.
func authorizeAdministrator(c Coordinates, s session.Session) error {
	userID, email := strings.TrimSpace(s.UserID), s.Email
	if c.PlatformAdmin == "" {
		return fmt.Errorf(`this box declares no platform administrator, so no human session is authorized for it.

Box: %s

One configured human operates a box's escrow. Declare them in
registry/clusters/<cluster>/values.yaml, using Infisical's own id for the
account rather than its address:

  platformAdminIdentity: <infisical user id>

`+"`kakt whoami`"+` prints the id of the session you are holding.`, c.BoxID)
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("this session carries no user id, so it cannot be checked against " +
			"the box's declared administrator. Run `kakt login` again to obtain one")
	}
	if userID != c.PlatformAdmin {
		return fmt.Errorf(`%s is not the platform administrator this box declares.

Box: %s
Declared: %s
This session: %s

A box accepts one configured human. If the administrator has changed, that is an
authorized rebind rather than an edit: the new identity has to hold membership of
this box's escrow project as well, which Infisical enforces and this check does
not.`, email, c.BoxID, c.PlatformAdmin, userID)
	}
	if c.EscrowOrgID != "" && strings.TrimSpace(s.OrgID) != c.EscrowOrgID {
		return fmt.Errorf("this session belongs to a different Infisical organization than "+
			"box %s declares (%s). The administrator id is only meaningful inside the "+
			"organization that issued it", c.BoxID, c.EscrowOrgID)
	}
	return nil
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

func (a *escrowAccess) Artifact(ctx context.Context, id Identity, name string) (string, error) {
	payload, err := a.store.RestoreArtifact(ctx, escrow.ForCluster(id.BoxID, id.ClusterName), name)
	if err != nil {
		return "", fmt.Errorf("%s could not read %s for box %s: %w", a.who, name, id.BoxID, err)
	}
	return payload, nil
}

func (a *escrowAccess) PutArtifact(ctx context.Context, id Identity, name, payload string) error {
	if err := a.store.BackupArtifact(ctx, escrow.ForCluster(id.BoxID, id.ClusterName), name, payload); err != nil {
		return fmt.Errorf("%s could not write %s for box %s: %w", a.who, name, id.BoxID, err)
	}
	return nil
}

func (a *escrowAccess) Provider(ctx context.Context, id Identity, provider string) (string, error) {
	artifact := escrow.ProviderCredentialArtifact(provider)
	// Read from the BOX's identity, which is where it is written: the credential
	// buys and destroys the box's infrastructure and outlives any cluster in it
	// (ADR-111 §1), so a successor hub finds the one already escrowed rather than
	// looking under its own new cluster name and finding nothing.
	payload, err := a.Artifact(ctx, Identity{BoxID: id.BoxID, ClusterName: id.BoxID}, artifact)
	if err != nil {
		return "", err
	}
	if payload == "" {
		return "", fmt.Errorf("the escrow holds no %s credential for box %s.\n\n"+
			"hub-operator escrows the box's provider credential on reconcile; a box that has "+
			"not reconciled since this was introduced has none yet. Set the provider's token "+
			"in the environment for this run", provider, id.BoxID)
	}
	return strings.TrimSpace(payload), nil
}

// verifiedAccess refuses an escrow that does not agree it is this box's escrow.
//
// WHAT THIS IS NOT. It is not an independent pin on the destination, and an
// earlier version of this comment claimed that it was. Both halves of the
// comparison come from the project the repository names: the expected value from
// the declaration, the recorded one from the escrow that declaration points at.
// Anyone who can edit the declaration AND write to the project they redirect it
// to can place a matching record there, and this check passes.
//
// WHAT IT ACTUALLY ESTABLISHES, which is still worth having:
//
//   - a destination nobody established is refused. An escrow project with no
//     binding for this box is not treated as this box's escrow, so a redirect to
//     an unprepared project -- a typo, a copied value, a project from another
//     box -- fails closed instead of writing recovery material into it.
//   - drift is refused. A declaration edited without a deliberate rebind no
//     longer matches the binding its own escrow holds, so the half-done change
//     is a refusal rather than a silent move.
//   - a rebind is explicit and leaves a record. The value changes, --rebind is
//     required, and the before and after are both printed.
//
// THE REAL ANCHORS for a privileged write are elsewhere and are not this file's
// to enforce: the acting identity's membership of the project (Infisical decides,
// and a redirect only works at a project that identity may write), and governance
// on the declaration itself -- review and branch protection on the repository
// that carries it. Those are stated in ADR-076 rather than implied here.
func verifiedAccess(ctx context.Context, a Access, c Coordinates) (Access, error) {
	id := Identity{BoxID: c.BoxID, ClusterName: c.BoxID}
	recorded, err := a.Artifact(ctx, id, escrow.ArtifactEscrowDestination)
	if err != nil {
		// Unreadable is not absent. Continuing here would treat an escrow that
		// cannot be questioned as one that agreed.
		return nil, fmt.Errorf("could not read the escrow binding for box %s: %w", c.BoxID, err)
	}
	want := DestinationRecord(c)
	switch {
	case recorded == "":
		return nil, fmt.Errorf(`escrow project %s at %s holds no binding for box %s, so it is not established as this box's escrow.

A box's escrow coordinates are committed to its repository, so an authorized
enrolment records them in the escrow as well: a project that was never bound is not
treated as this box's escrow, and a declaration edited without a deliberate rebind
no longer matches what its own escrow holds.

Bind it, as the platform administrator:

  kakt escrow bind-destination --gitops-dir . --cluster %s`,
			c.EscrowProjectID, c.EscrowURL, c.BoxID, c.ClusterName)
	case recorded != want:
		return nil, mismatchedBinding(c, recorded, want)
	}
	return a, nil
}

// OpenEstablishing opens the escrow for an AUTHORIZED ENROLMENT, and records the
// box's binding if it has none.
//
// Enrolment is where a box's escrow is established: it is the first privileged
// write, it is explicit, and it is done by an operator who knows which project is
// this box's. So a first enrolment into an unbound project binds it, in the same
// act -- rather than failing closed on a binding that nothing has yet had the
// chance to create.
//
// A binding that EXISTS and disagrees is still refused. Moving a bound box is a
// rebind, which stays a separate and deliberate operation.
func OpenEstablishing(ctx context.Context, c Coordinates) (Access, error) {
	a, err := OpenUnbound(ctx, c)
	if err != nil {
		return nil, err
	}
	id := Identity{BoxID: c.BoxID, ClusterName: c.BoxID}
	recorded, err := a.Artifact(ctx, id, escrow.ArtifactEscrowDestination)
	if err != nil {
		return nil, fmt.Errorf("could not read the escrow binding for box %s: %w", c.BoxID, err)
	}
	want := DestinationRecord(c)
	switch {
	case recorded == "":
		if err := a.PutArtifact(ctx, id, escrow.ArtifactEscrowDestination, want); err != nil {
			return nil, fmt.Errorf("could not record the escrow binding for box %s: %w", c.BoxID, err)
		}
		fmt.Printf("[escrow] box %s bound to %s\n", c.BoxID, want)
	case recorded != want:
		return nil, mismatchedBinding(c, recorded, want)
	}
	return a, nil
}

// mismatchedBinding is the one refusal for a declaration that disagrees with the
// binding its own escrow holds.
func mismatchedBinding(c Coordinates, recorded, want string) error {
	return fmt.Errorf(`box %s is bound to a different escrow than its repository now declares.

Recorded in the escrow: %s
Declared in the repository: %s

An edit alone does not move a box's recovery material. If the move is intended,
rebind it deliberately:

  kakt escrow bind-destination --gitops-dir . --cluster %s --rebind`,
		c.BoxID, recorded, want, c.ClusterName)
}

// DestinationRecord is the binding's payload: the coordinates it pins, in one
// line, so a mismatch names both halves.
func DestinationRecord(c Coordinates) string {
	return strings.TrimRight(c.EscrowURL, "/") + " " + c.EscrowProjectID
}

// ProviderToken is the one way an infrastructure command obtains a cloud
// credential.
//
// THE BOX'S OWN COPY, ALWAYS. The credential buys and destroys this box's
// infrastructure, and the question every caller is really asking -- what does
// this box own, what should be deleted, where does this cluster live -- can only
// be answered by the credential the box was built with. A token that happened to
// be exported in a shell authenticates perfectly and answers about a different
// provider project: an inventory that comes back empty, a duplicate management
// cluster admitted because the first one was invisible, a teardown that deletes
// nothing and reports success.
//
// So there is no environment variable, no file beside the repository and no
// flag. Those were three ways for the wrong answer to look like the right one.
// Enrolment is the one place a credential is accepted from an operator, and it
// is explicit, piped and separate (`kakt escrow enroll-provider-credential`).
//
// FAIL CLOSED. An escrow that cannot be reached, or holds no credential, is an
// error naming the enrolment command -- never a silent search elsewhere.
func ProviderToken(ctx context.Context, gitopsDir, clusterName, provider string) (string, error) {
	coords, err := Declaration(gitopsDir, clusterName)
	if err != nil {
		return "", err
	}
	access, err := Open(ctx, coords)
	if err != nil {
		return "", err
	}
	// The box on both halves: the credential is the box's, so every incarnation
	// finds the same one (ADR-111 ss1).
	return access.Provider(ctx, Identity{BoxID: coords.BoxID, ClusterName: coords.BoxID}, provider)
}
