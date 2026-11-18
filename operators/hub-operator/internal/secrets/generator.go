package secrets

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/kamarkat/kamarkat/internal/platform/escrow"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================================
// CRYPTOGRAPHIC UTILITIES
// ============================================================================

// GenerateSecurePassword generates a cryptographically secure 32-character hex password
// Uses hex encoding for maximum compatibility (no special characters)
func GenerateSecurePassword() (string, error) {
	bytes := make([]byte, 16) // 16 bytes = 32 hex characters
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// GenerateHexKey returns nbytes of cryptographic randomness, hex-encoded, giving a
// string of exactly 2*nbytes characters.
//
// Some consumers do not accept an arbitrary secret string: they hex-decode it and
// require an exact key length. agentgateway's OIDC cookie encoder is one — it calls
// hex::decode and demands 32 bytes for AES-256-GCM, so a 64-character URL-safe
// random string is the right LENGTH but the wrong alphabet, and it fails at startup
// with "Invalid character 'G' at position 0" rather than anything about encoding.
func GenerateHexKey(nbytes int) (string, error) {
	if nbytes <= 0 {
		return "", fmt.Errorf("key length must be positive")
	}
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateSecurePasswordWithCharset generates a cryptographically secure password
// using a custom character set. Avoids modulo bias by rejecting values that
// would cause non-uniform distribution.
func GenerateSecurePasswordWithCharset(length int, charset string) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("password length must be positive")
	}
	if len(charset) == 0 {
		return "", fmt.Errorf("charset cannot be empty")
	}

	password := make([]byte, length)
	charsetLen := len(charset)

	// Calculate the maximum valid random value to avoid modulo bias
	// We want: randomValue % charsetLen to be uniformly distributed
	maxValid := 256 - (256 % charsetLen)

	for i := range password {
		for {
			b := make([]byte, 1)
			if _, err := rand.Read(b); err != nil {
				return "", fmt.Errorf("failed to generate random bytes: %w", err)
			}
			randomValue := int(b[0])

			// Reject values that would cause modulo bias
			if randomValue < maxValid {
				password[i] = charset[randomValue%charsetLen]
				break
			}
			// If randomValue >= maxValid, retry to avoid bias
		}
	}

	return string(password), nil
}

// GenerateEd25519KeyPair generates an Ed25519 key pair for DKIM signing.
// Returns:
//   - privateKeyPEM: PKCS#8 PEM-encoded private key
//   - publicKeyBase64: raw base64-encoded public key (for DKIM p= DNS record)
//
// The public key is NOT PEM-wrapped — it's the exact bytes needed for:
//
//	v=DKIM1; k=ed25519; p=<publicKeyBase64>
func GenerateEd25519KeyPair() (privateKeyPEM string, publicKeyBase64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate Ed25519 key pair: %w", err)
	}

	// Marshal private key to PKCS#8 PEM
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal Ed25519 private key: %w", err)
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privBytes,
	})

	// Extract raw 32-byte public key and base64-encode it (DKIM p= format)
	pubB64 := base64.StdEncoding.EncodeToString([]byte(pub))

	return string(privPEM), pubB64, nil
}

// DeriveEd25519PublicKeyFromPEM extracts the raw base64-encoded public key from a
// PEM-encoded Ed25519 private key. Used for idempotency repair when the private key
// exists in Infisical but the public key is missing.
func DeriveEd25519PublicKeyFromPEM(privateKeyPEM string) (publicKeyBase64 string, err error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block from private key")
	}

	privKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse PKCS#8 private key: %w", err)
	}

	edPrivKey, ok := privKey.(ed25519.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not Ed25519")
	}

	pub := edPrivKey.Public().(ed25519.PublicKey)
	pubB64 := base64.StdEncoding.EncodeToString([]byte(pub))

	return pubB64, nil
}

// ValidateEd25519KeyPair verifies that a PEM-encoded private key and a base64-encoded
// public key form a consistent Ed25519 key pair. Returns nil if valid, error otherwise.
func ValidateEd25519KeyPair(privatePEM string, publicBase64 string) error {
	derived, err := DeriveEd25519PublicKeyFromPEM(privatePEM)
	if err != nil {
		return fmt.Errorf("cannot derive public key from private key: %w", err)
	}

	if derived != publicBase64 {
		return fmt.Errorf("key pair mismatch: derived public key does not match stored public key")
	}

	return nil
}

