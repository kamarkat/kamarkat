// Package session holds the developer's own Infisical login, in the operating
// system's credential store.
//
// WHY A SESSION AT ALL. Every external command that reaches a hub needs a
// credential for the box's escrow, and that credential used to be a machine
// identity: four INFISICAL_ESCROW_* values exported into the environment or left
// in k8-secrets/infisical/ by whoever ran `kakt escrow init`. A shared secret is
// then the price of admission -- it has to be copied onto each workstation, it
// authenticates every person as the same identity, it cannot be revoked for one
// of them, and the audit log records the box rather than the operator.
//
// A session replaces the copying with an authentication. The developer logs in as
// themselves, the token is theirs, their permissions decide what they may read,
// and Infisical attributes the read to them. A machine identity remains the right
// answer for automation, and `hubaccess` prefers it when the environment carries
// one -- a separately provisioned identity assigned to one developer is still a
// machine identity, so it is not the interactive path.
//
// WHY THE LIBRARY, AND NOT /usr/bin/security. The first version of this package
// drove `security add-generic-password -U -w` with the secret on stdin, which
// works headlessly and HANGS IN A TERMINAL: with a controlling tty present the
// tool prompts on /dev/tty ("password data for new item:") and ignores the pipe.
// It was tested where the bug cannot appear. go-keyring avoids the prompt the way
// the vendor's own CLI does -- `security -i`, with the whole command including the
// value written to stdin, so nothing is prompted for and the value never enters
// argv where `ps` would show it -- and it brings Windows Credential Manager and
// the Linux Secret Service with it.
//
// Upstream, deliberately, not Infisical's fork. The fork exists to add a `file`
// backend, and their wrapper falls back to it by generating a passphrase and
// writing it into their own config file. That is a plaintext-adjacent vault and
// the exact shape ADR-076 argues against, so a machine with no OS credential
// store is told to use a machine identity instead -- which is what CI should be
// doing anyway.
package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// service and the account key are FIXED, and the account is the Infisical domain.
//
// One entry per domain, so a developer working against a company instance and
// app.infisical.com keeps one session each rather than alternating. The service
// string is a constant because it is the lookup key: deriving it from anything --
// a version, a path, a hostname -- would orphan every session the day that
// changed.
const service = "kakt"

// maxEncodedBytes refuses a value too large for the platform stores before the
// store does.
//
// macOS passes the value through `security -i` as part of a command line, and the
// Secret Service and Credential Manager have their own caps; Infisical's CLI
// records the same limit ("platform keyrings cap a single entry at a few
// kilobytes") and splits entries to stay under it. A session is a JWT plus three
// short strings, so this is headroom rather than a constraint -- but a token that
// grew would otherwise fail inside the store with a message about neither.
const maxEncodedBytes = 3072

