// Package session holds the developer's own Infisical login, in the operating
// system's credential store.
//
// WHY A SESSION AT ALL. Every external command that reaches a hub needs a
// credential for the box's escrow, and until now that credential was a machine
// identity: four INFISICAL_ESCROW_* values, exported into the environment or left
// in k8-secrets/infisical/ by whoever ran `kakt escrow init`. That makes a shared
// secret the price of admission -- it has to be copied onto each workstation, it
// authenticates every person as the same identity, it cannot be revoked for one
// of them, and the audit log records the box rather than the operator.
//
// A session replaces the copying with an authentication. The developer logs in as
// themselves, the token is theirs, their permissions decide what they may read,
// and Infisical attributes the read to them.
//
// WHY THE OS CREDENTIAL STORE, AND NOT A DOTFILE. A file under $HOME is readable
// by everything the user runs, survives in backups and sync folders, and has no
// revocation story. The platform already refuses that shape for cluster
// credentials (ADR-076); a token that reads them is not held to a lower standard.
// There is deliberately NO file fallback: a machine with no credential store is
// told so, and automation uses a machine identity instead of a stored session,
// which is what CI should be doing anyway.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// service is the account name the credential store files these under. One entry
// per Infisical domain, so a developer working against two instances -- a
// company's own and app.infisical.com -- keeps one session each rather than
// alternating.
const service = "kakt"

// Session is what a login produces.
//
// The token is an Infisical USER session, which is what the escrow's secret
// routes accept beside a machine identity's. Email and organization are carried
// because `kakt whoami` has to be able to answer "as whom" without a round trip,
// and because a token alone cannot be read back for either.
type Session struct {
	Domain    string    `json:"domain"`
	Token     string    `json:"token"`
	Email     string    `json:"email"`
	OrgID     string    `json:"orgId"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// Expired reports whether the session is past its expiry.
//
// An unknown expiry is NOT treated as expired. Infisical does not always state
// one, and refusing a token the server would have accepted sends a developer
// through a browser login for nothing; the server's 401 is the authority, and
// Check turns that into the actionable error.
func (s Session) Expired() bool {
	return !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt)
}

// Store is the credential store this machine has.
type Store interface {
	Save(Session) error
	Load(domain string) (Session, error)
	Delete(domain string) error
	// Name is what the store is called, for messages that have to tell an
	// operator where their session went.
	Name() string
}

// ErrNoSession is returned when nothing is stored for a domain. Callers turn it
// into "run kakt login", so it is a sentinel rather than a string.
type ErrNoSession struct{ Domain string }

func (e *ErrNoSession) Error() string {
	return fmt.Sprintf("no session for %s", e.Domain)
}

// Open returns this machine's credential store, or says why there is none.
func Open() (Store, error) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("security"); err != nil {
			return nil, fmt.Errorf("the macOS keychain tool is not on PATH: %w", err)
		}
		return keychain{}, nil
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return nil, fmt.Errorf("no credential store: secret-tool is not installed, so " +
				"there is nowhere to keep a session.\n\n" +
				"Install libsecret (Debian/Ubuntu: libsecret-tools, Fedora: libsecret), or " +
				"authenticate with a machine identity instead -- which is what automation " +
				"should use: set INFISICAL_ESCROW_URL, INFISICAL_ESCROW_PROJECT_ID, " +
				"INFISICAL_ESCROW_CLIENT_ID and INFISICAL_ESCROW_CLIENT_SECRET")
		}
		return secretTool{}, nil
	default:
		return nil, fmt.Errorf("no credential store is implemented for %s: authenticate with "+
			"a machine identity instead (INFISICAL_ESCROW_* in the environment)", runtime.GOOS)
	}
}

// keychain is the macOS login keychain, through the `security` tool that ships
// with the OS.
//
// The tool rather than the Security framework, so this needs no cgo and no third
// party package: a credential store is not worth a dependency that has to be
// trusted with exactly this secret.
type keychain struct{}

func (keychain) Name() string { return "the macOS keychain" }

func (k keychain) Save(s Session) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode the session: %w", err)
	}
	// -U updates in place, so a second login replaces the first rather than
	// leaving two entries the OS picks between.
	//
	// -w with NO VALUE, and the blob written TWICE. Passing the token as an
	// argument would put it in `ps` output for the lifetime of the call, so it goes
	// on stdin instead -- and `security` prompts for it twice ("password data for
	// new item:", "retype password for new item:"), so a single write fails with
	// "passwords don't match" and stores nothing while exiting 0. Verified against
	// the tool rather than assumed; the doubled write is the whole reason this is
	// not two lines.
	cmd := exec.Command("security", "add-generic-password",
		"-a", s.Domain, "-s", service, "-U", "-w")
	cmd.Stdin = strings.NewReader(string(blob) + "\n" + string(blob) + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("store the session in the keychain: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (k keychain) Load(domain string) (Session, error) {
	cmd := exec.Command("security", "find-generic-password",
		"-a", domain, "-s", service, "-w")
	out, err := cmd.Output()
	if err != nil {
		return Session{}, &ErrNoSession{Domain: domain}
	}
	return decode(out)
}

func (k keychain) Delete(domain string) error {
	cmd := exec.Command("security", "delete-generic-password", "-a", domain, "-s", service)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "could not be found") {
			return &ErrNoSession{Domain: domain}
		}
		return fmt.Errorf("remove the session from the keychain: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// secretTool is the freedesktop secret service, through libsecret's CLI.
type secretTool struct{}

func (secretTool) Name() string { return "the login keyring" }

func (secretTool) Save(s Session) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode the session: %w", err)
	}
	cmd := exec.Command("secret-tool", "store", "--label=kakt session",
		"service", service, "domain", s.Domain)
	cmd.Stdin = strings.NewReader(string(blob))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("store the session in the keyring: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (secretTool) Load(domain string) (Session, error) {
	out, err := exec.Command("secret-tool", "lookup", "service", service, "domain", domain).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return Session{}, &ErrNoSession{Domain: domain}
	}
	return decode(out)
}

func (secretTool) Delete(domain string) error {
	if out, err := exec.Command("secret-tool", "clear", "service", service, "domain", domain).CombinedOutput(); err != nil {
		return fmt.Errorf("remove the session from the keyring: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// decode reads a stored blob, and refuses a session with no token.
//
// A stored entry that decodes to an empty token would otherwise be handed to the
// escrow and come back as 401, which reads as "your login expired" when the truth
// is that nothing was ever stored.
func decode(raw []byte) (Session, error) {
	var s Session
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &s); err != nil {
		return Session{}, fmt.Errorf("the stored session is not readable: %w", err)
	}
	if strings.TrimSpace(s.Token) == "" {
		return Session{}, fmt.Errorf("the stored session carries no token")
	}
	return s, nil
}

// Current loads the session for a domain and says what to do when there is none.
//
// Every caller needs the same three answers -- no session, expired session, a
// session -- and needs them worded for an operator rather than for a log, so the
// wording lives here once.
func Current(ctx context.Context, domain string) (Session, error) {
	store, err := Open()
	if err != nil {
		return Session{}, err
	}
	s, err := store.Load(domain)
	if err != nil {
		var none *ErrNoSession
		if errors.As(err, &none) {
			return Session{}, fmt.Errorf("not logged in to %s.\n\nRun: kakt login --domain %s",
				domain, domain)
		}
		return Session{}, err
	}
	if s.Expired() {
		return Session{}, fmt.Errorf("your session for %s expired at %s.\n\nRun: kakt login --domain %s",
			domain, s.ExpiresAt.Format(time.RFC3339), domain)
	}
	return s, nil
}