// GenerateInfisicalDBCredentials creates the infisical-db-credentials secret
// Required for Infisical bootstrap
func GenerateInfisicalDBCredentials(namespace string, owner metav1.OwnerReference) (*corev1.Secret, error) {
	password, err := GenerateSecurePassword()
	if err != nil {
		return nil, fmt.Errorf("failed to generate infisical-db-credentials password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "infisical-db-credentials",
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
			Labels: map[string]string{
				"ops.nutgraf.in/db-credentials": "true",
			},
		},
		// basic-auth rather than Opaque. CNPG 1.29 accepts an Opaque Secret here —
		// verified on a live cluster, where managed.roles reconciled the role and
		// applied the password from one — so this is about being semantically correct
		// and surviving a future CNPG that enforces the documented type, not about
		// fixing a present-day failure.
		//
		// Deliberately NOT accompanied by a migration for existing clusters: a
		// Secret's type is immutable, so converting one means delete-and-recreate,
		// which rotates a live database credential for no benefit while Opaque works.
		// New clusters get the right type; existing ones are left alone.
		Type: corev1.SecretTypeBasicAuth,
		StringData: map[string]string{
			corev1.BasicAuthUsernameKey: "infisical",
			corev1.BasicAuthPasswordKey: password,
		},
	}

	return secret, nil
}

// GenerateInfisicalSecretsResult contains both infisical-secrets and the extracted Redis password
type GenerateInfisicalSecretsResult struct {
	InfisicalSecrets *corev1.Secret
	RedisPassword    string
}

