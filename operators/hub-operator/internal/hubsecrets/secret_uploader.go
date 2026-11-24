package hubsecrets

import (
	"context"
	"fmt"
	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SecretUploader handles uploading secrets to Infisical
// Separation of Concerns: This class only uploads to Infisical, does not create K8s secrets
type SecretUploader struct {
	store             *secrets.Store
	k8sClient         client.Client
	uncachedK8sClient client.Client
}

// NewSecretUploader creates a new secret uploader
func NewSecretUploader(store *secrets.Store, k8sClient, uncachedK8sClient client.Client) *SecretUploader {
	return &SecretUploader{
		store:             store,
		k8sClient:         k8sClient,
		uncachedK8sClient: uncachedK8sClient,
	}
}

// UploadCLISecrets uploads all CLI-injected and bootstrap secrets to Infisical
// This makes Infisical the Source of Truth for all secrets
func (su *SecretUploader) UploadCLISecrets(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Create Infisical client using the infisical-auth secret
	// Use uncached client to read secret data (cached client strips data)

	logger.Info("Writing Day-0 secrets into OpenBao", "scope", secrets.HubScope)

	// Upload all configured secrets from the registry
	// See secret_mappings.go for the complete list
	mappings := CLISecretMappings
	successCount := 0
	var failedKeys []string

	for _, mapping := range mappings {
		err := su.uploadSecret(ctx, secrets.HubScope, mapping)
		if err != nil {
			logger.Error(err, "Failed to upload secret",
				"description", mapping.Description,
				"source", fmt.Sprintf("%s/%s", mapping.SourceNamespace, mapping.SourceName),
				"infisicalKey", mapping.Key)
			failedKeys = append(failedKeys, mapping.Key)
		} else {
			successCount++
			logger.Info("Uploaded secret to Infisical",
				"description", mapping.Description,
				"infisicalKey", mapping.Key)
		}
	}

	logger.Info("CLI secrets upload complete", "uploaded", successCount, "total", len(mappings), "failed", len(failedKeys))

	// ANY FAILURE IS FATAL, NOT JUST TOTAL FAILURE.
	//
	// This used to return nil whenever a single mapping succeeded, so one key
	// failing among twenty was a reconcile that reported success. The log carried an
	// Error line and nothing else did -- and a key that is missing from Infisical
	// does not announce itself later: its ExternalSecret simply never syncs, and the
	// component waiting on it fails for its own reasons somewhere else.
	//
	// zitadel_smtp_password made that concrete and is the most likely mapping to
	// fail: it is the only one that needs TENANT_ID and the only one that creates a
	// folder. resolveInfisicalPath refuses to fall back to "/" precisely so the
	// misconfiguration is loud, which it is not if the caller discards the error.
	//
	// A credential that is simply ABSENT on this box is not a failure -- uploadSecret
	// returns nil for a source Secret that does not exist -- so this fails on
	// malformed, empty, untransformable, unwritable or misrouted values, which are
	// all misconfigurations rather than optional inputs. Same reasoning as
	// escrowFailures in application_secret_uploader.go, same defect.
	if len(failedKeys) > 0 {
		if successCount == 0 {
			return fmt.Errorf("all %d secret uploads failed: %v", len(mappings), failedKeys)
		}
		return fmt.Errorf("%d of %d secret uploads failed and the rest succeeded, so this is not a "+
			"partial success: %v", len(failedKeys), len(mappings), failedKeys)
	}

	return nil
}

// uploadSecret uploads a single secret to Infisical based on the mapping configuration
func (su *SecretUploader) uploadSecret(ctx context.Context, scope string, mapping SecretMapping) error {
	logger := log.FromContext(ctx)

	// Read source secret from K8s
	// Use uncached client to read secret data (cached client strips data)
	secret := &corev1.Secret{}
	if err := su.uncachedK8sClient.Get(ctx, client.ObjectKey{
		Name:      mapping.SourceName,
		Namespace: mapping.SourceNamespace,
	}, secret); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Source secret not found, skipping",
				"secret", fmt.Sprintf("%s/%s", mapping.SourceNamespace, mapping.SourceName))
			return nil
		}
		return fmt.Errorf("failed to get source secret: %w", err)
	}

	// Extract the specified key
	value, ok := secret.Data[mapping.SourceKey]
	if !ok {
		return fmt.Errorf("source secret missing key %s", mapping.SourceKey)
	}

	if len(value) == 0 {
		if mapping.AllowEmpty {
			logger.Info("Source secret key is empty (inert capability, ADR-075), skipping upload",
				"source", fmt.Sprintf("%s/%s", mapping.SourceNamespace, mapping.SourceName),
				"key", mapping.SourceKey)
			return nil
		}
		return fmt.Errorf("source secret key %s is empty", mapping.SourceKey)
	}

	value, err := applyTransform(mapping.Transform, value)
	if err != nil {
		return fmt.Errorf("source secret key %s: %w", mapping.SourceKey, err)
	}

	// Upload to Infisical
	if err := su.store.UpdateSecret(ctx, scope, mapping.Key, string(value)); err != nil {
		return fmt.Errorf("failed to write to OpenBao: %w", err)
	}

	return nil
}

// applyTransform derives the uploaded value. Errors never include the value.
func applyTransform(t ValueTransform, value []byte) ([]byte, error) {
	switch t {
	case TransformNone:
		return value, nil
	case TransformURLPassword:
		u, err := url.Parse(strings.TrimSpace(string(value)))
		if err != nil || u.User == nil {
			return nil, fmt.Errorf("is not a URL with credentials (expected scheme://user:password@host)")
		}
		pw, ok := u.User.Password()
		if !ok || pw == "" {
			return nil, fmt.Errorf("URL carries no password")
		}
		return []byte(pw), nil
	default:
		return nil, fmt.Errorf("unknown transform %q", t)
	}
}
