package openbao

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

// RolloverOptions is one compromise rollover (ADR-117 §8.3): the intermediate's
// key is compromised, and relying parties trust the ROOT, so a new intermediate
// alone would not stop them accepting what the compromised key signs. The root is
// replaced, a new intermediate is signed under it, every other issuer and its key
// are destroyed, and every leaf is reissued so its ca.crt carries only the new
// root.
type RolloverOptions struct {
	BoxID string
	// Root is the custody root as read at the start of the run.
	Root RootCA
	// Custody holds the root (administrator only); Box receives its certificate.
	Custody, Box ProjectStore
	// Compromised is the serial number of the compromised intermediate, in hex
	// (colons allowed). Naming it makes the rollover deliberate and lets a re-run
	// tell a finished rollover from one that is needed.
	Compromised string
	Validity    time.Duration
	Now         time.Time
	Out         io.Writer
}

type pkiIssuer struct {
	ID    string
	KeyID string
	Cert  *x509.Certificate
}

func parseSerial(s string) (*big.Int, error) {
	h := strings.ToLower(strings.NewReplacer(":", "", "-", "", " ", "").Replace(strings.TrimSpace(s)))
	h = strings.TrimPrefix(h, "0x")
	n, ok := new(big.Int).SetString(h, 16)
	if h == "" || !ok {
		return nil, fmt.Errorf("%q is not a certificate serial number in hex", s)
	}
	return n, nil
}

func listIssuers(ctx context.Context, c *Client) ([]pkiIssuer, error) {
	var l struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.List(ctx, "pki_int/issuers", &l); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("list issuers: %w", err)
	}
	var out []pkiIssuer
	for _, id := range l.Data.Keys {
		var r struct {
			Data struct {
				Certificate string `json:"certificate"`
				KeyID       string `json:"key_id"`
			} `json:"data"`
		}
		if err := c.Read(ctx, "pki_int/issuer/"+id, &r); err != nil {
			return nil, fmt.Errorf("read issuer %s: %w", id, err)
		}
		cert, err := parseCertPEM([]byte(r.Data.Certificate))
		if err != nil {
			return nil, fmt.Errorf("issuer %s: %w", id, err)
		}
		out = append(out, pkiIssuer{ID: id, KeyID: r.Data.KeyID, Cert: cert})
	}
	return out, nil
}