// GenerateInfisicalSecrets creates the infisical-secrets Kubernetes secret with backup/restore logic
// Required for Infisical bootstrap (encryption keys, Redis URL, DB cert)
// Returns both the secret and the Redis password for creating infisical-redis-credentials
// NOTE: This secret MUST be created in platform-security namespace where Infisical pods run
//
// Implements REQ-7: Backup/restore logic with bootstrap detection
// - First-time bootstrap: Generate new keys, validate, backup to the escrow
// - Restore scenario: Restore from the escrow, validate, reconstruct secret
// - Error scenario: Fail if backup missing and cluster already bootstrapped
//
// Implements REQ-7.1: Secret reconstruction logic
// - Reads existing Redis password from infisical-redis-credentials (if available)
// - Reads CA certificate from infisical-db-ca (passed as parameter)
// - Reconstructs complete infisical-secrets with all 4 fields
func GenerateInfisicalSecrets(ctx context.Context, securityNamespace, dataNamespace string, caCert []byte, owner metav1.OwnerReference, isFirstTime bool, escrowID escrow.Identity, escrowClient escrow.EscrowClient, existingRedisPassword string) (*GenerateInfisicalSecretsResult, error) {
	var encryptionKey, authSecret string
	var err error

	// No escrow, no master keys.
	//
	// The escrow was optional here: a nil client skipped the restore, fell into
	// the generate branch, skipped the backup below, and returned freshly minted
	// ENCRYPTION_KEY and AUTH_SECRET that existed nowhere but inside the cluster
	// they decrypt. Nothing reported it. These are the two values a box cannot
	// regenerate -- losing them loses every secret the platform holds -- so
	// generating them with nowhere to put a copy is refused rather than done
	// quietly.
	if escrowClient == nil {
		return nil, fmt.Errorf("no escrow: the Infisical master keys cannot be " +
			"generated without somewhere outside this cluster to keep them (ADR-076)")
	}
	if escrowID.BoxID == "" {
		return nil, fmt.Errorf("no cluster name: the master keys would be escrowed " +
			"under a name nothing reads back")
	}

	// Try to restore from the escrow first
	var backupData interface{}
	backupData, err = escrowClient.RestoreMasterKeys(ctx, escrowID)
	if err != nil {
		return nil, fmt.Errorf("failed to read the escrow: %w", err)
	}

	if backupData != nil {
		// Backup exists - restore both keys
		// Type assert to extract keys from backup (the escrow returns map[string]interface{})
		if backupMap, ok := backupData.(map[string]interface{}); ok {
			if ek, ok := backupMap["encryptionKey"].(string); ok {
				encryptionKey = ek
			} else {
				return nil, fmt.Errorf("backup data missing encryptionKey field")
			}
			if as, ok := backupMap["authSecret"].(string); ok {
				authSecret = as
			} else {
				return nil, fmt.Errorf("backup data missing authSecret field")
			}
		} else {
			return nil, fmt.Errorf("invalid backup data format")
		}

		// REQ-7.2: Validate restored keys before use
		if err := ValidateEncryptionKey(encryptionKey); err != nil {
			return nil, fmt.Errorf("restored ENCRYPTION_KEY failed validation: %w", err)
		}
		if err := ValidateAuthSecret(authSecret); err != nil {
			return nil, fmt.Errorf("restored AUTH_SECRET failed validation: %w", err)
		}
	} else if isFirstTime {
		// First-time bootstrap - generate new keys
		encryptionKey, err = GenerateSecurePassword()
		if err != nil {
			return nil, fmt.Errorf("failed to generate ENCRYPTION_KEY: %w", err)
		}

		authSecret, err = GenerateSecurePassword()
		if err != nil {
			return nil, fmt.Errorf("failed to generate AUTH_SECRET: %w", err)
		}

		// REQ-7.2: Validate generated keys
		if err := ValidateEncryptionKey(encryptionKey); err != nil {
			return nil, fmt.Errorf("generated ENCRYPTION_KEY failed validation: %w", err)
		}
		if err := ValidateAuthSecret(authSecret); err != nil {
			return nil, fmt.Errorf("generated AUTH_SECRET failed validation: %w", err)
		}

		// Escrowed before they are returned, so a key this box will rely on is
		// never handed back without a copy outside the cluster.
		backupKeys := map[string]interface{}{
			"encryptionKey": encryptionKey,
			"authSecret":    authSecret,
			"createdAt":     time.Now().UTC(),
			"clusterId":     escrowID.BoxID,
			"version":       "1",
		}
		if err := escrowClient.BackupMasterKeys(ctx, escrowID, backupKeys); err != nil {
			return nil, fmt.Errorf("escrow the master keys for %s: %w", escrowID.BoxID, err)
		}
	} else {
		// NOT first-time AND backup missing - CRITICAL ERROR
		return nil, fmt.Errorf("ENCRYPTION_KEY backup not found in AWS and cluster already bootstrapped. Manual intervention required")
	}

	// REQ-7.1: Reconstruct Redis password from existing secret or generate new
	// During restore scenario, preserve existing Redis password to avoid breaking connections
	var redisPassword string
	if existingRedisPassword != "" {
		// Reuse existing Redis password (restore scenario)
		redisPassword = existingRedisPassword
	} else {
		// Generate new Redis password (first-time bootstrap)
		redisPassword, err = GenerateSecurePassword()
		if err != nil {
			return nil, fmt.Errorf("failed to generate Redis password: %w", err)
		}
	}

	// Construct REDIS_URL - Redis runs in data namespace
	redisURL := fmt.Sprintf("redis://:%s@redis-master.%s.svc:6379", redisPassword, dataNamespace)

	// Base64-encode CA certificate (handle nil/empty gracefully)
	// CA is now managed by cert-manager per ADR-035; if not yet available, use empty string
	var dbRootCert string
	if len(caCert) > 0 {
		dbRootCert = base64.StdEncoding.EncodeToString(caCert)
	}

	// Create secret in security namespace where Infisical pods run
	// REQ-9: Add finalizer to prevent accidental deletion
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "infisical-secrets",
			Namespace:       securityNamespace,
			OwnerReferences: []metav1.OwnerReference{owner},
			Finalizers:      []string{EncryptionKeyProtectionFinalizer},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"ENCRYPTION_KEY": encryptionKey,
			"AUTH_SECRET":    authSecret,
			"REDIS_URL":      redisURL,
			"DB_ROOT_CERT":   dbRootCert,
		},
	}

	return &GenerateInfisicalSecretsResult{
		InfisicalSecrets: secret,
		RedisPassword:    redisPassword,
	}, nil
}

// GenerateInfisicalRedisCredentials creates the infisical-redis-credentials secret
// Required for Redis bootstrap
// REQ-9: Add finalizer to prevent accidental deletion
func GenerateInfisicalRedisCredentials(namespace, redisPassword string, owner metav1.OwnerReference) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "infisical-redis-credentials",
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
			Finalizers:      []string{EncryptionKeyProtectionFinalizer},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "hub-operator",
				"app.kubernetes.io/component":  "bootstrap-secret",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"password": redisPassword,
		},
	}

	return secret, nil
}

// GenerateInfisicalPostgresConnection creates the infisical-postgres-connection secret
// Required for Infisical to connect to PostgreSQL
// NOTE: This secret MUST be created in platform-security namespace where Infisical pods run
func GenerateInfisicalPostgresConnection(securityNamespace, dbHost, dbName, username, password string, owner metav1.OwnerReference) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "infisical-postgres-connection",
			Namespace:       securityNamespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"DB_HOST":     dbHost,
			"DB_PORT":     "5432",
			"DB_USER":     username,
			"DB_PASSWORD": password,
			"DB_NAME":     dbName,
			"DB_SSL_MODE": "require",
		},
	}

	return secret, nil
}

