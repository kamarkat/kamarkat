package openbao

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func csr(t *testing.T, cn string, dns ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn}, DNSNames: dns,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestSignIntermediateChainsToRootAndIsBounded(t *testing.T) {
	root, err := GenerateRootCA("box-1", now)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := SignIntermediate(csr(t, IntermediateCommonName), root, "box-1", 100*365*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	inter, err := parseCertPEM(chain)
	if err != nil {
		t.Fatal(err)
	}
	rootCert, _ := parseCertPEM(root.CertPEM)
	if err := inter.CheckSignatureFrom(rootCert); err != nil {
		t.Fatalf("intermediate does not chain to the root: %v", err)
	}
	if !inter.IsCA || inter.MaxPathLen != 0 || !inter.MaxPathLenZero {
		t.Fatal("intermediate must be a CA with path length 0")
	}
	if !inter.NotAfter.Before(rootCert.NotAfter) {
		t.Fatalf("intermediate outlives the root: %s >= %s", inter.NotAfter, rootCert.NotAfter)
	}
	if !strings.Contains(string(chain), string(root.CertPEM)) {
		t.Fatal("chain must include the root certificate")
	}
}

func TestSignIntermediateRefusals(t *testing.T) {
	root, err := GenerateRootCA("box-1", now)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		csr   []byte
		box   string
		wants string
	}{
		"wrong subject":  {csr(t, "something else"), "box-1", "not this box's issuing intermediate"},
		"SANs requested": {csr(t, IntermediateCommonName, "evil.example"), "box-1", "subject alternative names"},
		"another box":    {csr(t, IntermediateCommonName), "box-2", "not this box's"},
		"not a CSR":      {[]byte("garbage"), "box-1", "no certificate request"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := SignIntermediate(c.csr, root, c.box, 0, now)
			if err == nil || !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("want refusal containing %q, got %v", c.wants, err)
			}
		})
	}
}

func TestParseRootRejectsMismatchedKey(t *testing.T) {
	a, _ := GenerateRootCA("box-1", now)
	b, _ := GenerateRootCA("box-1", now)
	if _, _, err := ParseRoot(RootCA{CertPEM: a.CertPEM, KeyPEM: b.KeyPEM}, "box-1", now); err == nil {
		t.Fatal("a root key that does not match its certificate must be refused")
	}
}

func TestRecoveryPGPRoundTripAndFingerprint(t *testing.T) {
	k, err := GenerateRecoveryPGP("box-1")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := PublicFingerprint(k.PublicB64)
	if err != nil || fp != k.Fingerprint {
		t.Fatalf("fingerprint from public key %q != %q (%v)", fp, k.Fingerprint, err)
	}
	ct, err := EncryptTo(k.PublicB64, []byte("share"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := DecryptWith(k.PrivateArmored, ct)
	if err != nil || string(pt) != "share" {
		t.Fatalf("round trip failed: %q %v", pt, err)
	}
}