// RollOverCompromised performs the OpenBao and custody half of a compromise
// rollover and returns the root now in force. c must be logged in as the
// signing identity. Every step is idempotent, so an interrupted run is finished by
// running it again with the same serial.
func RollOverCompromised(ctx context.Context, c *Client, opt RolloverOptions) (RootCA, error) {
	out := opt.Out
	if out == nil {
		out = io.Discard
	}
	serial, err := parseSerial(opt.Compromised)
	if err != nil {
		return RootCA{}, err
	}
	root := opt.Root
	rootCert, _, err := ParseRoot(root, opt.BoxID, opt.Now)
	if err != nil {
		return RootCA{}, err
	}
	var replaced *x509.Certificate
	if len(root.Replaces) > 0 {
		if replaced, err = parseCertPEM(root.Replaces); err != nil {
			return RootCA{}, fmt.Errorf("the custody root's record of the root it replaced: %w", err)
		}
	}

	issuers, err := listIssuers(ctx, c)
	if err != nil {
		return RootCA{}, err
	}
	var compromised *pkiIssuer
	for i := range issuers {
		if issuers[i].Cert.SerialNumber.Cmp(serial) == 0 {
			compromised = &issuers[i]
		}
	}

	// 1. The root. A compromised intermediate under the CURRENT root means the root
	// has not been replaced yet; under the root it replaced, it already has.
	switch {
	case compromised != nil && compromised.Cert.CheckSignatureFrom(rootCert) == nil:
		next, err := GenerateRootCA(opt.BoxID, opt.Now)
		if err != nil {
			return RootCA{}, err
		}
		next.Replaces = root.CertPEM
		payload, _ := json.Marshal(next)
		// Custody first: once the new root is recorded there, a re-run continues from
		// it. The replaced root's key is not kept.
		if err := opt.Custody.BackupProjectArtifact(ctx, ArtifactRootCA, string(payload)); err != nil {
			return RootCA{}, fmt.Errorf("store the new root in custody: %w", err)
		}
		root = next
		if rootCert, _, err = ParseRoot(root, opt.BoxID, opt.Now); err != nil {
			return RootCA{}, err
		}
		fmt.Fprintf(out, "Created a new root for box %s; the compromised hierarchy's root is retired.\n", opt.BoxID)
	case compromised != nil && replaced != nil && compromised.Cert.CheckSignatureFrom(replaced) == nil:
		fmt.Fprintln(out, "The root was already replaced by an earlier run; continuing.")
	case compromised != nil:
		return RootCA{}, fmt.Errorf("intermediate %s chains to neither this box's root nor the root it "+
			"replaced; refusing to act on an unknown hierarchy", opt.Compromised)
	case replaced == nil:
		return RootCA{}, fmt.Errorf("this box's OpenBao has no intermediate with serial %s; nothing to roll over "+
			"(the serial is the intermediate's, as `openssl x509 -serial` prints it)", opt.Compromised)
	default:
		fmt.Fprintf(out, "Intermediate %s is already destroyed; finishing the rollover.\n", opt.Compromised)
	}
	if err := opt.Box.BackupProjectArtifact(ctx, ArtifactRootCertificate, string(root.CertPEM)); err != nil {
		return RootCA{}, fmt.Errorf("publish the new root certificate to the box escrow: %w", err)
	}

	// 2. A default issuer under the new root.
	current, err := defaultIssuer(ctx, c)
	if err != nil {
		return RootCA{}, err
	}
	if current == nil || current.CheckSignatureFrom(rootCert) != nil {
		keyName, err := keyToSign(ctx, c, current != nil)
		if err != nil {
			return RootCA{}, err
		}
		issuerID, err := installIntermediate(ctx, c, keyName, root, opt.BoxID, opt.Validity, opt.Now)
		if err != nil {
			return RootCA{}, err
		}
		fmt.Fprintf(out, "Signed and installed a new issuing intermediate under the new root (issuer %s, key %s).\n",
			issuerID, keyName)
	}

	// 3. Destroy every issuer not under the new root, then every key no remaining
	// issuer uses -- the compromised key included. OpenBao refuses to delete a key an
	// issuer still references, so issuers go first.
	if issuers, err = listIssuers(ctx, c); err != nil {
		return RootCA{}, err
	}
	keep := map[string]bool{}
	for _, is := range issuers {
		if is.Cert.CheckSignatureFrom(rootCert) == nil {
			keep[is.KeyID] = true
			continue
		}
		if err := c.Delete(ctx, "pki_int/issuer/"+is.ID); err != nil {
			return RootCA{}, fmt.Errorf("destroy issuer %s (serial %x): %w", is.ID, is.Cert.SerialNumber, err)
		}
		fmt.Fprintf(out, "Destroyed issuer %s (serial %x).\n", is.ID, is.Cert.SerialNumber)
	}
	if len(keep) == 0 {
		return RootCA{}, errors.New("no issuer under the new root remains; refusing to destroy keys")
	}
	var keys struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.List(ctx, "pki_int/keys", &keys); err != nil && !errors.Is(err, ErrNotFound) {
		return RootCA{}, fmt.Errorf("list OpenBao PKI keys: %w", err)
	}
	for _, k := range keys.Data.Keys {
		if keep[k] {
			continue
		}
		if err := c.Delete(ctx, "pki_int/key/"+k); err != nil {
			return RootCA{}, fmt.Errorf("destroy key %s: %w", k, err)
		}
		fmt.Fprintf(out, "Destroyed key %s.\n", k)
	}
	return root, nil
}
