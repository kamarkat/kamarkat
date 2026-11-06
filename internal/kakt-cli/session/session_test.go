package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// The whole point of the store is that a write persists. go-keyring's mock
// exercises Set/Get/Delete without touching a developer's real keychain, which
// is what CI must do; the native stores are verified by running `kakt login` on
// each platform, because a mock cannot prove a platform backend.
func TestSaveLoadDeleteRoundTrip(t *testing.T) {
	keyring.MockInit()
	s := Store{}
	const domain = "https://app.infisical.com"

	want := Session{Domain: domain, Token: "a.b.c", Email: "dev@example.com", OrgID: "org-1"}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Token != want.Token || got.Email != want.Email || got.OrgID != want.OrgID {
		t.Fatalf("round trip changed the session: %+v", got)
	}

	// Logging in again replaces the session rather than adding a second entry the
	// store would then choose between.
	second := want
	second.Token = "d.e.f"
	if err := s.Save(second); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if got, _ = s.Load(domain); got.Token != "d.e.f" {
		t.Fatalf("a second login did not replace the first: %q", got.Token)
	}

	if err := s.Delete(domain); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var none *ErrNoSession
	if _, err := s.Load(domain); !errors.As(err, &none) {
		t.Fatalf("after logout, Load returned %v, want ErrNoSession", err)
	}
	// Logging out twice is not an error state to handle; it is already true.
	if err := s.Delete(domain); !errors.As(err, &none) {
		t.Fatalf("second Delete returned %v, want ErrNoSession", err)
	}
}

// One entry per domain: a developer with two instances keeps a session for each.
func TestSessionsAreSeparatePerDomain(t *testing.T) {
	keyring.MockInit()
	s := Store{}
	if err := s.Save(Session{Domain: "https://a.example", Token: "t-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Session{Domain: "https://b.example", Token: "t-b"}); err != nil {
		t.Fatal(err)
	}
	for domain, want := range map[string]string{"https://a.example": "t-a", "https://b.example": "t-b"} {
		got, err := s.Load(domain)
		if err != nil {
			t.Fatalf("%s: %v", domain, err)
		}
		if got.Token != want {
			t.Errorf("%s holds %q, want %q", domain, got.Token, want)
		}
	}
}

// A session with no token is refused at the door. Stored, it would be handed to
// the escrow and come back 401, which reads as "your login expired" when nothing
// was ever stored.
func TestSaveRefusesAnEmptySession(t *testing.T) {
	keyring.MockInit()
	s := Store{}
	for _, c := range []struct {
		name string
		sess Session
	}{
		{"no token", Session{Domain: "https://a.example"}},
		{"no domain", Session{Token: "t"}},
	} {
		if err := s.Save(c.sess); err == nil {
			t.Errorf("%s was stored", c.name)
		}
	}
}

// An expiry in the past is expired; an absent expiry is not. Infisical does not
// always state one, and treating silence as expiry sends a developer through a
// browser login for nothing.
func TestExpiry(t *testing.T) {
	if (Session{}).Expired() {
		t.Error("a session with no stated expiry was treated as expired")
	}
	if !(Session{ExpiresAt: time.Now().Add(-time.Minute)}).Expired() {
		t.Error("a session that expired a minute ago was treated as current")
	}
	if (Session{ExpiresAt: time.Now().Add(time.Hour)}).Expired() {
		t.Error("a session valid for an hour was treated as expired")
	}
}

// Current words the three outcomes for an operator, and names the command that
// fixes each.
func TestCurrentSaysWhatToDo(t *testing.T) {
	keyring.MockInit()
	const domain = "https://app.infisical.com"

	if _, err := Current(t.Context(), domain); err == nil ||
		!strings.Contains(err.Error(), "kakt login") {
		t.Fatalf("with no session, Current said: %v", err)
	}

	if err := (Store{}).Save(Session{
		Domain: domain, Token: "t", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := Current(t.Context(), domain)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("with an expired session, Current said: %v", err)
	}
}

// The value crosses a shell on macOS, where go-keyring passes it through
// `security -i`. base64 leaves nothing for escaping to get wrong -- which is the
// bug class upstream carries shellescape for.
func TestEncodingIsShellSafe(t *testing.T) {
	encoded, err := encode(Session{
		Domain: "https://a.example",
		Token:  `tok'en with "quotes" and $(a command) and a	tab`,
		Email:  "dev@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"'", `"`, " ", "\t", "\n", "$"} {
		if strings.Contains(encoded, bad) {
			t.Errorf("the encoded session contains %q, which a shell would interpret", bad)
		}
	}
	back, err := decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if back.Token != `tok'en with "quotes" and $(a command) and a	tab` {
		t.Errorf("the token did not survive encoding: %q", back.Token)
	}
}
