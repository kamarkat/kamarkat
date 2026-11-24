package openbao

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
)

// renewalThreshold: a default issuer with less remaining life than this is renewed
// by `kakt pki sign-intermediate --renew` (ADR-117 §6, scheduled renewal).
const renewalThreshold = 2 * 365 * 24 * time.Hour

// SignOptions controls one administrator signing run.
type SignOptions struct {
	BoxID      string
	Kubeconfig string
	// Root is read from the custody project under the administrator's session.
	Root RootCA
	// Validity of the new intermediate; bounded by the root's remaining life.
	Validity time.Duration
	// Renew issues a new intermediate over a NEW key even when the current default
	// issuer is valid (scheduled renewal). Without it, a valid default issuer that
	// chains to this root makes the command a no-op.
	Renew bool
	Now   time.Time
	Out   io.Writer
}

// SignIntermediateOnBox is `kakt pki sign-intermediate`: it validates the box's
// pending CSR, signs it with the custody-held root, and returns only the chain to
// OpenBao. Idempotent: re-running after success changes nothing.
func SignIntermediateOnBox(ctx context.Context, cs kubernetes.Interface, opt SignOptions) error {
	out := opt.Out
	if out == nil {
		out = io.Discard
	}
	rootCert, _, err := ParseRoot(opt.Root, opt.BoxID, opt.Now)
	if err != nil {
		return err
	}
	c, err := Connect(ctx, cs, opt.Kubeconfig)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := waitUnsealed(ctx, c, 30*time.Minute); err != nil {
		return err
	}
	if err := c.LoginAs(ctx, cs, saPKISigning, rolePKISigning); err != nil {
		return err
	}

	current, err := defaultIssuer(ctx, c)
	if err != nil {
		return err
	}
	if current != nil {
		if current.CheckSignatureFrom(rootCert) != nil {
			return errors.New("this box's default issuer does not chain to the custody root; the trust " +
				"hierarchy is not the expected one (refusing to sign). If the root was replaced after a " +
				"compromise, follow the root-rollover procedure")
		}
		if !opt.Renew && current.NotAfter.Sub(opt.Now) > renewalThreshold {
			fmt.Fprintf(out, "The issuing intermediate is already signed by this box's root (valid until %s). Nothing to do.\n",
				current.NotAfter.Format(time.RFC3339))
			return nil
		}
	}

	keyName, err := keyToSign(ctx, c, current != nil)
	if err != nil {
		return err
	}
	var gen struct {
		Data struct {
			CSR string `json:"csr"`
		} `json:"data"`
	}
	if err := c.Write(ctx, "pki_int/intermediate/generate/existing", map[string]any{
		"key_ref": keyName, "common_name": IntermediateCommonName,
	}, &gen); err != nil {
		return fmt.Errorf("ask OpenBao for a CSR over key %s: %w", keyName, err)
	}
	chain, err := SignIntermediate([]byte(gen.Data.CSR), opt.Root, opt.BoxID, opt.Validity, opt.Now)
	if err != nil {
		return err
	}
	var set struct {
		Data struct {
			ImportedIssuers []string `json:"imported_issuers"`
		} `json:"data"`
	}
	if err := c.Write(ctx, "pki_int/intermediate/set-signed", map[string]any{"certificate": string(chain)}, &set); err != nil {
		return fmt.Errorf("install the signed chain: %w", err)
	}
	issuerID, err := issuerFor(ctx, c, set.Data.ImportedIssuers)
	if err != nil {
		return err
	}
	if err := c.Write(ctx, "pki_int/issuer/"+issuerID, map[string]any{
		"issuer_name": keyName, "leaf_not_after_behavior": "err",
	}, nil); err != nil {
		return fmt.Errorf("configure issuer %s: %w", issuerID, err)
	}
	if err := c.Write(ctx, "pki_int/config/issuers", map[string]any{"default": issuerID}, nil); err != nil {
		return fmt.Errorf("make issuer %s the default: %w", issuerID, err)
	}
	fmt.Fprintf(out, "Signed and installed the issuing intermediate (issuer %s, key %s). Only the certificate chain left this machine.\n",
		issuerID, keyName)
	return nil
}

// IntermediateSigned is Day-0's pki-signed check: the unauthenticated default CA
// endpoint serves an intermediate that chains to the box's root certificate.
func IntermediateSigned(ctx context.Context, c *Client, rootCertPEM []byte) (bool, error) {
	root, err := parseCertPEM(rootCertPEM)
	if err != nil {
		return false, fmt.Errorf("the box's root certificate (box escrow %s): %w", ArtifactRootCertificate, err)
	}
	cert, err := c.rawPEM(ctx, "pki_int/ca/pem")
	if errors.Is(err, ErrNotFound) || cert == nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cert.CheckSignatureFrom(root) == nil, nil
}

func defaultIssuer(ctx context.Context, c *Client) (*x509.Certificate, error) {
	cert, err := c.rawPEM(ctx, "pki_int/ca/pem")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return cert, err
}

// keyToSign returns the self-init key when no issuer exists yet, and a new key
// (issuing-N+1) for a renewal, so the old issuer keeps signing during the overlap.
func keyToSign(ctx context.Context, c *Client, renewal bool) (string, error) {
	var keys struct {
		Data struct {
			KeyInfo map[string]struct {
				KeyName string `json:"key_name"`
			} `json:"key_info"`
		} `json:"data"`
	}
	err := c.List(ctx, "pki_int/keys", &keys)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("list OpenBao PKI keys: %w", err)
	}
	var names []string
	for _, k := range keys.Data.KeyInfo {
		if strings.HasPrefix(k.KeyName, "issuing-") {
			names = append(names, k.KeyName)
		}
	}
	sort.Slice(names, func(i, j int) bool { return keyIndex(names[i]) < keyIndex(names[j]) })
	if !renewal {
		if len(names) == 0 {
			return "", fmt.Errorf("OpenBao has no %s key: self-initialization did not complete", IntermediateKeyName)
		}
		return names[len(names)-1], nil
	}
	next := "issuing-" + strconv.Itoa(keyIndex(names[len(names)-1])+1)
	if err := c.Write(ctx, "pki_int/keys/generate/internal", map[string]any{
		"key_name": next, "key_type": "rsa", "key_bits": 4096,
	}, nil); err != nil {
		return "", fmt.Errorf("generate renewal key %s inside OpenBao: %w", next, err)
	}
	return next, nil
}

func keyIndex(name string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(name, "issuing-"))
	return n
}

func issuerFor(ctx context.Context, c *Client, imported []string) (string, error) {
	if len(imported) == 1 {
		return imported[0], nil
	}
	// set-signed reports only NEWLY imported issuers; an empty list means this exact
	// chain was imported before (a re-run). Find it among the issuers instead.
	var issuers struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.List(ctx, "pki_int/issuers", &issuers); err != nil {
		return "", fmt.Errorf("list issuers: %w", err)
	}
	if len(issuers.Data.Keys) == 0 {
		return "", errors.New("OpenBao imported no issuer from the signed chain")
	}
	return issuers.Data.Keys[len(issuers.Data.Keys)-1], nil
}

// rawPEM reads an unauthenticated PEM endpoint.
func (c *Client) rawPEM(ctx context.Context, path string) (*x509.Certificate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+"/v1/"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || len(strings.TrimSpace(string(body))) == 0 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return parseCertPEM(body)
}
