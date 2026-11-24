// Package openbao is the CLI side of the box's secret store (ADR-117): custody
// material generated before any cluster exists, the seal and listener inputs Day-0
// delivers, the Day-0 trust bootstrap, and the administrator's intermediate signing.
//
// Nothing here runs inside a cluster, and nothing here gives a runtime or CI identity
// access to root or recovery material: the custody operations take an escrow store
// opened under the administrator's own session.
package openbao

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	// The maintained OpenPGP implementation, at the version OpenBao 2.7.3 itself uses
	// for pgp_keys (its go.mod), so both ends of the recovery shares interoperate.
	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const (
	// IntermediateCommonName is the subject OpenBao's CSR carries. The signer refuses
	// any other, so a CSR that is not this box's issuing intermediate is never signed.
	IntermediateCommonName = "Kamarkat Issuing Intermediate"

	// IntermediateKeyName is the key self-initialization creates inside OpenBao
	// (manifests/hub-core-services/openbao/values.yaml, initialize "pki").
	IntermediateKeyName = "issuing-1"

	// RootValidity and DefaultIntermediateValidity are defaults, not invariants
	// (ADR-117 §6). The intermediate is always bounded by the root's remaining life.
	RootValidity                = 20 * 365 * 24 * time.Hour
	DefaultIntermediateValidity = 5 * 365 * 24 * time.Hour

	rootCommonNamePrefix = "Kamarkat Root CA "
)

// RootCA is the box's trust anchor. The key exists only in the custody project
// and, briefly, in the memory of an administrator-run command.
type RootCA struct {
	CertPEM []byte
	KeyPEM  []byte
}