// Session is what a login produces.
//
// The token is an Infisical USER session, which the escrow's secret routes accept
// beside a machine identity's (their verifyAuth lists AuthMode.JWT). Email and
// organization are carried so `kakt whoami` can answer "as whom" without a round
// trip, since a token alone reveals neither.
type Session struct {
	Domain string `json:"domain"`
	Token  string `json:"token"`
	// UserID is Infisical's own identifier for the authenticated human, and it is
	// what authorization compares against.
	//
	// The email is for display and nothing else. An address can be changed, and
	// on some providers re-assigned, so a box that named its administrator by
	// email would transfer that authority to whoever next holds the address --
	// silently, and in the one place where silence is least affordable.
	UserID    string    `json:"userId"`
	Email     string    `json:"email"`
	OrgID     string    `json:"orgId"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// Expired reports whether the session is past its stated expiry.
//
// An unknown expiry is NOT expired. Infisical does not always state one, and
// refusing a token the server would have accepted sends a developer through a
// browser login for nothing; the server's 401 is the authority, and the commands
// turn that into the actionable error.
func (s Session) Expired() bool {
	return !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt)
}

// ErrNoSession is returned when the store holds nothing for a domain.
type ErrNoSession struct{ Domain string }

func (e *ErrNoSession) Error() string { return fmt.Sprintf("no session for %s", e.Domain) }

// ErrNoStore is returned when this machine has no usable credential store.
//
// Distinct from ErrNoSession because the remedy is different and neither is a
// login problem: one is "log in", the other is "this machine cannot keep a
// session, use a machine identity".
type ErrNoStore struct{ Err error }

func (e *ErrNoStore) Error() string {
	return fmt.Sprintf("no usable OS credential store: %v", e.Err)
}
func (e *ErrNoStore) Unwrap() error { return e.Err }

// CredentialStore is the one abstraction over the platform stores.
//
// An interface rather than three functions so a test can substitute a store
// without a keychain, and so the rest of the CLI depends on the operation rather
// than on go-keyring.
type CredentialStore interface {
	Save(Session) error
	Load(domain string) (Session, error)
	Delete(domain string) error
	// Name is what to call the store in a message an operator reads.
	Name() string
}

// Store is the OS credential store, through go-keyring.
type Store struct{}

// Open returns this machine's store.
//
// It does not probe. go-keyring reports an unavailable backend on the first real
// operation, and a probe here would either write a throwaway entry -- leaving
// litter in a developer's keychain on every command -- or test something other
// than what the next call does.
func Open() (CredentialStore, error) { return Store{}, nil }

func (Store) Name() string { return "your OS credential store" }

// Save stores the session and PROVES IT by reading it back.
//
// The read-back is not belt and braces. The first version of this package wrote
// through a tool that printed its prompt, consumed nothing, and exited 0: the
// session was never stored and the next command reported "not logged in" with no
// hint that the write had failed. A store that silently does nothing is the one
// failure a credential store must not have.
func (s Store) Save(sess Session) error {
	if strings.TrimSpace(sess.Domain) == "" {
		return fmt.Errorf("a session needs the domain it belongs to")
	}
	if strings.TrimSpace(sess.Token) == "" {
		return fmt.Errorf("a session needs a token")
	}
	encoded, err := encode(sess)
	if err != nil {
		return err
	}
	if len(encoded) > maxEncodedBytes {
		return fmt.Errorf("this session is %d bytes encoded, over the %d the platform "+
			"credential stores accept for one entry", len(encoded), maxEncodedBytes)
	}
	if err := keyring.Set(service, sess.Domain, encoded); err != nil {
		return storeError(err)
	}

	readBack, err := s.Load(sess.Domain)
	if err != nil {
		return fmt.Errorf("the session was written but cannot be read back: %w", err)
	}
	if readBack.Token != sess.Token {
		return fmt.Errorf("the credential store returned a different session than was "+
			"written for %s, so nothing here can be trusted to persist", sess.Domain)
	}
	return nil
}

func (Store) Load(domain string) (Session, error) {
	raw, err := keyring.Get(service, domain)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return Session{}, &ErrNoSession{Domain: domain}
		}
		return Session{}, storeError(err)
	}
	return decode(raw)
}

func (Store) Delete(domain string) error {
	if err := keyring.Delete(service, domain); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return &ErrNoSession{Domain: domain}
		}
		return storeError(err)
	}
	return nil
}

// storeError turns an unavailable backend into the one error whose remedy is a
// machine identity rather than a login.
func storeError(err error) error {
	if errors.Is(err, keyring.ErrUnsupportedPlatform) {
		return &ErrNoStore{Err: err}
	}
	// A Linux session with no Secret Service provider running reports a D-Bus
	// failure rather than an unsupported platform, and it has the same remedy.
	if strings.Contains(err.Error(), "dbus") || strings.Contains(err.Error(), "org.freedesktop.secrets") {
		return &ErrNoStore{Err: err}
	}
	return err
}

// encode stores the session base64'd.
//
// Not for secrecy -- the store provides that -- but because the value crosses a
// shell on macOS, where go-keyring passes it through `security -i` as part of a
// command. base64 has no quotes, spaces or newlines, so there is nothing for
// escaping to get wrong, which is the class of bug upstream carries shellescape
// to handle.
func encode(s Session) (string, error) {
	blob, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode the session: %w", err)
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

func decode(raw string) (Session, error) {
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return Session{}, fmt.Errorf("the stored session is not readable: %w", err)
	}
	var s Session
	if err := json.Unmarshal(blob, &s); err != nil {
		return Session{}, fmt.Errorf("the stored session is not readable: %w", err)
	}
	if strings.TrimSpace(s.Token) == "" {
		return Session{}, fmt.Errorf("the stored session carries no token")
	}
	return s, nil
}

// Current loads the session for a domain and says what to do when there is none.
//
// Every caller needs the same three answers -- no store, no session, expired --
// worded for an operator rather than for a log, so the wording lives here once.
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
		var noStore *ErrNoStore
		if errors.As(err, &noStore) {
			return Session{}, fmt.Errorf("%w.\n\n"+
				"A session cannot be kept on this machine. Use a machine identity instead, "+
				"which is what automation should use anyway:\n\n"+
				"  INFISICAL_ESCROW_URL  INFISICAL_ESCROW_PROJECT_ID\n"+
				"  INFISICAL_ESCROW_CLIENT_ID  INFISICAL_ESCROW_CLIENT_SECRET", err)
		}
		return Session{}, err
	}
	if s.Expired() {
		return Session{}, fmt.Errorf("your session for %s expired at %s.\n\nRun: kakt login --domain %s",
			domain, s.ExpiresAt.Format(time.RFC3339), domain)
	}
	return s, nil
}