// ============================================================================
// BOOTSTRAP SECRETS ORCHESTRATION
// ============================================================================

// BootstrapSecretsResult contains bootstrap secrets required for infrastructure to start
// These secrets break circular dependencies: the bootstrap cluster's initdb needs
// infisical-db-credentials before Infisical can start, and every other credential is
// fetched FROM Infisical.
type BootstrapSecretsResult struct {
	InfisicalDBCredentials      *corev1.Secret
	InfisicalSecrets            *corev1.Secret
	InfisicalRedisCredentials   *corev1.Secret
	InfisicalPostgresConnection *corev1.Secret
}

// GenerateBootstrapSecrets orchestrates the generation of bootstrap secrets only
// Bootstrap secrets are required for infrastructure components to start (CNPG, Infisical, Redis)
// Application secrets (control-plane-db-credentials, hub-db-credentials, etc.) are created by ESO
// Implements idempotency by reusing existing passwords
// Sets ownerReferences on all secrets
//
// REQ-7: Implements backup/restore logic for Infisical master keys
// - Detects first-time bootstrap using HubEnvironment.Status.Conditions
// - Restores keys from AWS if backup exists
// - Generates and backs up new keys on first-time bootstrap
// - Fails gracefully if backup missing and cluster already bootstrapped
func GenerateBootstrapSecrets(ctx context.Context, dataNamespace, securityNamespace, dbHost string, owner metav1.OwnerReference, existingSecrets map[string]*corev1.Secret, isFirstTime bool, escrowID escrow.Identity, escrowClient escrow.EscrowClient) (*BootstrapSecretsResult, error) {
	result := &BootstrapSecretsResult{}

	// Step 1: Read infisical-db-ca CA certificate if available (in data namespace)
	// CA is now managed by cert-manager per ADR-035
	var caCert []byte
	if existing, ok := existingSecrets["infisical-db-ca"]; ok {
		caCert = existing.Data["ca.crt"]
	}
	// If not available yet, caCert remains nil.
	// GenerateInfisicalSecrets handles nil caCert gracefully by using empty string for DB_ROOT_CERT.

	// Step 2: Generate or reuse infisical-db-credentials (in data namespace)
	var infisicalUsername, infisicalPassword string
	if existing, ok := existingSecrets["infisical-db-credentials"]; ok {
		result.InfisicalDBCredentials = nil
		infisicalUsername = string(existing.Data["username"])
		infisicalPassword = string(existing.Data["password"])
	} else {
		infisicalDBCreds, err := GenerateInfisicalDBCredentials(dataNamespace, owner)
		if err != nil {
			return nil, fmt.Errorf("failed to generate infisical-db-credentials: %w", err)
		}
		result.InfisicalDBCredentials = infisicalDBCreds
		infisicalUsername = infisicalDBCreds.StringData["username"]
		infisicalPassword = infisicalDBCreds.StringData["password"]
	}

	// Step 4: Generate or restore infisical-secrets with backup/restore logic (in security namespace)
	var redisPassword string
	if _, ok := existingSecrets["infisical-secrets"]; ok {
		// Secret exists - check if we need to restore from AWS
		// This handles the case where secret was deleted and needs restoration
		result.InfisicalSecrets = nil

		// Reuse existing redis credentials if available
		if existing, ok := existingSecrets["infisical-redis-credentials"]; ok {
			redisPassword = string(existing.Data["password"])
			result.InfisicalRedisCredentials = nil
		}
	} else {
		// Secret doesn't exist - use backup/restore logic
		// REQ-7.1: Read existing Redis password to preserve during restore
		var existingRedisPassword string
		if existing, ok := existingSecrets["infisical-redis-credentials"]; ok {
			existingRedisPassword = string(existing.Data["password"])
		}

		infisicalResult, err := GenerateInfisicalSecrets(ctx, securityNamespace, dataNamespace, caCert, owner, isFirstTime, escrowID, escrowClient, existingRedisPassword)
		if err != nil {
			return nil, fmt.Errorf("failed to generate infisical-secrets: %w", err)
		}
		result.InfisicalSecrets = infisicalResult.InfisicalSecrets
		redisPassword = infisicalResult.RedisPassword

		// Generate infisical-redis-credentials using the Redis password (in data namespace)
		// Only create if it doesn't already exist
		if _, ok := existingSecrets["infisical-redis-credentials"]; !ok {
			redisSecret, err := GenerateInfisicalRedisCredentials(dataNamespace, redisPassword, owner)
			if err != nil {
				return nil, fmt.Errorf("failed to generate infisical-redis-credentials: %w", err)
			}
			result.InfisicalRedisCredentials = redisSecret
		}
	}

	// Step 5: Generate infisical-postgres-connection (in security namespace)
	// Always generate this as it depends on infisical-db-credentials
	infisicalPostgresConn, err := GenerateInfisicalPostgresConnection(
		securityNamespace,
		dbHost,
		"infisical",
		infisicalUsername,
		infisicalPassword,
		owner,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to generate infisical-postgres-connection: %w", err)
	}
	result.InfisicalPostgresConnection = infisicalPostgresConn

	return result, nil
}