// GenerateRootCA creates the box's root. Its subject names the box, so a root can
// be checked against the box it is used for before it signs anything.
func GenerateRootCA(boxID string, now time.Time) (RootCA, error) {
	boxID = strings.TrimSpace(boxID)
	if boxID == "" {
		return RootCA{}, errors.New("a root CA belongs to one box: the box id is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return RootCA{}, fmt.Errorf("generate root key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return RootCA{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: rootCommonNamePrefix + boxID, Organization: []string{"Kamarkat"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(RootValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return RootCA{}, fmt.Errorf("self-sign root: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return RootCA{}, fmt.Errorf("encode root key: %w", err)
	}
	return RootCA{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// ParseRoot validates a root read back from custody: a CA, self-signed, unexpired,
// and belonging to boxID. Every check is a refusal, never a warning.
func ParseRoot(r RootCA, boxID string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := parseCertPEM(r.CertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("root certificate: %w", err)
	}
	if want := rootCommonNamePrefix + strings.TrimSpace(boxID); cert.Subject.CommonName != want {
		return nil, nil, fmt.Errorf("the custody root is %q, not this box's (%q): refusing to sign with "+
			"another box's trust anchor", cert.Subject.CommonName, want)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return nil, nil, errors.New("the custody root is not a CA certificate")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf("the custody root is not self-signed: %w", err)
	}
	if now.After(cert.NotAfter) || now.Before(cert.NotBefore) {
		return nil, nil, fmt.Errorf("the custody root is outside its validity (%s to %s)", cert.NotBefore, cert.NotAfter)
	}
	block, _ := pem.Decode(r.KeyPEM)
	if block == nil {
		return nil, nil, errors.New("the custody root key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("the custody root key: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, errors.New("the custody root key does not match its certificate")
	}
	return cert, key, nil
}

// SignIntermediate validates OpenBao's CSR and signs it with the root. It returns
// only the certificate chain (intermediate, then root); no private key is handled.
//
// The CSR must be for this box's issuing intermediate: the expected subject, a
// valid self-signature, no SANs and no requested extensions. The validity is
// bounded by the root's remaining life.
func SignIntermediate(csrPEM []byte, root RootCA, boxID string, validity time.Duration, now time.Time) ([]byte, error) {
	rootCert, rootKey, err := ParseRoot(root, boxID, now)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || !strings.Contains(block.Type, "CERTIFICATE REQUEST") {
		return nil, errors.New("OpenBao returned no certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the intermediate CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the intermediate CSR's signature is invalid: %w", err)
	}
	if csr.Subject.CommonName != IntermediateCommonName {
		return nil, fmt.Errorf("the CSR is for %q, not this box's issuing intermediate (%q)",
			csr.Subject.CommonName, IntermediateCommonName)
	}
	if len(csr.DNSNames)+len(csr.IPAddresses)+len(csr.EmailAddresses)+len(csr.URIs) > 0 {
		return nil, errors.New("the intermediate CSR requests subject alternative names; a CA needs none")
	}
	for _, ext := range csr.Extensions {
		// Basic constraints and key usage are set by the signer, not taken from the
		// request; anything else requested is refused rather than ignored.
		if !ext.Id.Equal(oidBasicConstraints) && !ext.Id.Equal(oidKeyUsage) && !ext.Id.Equal(oidSubjectKeyID) {
			return nil, fmt.Errorf("the intermediate CSR requests an unexpected extension %s", ext.Id)
		}
	}
	if validity <= 0 {
		validity = DefaultIntermediateValidity
	}
	notAfter := now.Add(validity)
	if limit := rootCert.NotAfter.Add(-24 * time.Hour); notAfter.After(limit) {
		notAfter = limit
	}
	if !notAfter.After(now) {
		return nil, errors.New("the root has no remaining validity to sign an intermediate under")
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               csr.Subject,
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, rootCert, csr.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("sign the intermediate: %w", err)
	}
	var chain bytes.Buffer
	_ = pem.Encode(&chain, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	chain.Write(root.CertPEM)
	return chain.Bytes(), nil
}

// RecoveryPGP is the key pair the recovery shares are encrypted to (ADR-117 §3).
// The private half goes to custody only; the public half to the box escrow.
type RecoveryPGP struct {
	// PublicB64 is the binary public key, base64-encoded: the form OpenBao's
	// pgp_keys parameter takes.
	PublicB64 string
	// PrivateArmored is the ASCII-armored private key, held only in custody.
	PrivateArmored string
	Fingerprint    string
}

// GenerateRecoveryPGP creates the pair and proves it round-trips before returning:
// a key whose private half cannot decrypt what its public half encrypts would make
// the escrowed recovery shares unrecoverable, and that must be found now.
func GenerateRecoveryPGP(boxID string) (RecoveryPGP, error) {
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 4096}
	ent, err := openpgp.NewEntity("kamarkat-recovery", "OpenBao recovery key for "+boxID, "", cfg)
	if err != nil {
		return RecoveryPGP{}, fmt.Errorf("generate recovery PGP key: %w", err)
	}
	var pub bytes.Buffer
	if err := ent.Serialize(&pub); err != nil {
		return RecoveryPGP{}, fmt.Errorf("serialize recovery PGP public key: %w", err)
	}
	var priv bytes.Buffer
	w, err := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	if err != nil {
		return RecoveryPGP{}, err
	}
	if err := ent.SerializePrivate(w, nil); err != nil {
		return RecoveryPGP{}, fmt.Errorf("serialize recovery PGP private key: %w", err)
	}
	if err := w.Close(); err != nil {
		return RecoveryPGP{}, err
	}
	k := RecoveryPGP{
		PublicB64:      base64.StdEncoding.EncodeToString(pub.Bytes()),
		PrivateArmored: priv.String(),
		Fingerprint:    fmt.Sprintf("%x", ent.PrimaryKey.Fingerprint),
	}
	probe := []byte("kamarkat recovery round-trip")
	ct, err := EncryptTo(k.PublicB64, probe)
	if err != nil {
		return RecoveryPGP{}, err
	}
	pt, err := DecryptWith(k.PrivateArmored, ct)
	if err != nil || !bytes.Equal(pt, probe) {
		return RecoveryPGP{}, fmt.Errorf("the recovery PGP key does not round-trip: %v", err)
	}
	return k, nil
}

// PublicFingerprint returns the fingerprint of a base64 binary public key.
func PublicFingerprint(publicB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(publicB64)
	if err != nil {
		return "", fmt.Errorf("decode recovery PGP public key: %w", err)
	}
	el, err := openpgp.ReadKeyRing(bytes.NewReader(raw))
	if err != nil || len(el) == 0 {
		return "", fmt.Errorf("read recovery PGP public key: %v", err)
	}
	return fmt.Sprintf("%x", el[0].PrimaryKey.Fingerprint), nil
}

// EncryptTo encrypts plaintext to a base64 binary public key.
func EncryptTo(publicB64 string, plaintext []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(publicB64)
	if err != nil {
		return nil, err
	}
	el, err := openpgp.ReadKeyRing(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, err := openpgp.Encrypt(&out, el, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// DecryptWith decrypts binary ciphertext with an armored private key.
func DecryptWith(privateArmored string, ciphertext []byte) ([]byte, error) {
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(privateArmored))
	if err != nil {
		return nil, err
	}
	md, err := openpgp.ReadMessage(bytes.NewReader(ciphertext), el, nil, nil)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(md.UnverifiedBody); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var (
	oidBasicConstraints = asn1.ObjectIdentifier{2, 5, 29, 19}
	oidKeyUsage         = asn1.ObjectIdentifier{2, 5, 29, 15}
	oidSubjectKeyID     = asn1.ObjectIdentifier{2, 5, 29, 14}
)

func parseCertPEM(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func randomSerial() (*big.Int, error) {
	s, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return s, nil
}

// ProjectStore is one escrow project read and written at project level
// (escrow.EscrowClient's BackupProjectArtifact/RestoreProjectArtifact).
type ProjectStore interface {
	BackupProjectArtifact(ctx context.Context, name, payload string) error
	RestoreProjectArtifact(ctx context.Context, name string) (string, error)
}

// ArtifactCustodyProjectID, in the BOX project, records which project is this
// box's custody project. It names a project and grants nothing.
const ArtifactCustodyProjectID = "custody-project-id"

// CustodyReport says what EstablishCustody found and what it created, so the
// command can report accurately.
type CustodyReport struct {
	RootCreated bool
	PGPCreated  bool
	Fingerprint string
}

// EstablishCustody makes sure the box's root CA and recovery PGP key exist:
// private halves in custody (administrator only), public halves in the box project
// (ADR-117 §3, §6). binding is the box's escrow project id, which the root's subject
// carries. Idempotent: existing material is kept, never regenerated, and a
// partial earlier run is completed from what custody already holds.
func EstablishCustody(ctx context.Context, box, custody ProjectStore, binding string, now time.Time) (CustodyReport, error) {
	var rep CustodyReport

	rawRoot, err := custody.RestoreProjectArtifact(ctx, ArtifactRootCA)
	if err != nil {
		return rep, fmt.Errorf("read the custody root: %w", err)
	}
	var root RootCA
	if strings.TrimSpace(rawRoot) == "" {
		if root, err = GenerateRootCA(binding, now); err != nil {
			return rep, err
		}
		payload, _ := json.Marshal(root)
		if err := custody.BackupProjectArtifact(ctx, ArtifactRootCA, string(payload)); err != nil {
			return rep, fmt.Errorf("store the root in custody: %w", err)
		}
		rep.RootCreated = true
	} else if err := json.Unmarshal([]byte(rawRoot), &root); err != nil {
		return rep, fmt.Errorf("the custody root is malformed: %w", err)
	}
	if _, _, err := ParseRoot(root, binding, now); err != nil {
		return rep, err
	}
	if err := box.BackupProjectArtifact(ctx, ArtifactRootCertificate, string(root.CertPEM)); err != nil {
		return rep, fmt.Errorf("publish the root certificate to the box escrow: %w", err)
	}

	rawPGP, err := custody.RestoreProjectArtifact(ctx, ArtifactRecoveryPGPPrivate)
	if err != nil {
		return rep, fmt.Errorf("read the custody recovery key: %w", err)
	}
	var k RecoveryPGP
	if strings.TrimSpace(rawPGP) == "" {
		if k, err = GenerateRecoveryPGP(binding); err != nil {
			return rep, err
		}
		payload, _ := json.Marshal(k)
		if err := custody.BackupProjectArtifact(ctx, ArtifactRecoveryPGPPrivate, string(payload)); err != nil {
			return rep, fmt.Errorf("store the recovery PGP key in custody: %w", err)
		}
		rep.PGPCreated = true
	} else if err := json.Unmarshal([]byte(rawPGP), &k); err != nil {
		return rep, fmt.Errorf("the custody recovery key is malformed: %w", err)
	}
	if err := box.BackupProjectArtifact(ctx, ArtifactRecoveryPGPPublic, k.PublicB64); err != nil {
		return rep, fmt.Errorf("publish the recovery PGP public key to the box escrow: %w", err)
	}
	rep.Fingerprint = k.Fingerprint
	return rep, nil
}
