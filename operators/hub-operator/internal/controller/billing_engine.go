package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// The billing engine is a selectable platform capability of the box (ADR-113),
// run on the hub and turned on by capabilities.billing.enabled. It is a
// platform capability with one caller (platform_capability.go); this is what
// is its own.
//
// Its database, role and credential are platform prerequisites present on
// every box (billing-db, the application secret mappings). Its own keys are
// generated here, once, when it is first enabled.
var billingEngine = platformCapability{
	name:              "billing",
	title:             "the billing engine",
	namespace:         "platform-billing",
	marker:            "billing-engine",
	projectIDKey:      "billing-oidc-project-id",
	callerClientIDKey: "billing-caller-client-id",
	callerOrgIDKey:    "billing-caller-org-id",
	ownerCondition:    "BillingEngineOwner",
	controllerName:    "billingengine",
	provision:         ensureBillingEngineKeys,
}

// billingEngineKeys are the engine's own keys (billing-engine-keys in its
// namespace), each with how it is generated. The engine encrypts its records
// with three of them and signs its notifications with the RSA key, so none is
// ever rotated: a key that exists is left exactly as it is.
var billingEngineKeys = []struct {
	key      string
	generate func(context.Context, TenantSecretStore) (string, error)
}{
	{"billing-engine-secret-key-base", hexKey(64)},
	{"billing-engine-encryption-primary-key", hexKey(32)},
	{"billing-engine-encryption-deterministic-key", hexKey(32)},
	{"billing-engine-encryption-key-derivation-salt", hexKey(32)},
	// The organisation's API key: only the engine's gateway presents it.
	{"billing-engine-org-api-key", hexKey(32)},
	// The admin user the engine requires; nothing signs in as it.
	{"billing-engine-org-user-password", hexKey(16)},
	{"billing-engine-rsa-private-key", rsaPrivateKey},
}

func hexKey(nbytes int) func(context.Context, TenantSecretStore) (string, error) {
	return func(ctx context.Context, s TenantSecretStore) (string, error) { return s.HexKey(ctx, nbytes) }
}

// rsaPrivateKey is a 2048-bit RSA key as the engine reads it: base64 of its
// PKCS#1 PEM.
func rsaPrivateKey(context.Context, TenantSecretStore) (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return base64.StdEncoding.EncodeToString(block), nil
}

// ensureBillingEngineKeys creates each of the engine's keys that does not exist.
func ensureBillingEngineKeys(ctx context.Context, r *AINativeSaaSReconciler) error {
	for _, k := range billingEngineKeys {
		exists, err := r.Store.SecretExists(ctx, hubSecretPath, k.key)
		if err != nil {
			return fmt.Errorf("check %s: %w", k.key, err)
		}
		if exists {
			continue
		}
		value, err := k.generate(ctx, r.Store)
		if err != nil {
			return fmt.Errorf("generate %s: %w", k.key, err)
		}
		if err := r.Store.CreateSecret(ctx, hubSecretPath, k.key, value); err != nil {
			return fmt.Errorf("create %s: %w", k.key, err)
		}
	}
	return nil
}
