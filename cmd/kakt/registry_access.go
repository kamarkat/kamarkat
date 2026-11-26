package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/browser"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/hubaccess"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/tenant"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// registryTokenPage opens GitHub's token form with read:packages, the only
// scope pulling a package needs.
const registryTokenPage = "https://github.com/settings/tokens/new?scopes=read:packages&description=kamarkat%20package%20read"

// terminalRegistryIO asks its questions in the terminal and keeps the box's
// registry credential in the box's escrow, under the operator's own session.
// The escrow is opened only when a private package needs it.
type terminalRegistryIO struct {
	gitopsDir string
	access    hubaccess.Access
	box       string
	in        *bufio.Reader
}

func newTerminalRegistryIO(gitopsDir string) *terminalRegistryIO {
	return &terminalRegistryIO{gitopsDir: gitopsDir, in: bufio.NewReader(os.Stdin)}
}

func interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func (t *terminalRegistryIO) AskVisibility(kind string, _ []tenant.Package) (tenant.Visibility, error) {
	if !interactive() {
		return "", fmt.Errorf("whether this application's %s are public or private cannot be "+
			"established from the registry yet; pass --%s public or --%s private", kind, kind, kind)
	}
	fmt.Printf("\nHow will your application %s be published?\n"+
		"  1) Public  — anyone can pull them\n"+
		"  2) Private — registry authentication is required\n"+
		"Choose 1 or 2: ", kind)
	for {
		line, err := t.in.ReadString('\n')
		if err != nil {
			return "", err
		}
		switch strings.TrimSpace(line) {
		case "1", "public", "Public":
			return tenant.Public, nil
		case "2", "private", "Private":
			return tenant.Private, nil
		}
		fmt.Print("Choose 1 (public) or 2 (private): ")
	}
}

func (t *terminalRegistryIO) AskPublished(p tenant.Package) (bool, error) {
	if !interactive() {
		return true, nil
	}
	fmt.Printf("\nThe registry refused %s. Has it been published yet? [y/N]: ", p)
	line, err := t.in.ReadString('\n')
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y"), nil
}

func (t *terminalRegistryIO) open(ctx context.Context) error {
	if t.access != nil {
		return nil
	}
	coords, err := hubaccess.Declaration(t.gitopsDir, "")
	if err != nil {
		return fmt.Errorf("reach this box's escrow for its registry credential: %w", err)
	}
	access, err := hubaccess.Open(ctx, coords)
	if err != nil {
		return err
	}
	t.access, t.box = access, coords.BoxID
	return nil
}

// boxScope is where box-scoped credentials live: the box, under itself, the same
// scope `kakt escrow enroll-registry-credential` writes.
func (t *terminalRegistryIO) boxScope() hubaccess.Identity {
	return hubaccess.Identity{BoxID: t.box, ClusterName: t.box}
}

func (t *terminalRegistryIO) LoadCredential(ctx context.Context) (*tenant.RegistryCredential, error) {
	if err := t.open(ctx); err != nil {
		return nil, err
	}
	payload, err := t.access.Artifact(ctx, t.boxScope(), escrow.ArtifactRegistryCredential)
	if err != nil {
		return nil, err
	}
	f := escrow.ParseFields(payload)
	if f[escrow.FieldRegistryToken] == "" {
		return nil, nil
	}
	return &tenant.RegistryCredential{Username: f[escrow.FieldRegistryUser], Token: f[escrow.FieldRegistryToken]}, nil
}

func (t *terminalRegistryIO) RequestCredential(reason string) (*tenant.RegistryCredential, error) {
	if !interactive() {
		return nil, fmt.Errorf("%s\n\nNo terminal to ask in. Enrol one, then run this again:\n"+
			"  printf 'username=<user>\\ntoken=<token>' | kakt escrow enroll-registry-credential --box %s --rotate",
			reason, t.box)
	}
	fmt.Printf("\nRegistry access required\n\n%s\n\n1. Create a GitHub access token with read:packages\n   %s\n", reason, registryTokenPage)
	_ = browser.Open(registryTokenPage)
	fmt.Print("2. Enter your GitHub username: ")
	user, err := t.in.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fmt.Print("3. Enter your access token (not echoed): ")
	tok, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return nil, err
	}
	return &tenant.RegistryCredential{Username: strings.TrimSpace(user), Token: strings.TrimSpace(string(tok))}, nil
}

func (t *terminalRegistryIO) SaveCredential(ctx context.Context, c *tenant.RegistryCredential) error {
	if err := t.open(ctx); err != nil {
		return err
	}
	payload := escrow.FieldRegistryUser + "=" + c.Username + "\n" + escrow.FieldRegistryToken + "=" + c.Token
	return t.access.PutArtifact(ctx, t.boxScope(), escrow.ArtifactRegistryCredential, payload)
}

func (t *terminalRegistryIO) Say(format string, args ...any) { fmt.Printf(format+"\n", args...) }
