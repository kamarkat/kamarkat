package support

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Enrolment is what this box holds in order to report, and it is a certificate.
//
// ADR-077 addendum 1 withdrew the enrolment-token exchange. In an
// agent-to-principal design the certificate IS the credential: the identity
// lives in it, the receiver validates it against its own CA, and revocation is a
// fingerprint blocklist at the receiving end. There is no token to leak, no
// first-use window during which a long-lived secret exists, and nothing for an
// operator to remember to rotate -- the leaf is short and reissues on the
// issuance machinery every box already runs (ADR-066).
//
// So "enrolled" means exactly one thing: a client certificate exists at these
// paths. Nothing else distinguishes an enrolled box from an unenrolled one, and
// an unenrolled box is not degraded -- it simply has nowhere to report.
type Enrolment struct {
	CertFile string
	KeyFile  string
	// CAFile validates the PLATFORM's server certificate. Without it the agent
	// would trust the host chain, and a support endpoint is not a public web
	// site: the whole point of naming the CA is that only the platform's
	// ingress can terminate this connection.
	CAFile   string
	Endpoint string
}

// Enrolled reports whether this box can report at all.
//
// Checked by reading the files rather than by a flag, because the files are what
// the enrolment actually is. A box whose certificate has been removed becomes
// unenrolled on its next cycle with nothing else to update.
func (e Enrolment) Enrolled() bool {
	if e.Endpoint == "" || e.CertFile == "" || e.KeyFile == "" {
		return false
	}
	for _, f := range []string{e.CertFile, e.KeyFile} {
		if _, err := os.Stat(f); err != nil {
			return false
		}
	}
	return true
}

// Identity is the subject common name of the client certificate.
//
// Read from the certificate rather than configured, so the agent cannot report
// under a name it was not issued. It is informational here -- the receiving end
// extracts the identity from the certificate itself and never trusts a field in
// the payload -- but printing it is what lets `kakt support preview` show an
// operator which box this would report as.
func (e Enrolment) Identity() (string, error) {
	pair, err := tls.LoadX509KeyPair(e.CertFile, e.KeyFile)
	if err != nil {
		return "", err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", err
	}
	return leaf.Subject.CommonName, nil
}

// Transport sends payloads to the platform, and does nothing else.
type Transport struct {
	Enrolment Enrolment
	client    *http.Client
}

// NewTransport builds the mTLS client.
//
// Fails rather than falling back. A transport that quietly degraded to a plain
// TLS connection when the client certificate would not load would keep
// reporting, unauthenticated, and look healthy doing it.
func NewTransport(e Enrolment) (*Transport, error) {
	pair, err := tls.LoadX509KeyPair(e.CertFile, e.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("support: client certificate: %w", err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS13,
	}
	if e.CAFile != "" {
		pem, err := os.ReadFile(e.CAFile)
		if err != nil {
			return nil, fmt.Errorf("support: CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("support: CA bundle at %s contains no certificate", e.CAFile)
		}
		cfg.RootCAs = pool
	}

	return &Transport{
		Enrolment: e,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: cfg},
		},
	}, nil
}

// Send delivers one payload. Outbound only: there is no response body this
// agent acts on, and nothing the platform returns can instruct it.
//
// That is deliberate and is the other half of the NetworkPolicy having no
// ingress rule. A response that could change what the agent collects would make
// the allowlist advisory -- the platform would be able to widen its own scope
// without publishing a bundle version the tenant merges (ADR-064).
func (t *Transport) Send(ctx context.Context, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Enrolment.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("support: %w", err)
	}
	defer resp.Body.Close()

	// Read, but only far enough to find the one field that means something.
	// Nothing else in the response can change what this agent collects -- see
	// the note above.
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	if isRevocation(body) {
		return ErrEnrolmentRevoked
	}
	return fmt.Errorf("support: endpoint returned %s", resp.Status)
}

// ErrEnrolmentRevoked is the one failure that is not transient.
//
// ADR-077 addendum 3, decision 6: revocation is stated, never inferred. A 401, a
// 403 or a refused handshake all have mundane causes -- a proxy in front of the
// endpoint, a CA bundle that has not rolled over, an expired server certificate,
// a bug on our side -- and every one of them is a fault to retry and surface, not
// a reason to stop reporting. Treating them as revocation would stop a healthy
// tenant reporting because someone misconfigured an ingress.
//
// So the Support Plane has to say so explicitly, and a plane that is down,
// misconfigured or unreachable cannot revoke anything. Failure to communicate is
// never revocation, which is the correct default for the one direction the tenant
// cannot undo.
var ErrEnrolmentRevoked = errors.New(
	"support: the platform reports this enrolment as revoked")

// isRevocation looks for the marker and nothing else.
//
// Parsed permissively on purpose: the field is the contract, and a response that
// is not JSON, or carries other fields, must not be mistaken for one that says
// this. Absence means "not revoked", which is the safe direction -- an agent that
// guessed revocation from a malformed body would stop on a server-side bug.
func isRevocation(body []byte) bool {
	var r struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return false
	}
	return r.Reason == RevokedReason
}

// RevokedReason is the machine-readable marker. One string, checked exactly --
// substring matching would let an error message quoting it revoke an agent.
const RevokedReason = "ENROLMENT_REVOKED"
