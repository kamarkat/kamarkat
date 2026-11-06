package infisical

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infisicalclient "github.com/kamarkat/kamarkat/operators/hub-operator/internal/client"
)

// SecretUploader handles uploading secrets to Infisical
// Separation of Concerns: This class only uploads to Infisical, does not create K8s secrets
type SecretUploader struct {
	k8sClient         client.Client
	uncachedK8sClient client.Client
}

// NewSecretUploader creates a new secret uploader
func NewSecretUploader(k8sClient, uncachedK8sClient client.Client) *SecretUploader {
	return &SecretUploader{
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
	infisicalClient, err := infisicalclient.NewInfisicalClient(ctx, su.uncachedK8sClient, "")
	if err != nil {
		return fmt.Errorf("failed to create Infisical client: %w", err)
	}

	// Get project slug from environment variable (set via configmap hub-bootstrap-config)
	// Use secrets project for secret operations (hub-secrets is type "secret-manager")
	// Fall back to INFISICAL_PROJECT_SLUG for backward compatibility
	projectSlug := os.Getenv("INFISICAL_SECRETS_PROJECT_SLUG")
	if projectSlug == "" {
		projectSlug = os.Getenv("INFISICAL_PROJECT_SLUG")
	}
	if projectSlug == "" {
		return fmt.Errorf("neither INFISICAL_SECRETS_PROJECT_SLUG nor INFISICAL_PROJECT_SLUG environment variable is set")
	}

	environmentSlug, err := managementEnvironmentSlug()
	if err != nil {
		return err
	}
	tenantID := os.Getenv("TENANT_ID")

	logger.Info("Uploading CLI secrets to Infisical", "project", projectSlug, "environment", environmentSlug)

	// Upload all configured secrets from the registry
	// See secret_mappings.go for the complete list
	mappings := CLISecretMappings
	successCount := 0
	var failedKeys []string

	for _, mapping := range mappings {
		secretPath, err := resolveInfisicalPath(mapping.InfisicalPath, tenantID)
		if err == nil && secretPath != "/" {
			err = infisicalClient.EnsureFolderRaw(ctx, projectSlug, environmentSlug, secretPath)
		}
		if err == nil {
			err = su.uploadSecret(ctx, infisicalClient, projectSlug, environmentSlug, secretPath, mapping)
		}
		if err != nil {
			logger.Error(err, "Failed to upload secret",
				"description", mapping.Description,
				"source", fmt.Sprintf("%s/%s", mapping.SourceNamespace, mapping.SourceName),
				"infisicalKey", mapping.InfisicalKey)
			failedKeys = append(failedKeys, mapping.InfisicalKey)
		} else {
			successCount++
			logger.Info("Uploaded secret to Infisical",
				"description", mapping.Description,
				"infisicalKey", mapping.InfisicalKey)
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
func (su *SecretUploader) uploadSecret(ctx context.Context, infisicalClient *infisicalclient.InfisicalClient, projectSlug, environmentSlug, secretPath string, mapping SecretMapping) error {
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
		return fmt.Errorf("source secret key %s is empty", mapping.SourceKey)
	}

	value, err := applyTransform(mapping.Transform, value)
	if err != nil {
		return fmt.Errorf("source secret key %s: %w", mapping.SourceKey, err)
	}

	// Upload to Infisical
	if err := infisicalClient.CreateOrUpdateSecretRaw(ctx, projectSlug, environmentSlug, secretPath, mapping.InfisicalKey, string(value)); err != nil {
		return fmt.Errorf("failed to upload to Infisical: %w", err)
	}

	return nil
}

// resolveInfisicalPath returns the folder a mapping writes to: "/" when unset,
// with TenantPlaceholder replaced by the box's tenant id. A path that needs the
// tenant id when none is configured is an error, not a fallback to "/": writing
// the owner's credential to the project root would put it where its consumer
// never reads it, and the mail would fail silently.
func resolveInfisicalPath(p, tenantID string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if strings.Contains(p, TenantPlaceholder) {
		if tenantID == "" {
			return "", fmt.Errorf("path %s needs the tenant id, and TENANT_ID is not set (hub-bootstrap-config)", p)
		}
		p = strings.ReplaceAll(p, TenantPlaceholder, tenantID)
	}
	return p, nil
}

// managementEnvironmentSlug is the Infisical environment the HUB's own secrets
// live in -- every mapping in CLISecretMappings and ApplicationSecretMappings,
// all of which are read from a hub namespace and consumed by a hub component.
//
// Read from the same variable the readers use, so writer and reader cannot
// disagree: hub-bootstrap-config carries INFISICAL_ENVIRONMENT_SLUG, rendered from
// the box's declared `management.infisicalEnvironmentSlug`. It is required, with no default,
// because the only possible default is a guess at which environment someone
// else's secrets are in -- and guessing wrong is silent, as the comment in
// constants.go records.
//
// This is deliberately NOT the environment of a workload cluster or of an
// application. Those have their own authorities -- a spoke's own `environment`
// label, and the slug the tenant names in `kakt fleet secrets set <env> …` --
// and nothing here may stand in for either.
func managementEnvironmentSlug() (string, error) {
	slug := strings.TrimSpace(os.Getenv("INFISICAL_ENVIRONMENT_SLUG"))
	if slug == "" {
		return "", fmt.Errorf("INFISICAL_ENVIRONMENT_SLUG is not set: the hub's own " +
			"secrets have no environment to be written to, and defaulting would write them " +
			"where their ExternalSecrets do not read (hub-bootstrap-config's " +
			"INFISICAL_ENVIRONMENT_SLUG, rendered from management.infisicalEnvironmentSlug)")
	}
	return slug, nil
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