// ============================================================================
// THE LOGIN SERVICE'S SIGNING KEYPAIR
// ============================================================================

// The material the identity provider's login service signs its system-API
// assertions with, and that the issuer verifies them against.
//
// MINTED HERE BECAUSE NOTHING ELSE CAN. `login.loginServiceKeySecretName` names a
// Secret the platform owns, which is what stops the chart minting a fresh pair on
// every ArgoCD render -- its reuse branch is guarded by Helm's `lookup`, which is
// always empty under `helm template` (five hours of AUTH-7fs1e on 2026-10-30,
// because the issuer reads SystemAPIUsers once at startup and the login service
// had moved on). Naming the Secret left a clean box with no producer at all: the
// ExternalSecret reads two Infisical keys, and the only thing that ever wrote them
// was an upload of the Secret the chart no longer renders.
//
// The SHAPE is the chart's, deliberately: RSA 2048, a self-signed certificate
// whose subject and only DNS name are "login-service", valid for ten years, and a
// PKCS#1 private key. Zitadel takes the public key from the certificate and the
// login service signs RS256 with the key (apps/login/src/lib/server/api.ts hands
// the PEM to newSystemToken, whose own tests cover `BEGIN RSA PRIVATE KEY`), so
// the encoding is part of the contract and not a detail.
//
// Nothing validates the name: this is an assertion-signing credential, not a TLS
// server identity. The ten years are what make a FROZEN pair safe to keep -- a
// renewal the login service adopts before the issuer restarts fails every
// assertion in between, which is the incident above.
const (
	loginServiceCommonName   = "login-service"
	loginServiceKeyBits      = 2048
	loginServiceCertValidFor = 3650 * 24 * time.Hour
)

// GenerateLoginServiceKeyPair mints a new pair.
func GenerateLoginServiceKeyPair() (certPEM string, keyPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, loginServiceKeyBits)
	if err != nil {
		return "", "", fmt.Errorf("generate the login service's RSA key: %w", err)
	}
	certPEM, err = selfSignLoginServiceCertificate(key)
	if err != nil {
		return "", "", err
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	return certPEM, keyPEM, nil
}

// ReissueLoginServiceCertificate issues a new certificate over an EXISTING key.
//
// The repair for a stored key whose certificate is gone. It is a repair rather
// than a regeneration because the certificate only carries the key's public half:
// re-issuing over the same key produces a certificate the running issuer already
// trusts, while minting a new pair would not.
func ReissueLoginServiceCertificate(keyPEM string) (string, error) {
	key, err := parseLoginServiceKey(keyPEM)
	if err != nil {
		return "", err
	}
	return selfSignLoginServiceCertificate(key)
}

// ValidateLoginServiceKeyPair reports whether a stored certificate and key are
// each other's.
//
// A pair that does not match cannot be repaired by regenerating either half: the
// certificate the issuer trusts and the key the login service signs with have
// already diverged, and only replacing both together is coherent.
func ValidateLoginServiceKeyPair(certPEM string, keyPEM string) error {
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		return fmt.Errorf("the stored login service certificate and private key are not a pair: %w", err)
	}
	return nil
}

// parseLoginServiceKey accepts either PEM encoding of an RSA key.
//
// PKCS#1 is what this mints and what the chart minted before it; PKCS#8 is
// accepted so a pair stored by anything else is read rather than refused. A key
// that is not RSA IS refused: the assertions are RS256.
func parseLoginServiceKey(keyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, fmt.Errorf("the stored login service private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the stored login service private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the stored login service private key is %T, and the assertions "+
			"it signs are RS256", parsed)
	}
	return key, nil
}

func selfSignLoginServiceCertificate(key *rsa.PrivateKey) (string, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", fmt.Errorf("draw a serial number for the login service certificate: %w", err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: loginServiceCommonName},
		DNSNames:     []string{loginServiceCommonName},
		// An hour of slack, because a certificate that is not yet valid on a node
		// whose clock runs slightly behind fails exactly like a wrong one.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(loginServiceCertValidFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", fmt.Errorf("self-sign the login service certificate: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}
