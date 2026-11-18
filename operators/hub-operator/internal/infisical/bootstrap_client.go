package infisical

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// BootstrapClient orchestrates Infisical Day 0 initialization
type BootstrapClient struct {
	k8sClient         client.Client
	uncachedK8sClient client.Client
	api               *BootstrapAPI
}

// NewBootstrapClient creates a new bootstrap client
func NewBootstrapClient(k8sClient, uncachedK8sClient client.Client) *BootstrapClient {
	return &BootstrapClient{
		k8sClient:         k8sClient,
		uncachedK8sClient: uncachedK8sClient,
		api:               NewBootstrapAPI(),
	}
}

// Bootstrap reports whether the CLI has completed Day-0 bootstrap.
//
// It only looks. Day-0 is exclusively the CLI's responsibility (ADR-021) and the
// operator must never create the Infisical organisation, projects or master
// Machine Identity -- so this returns an error or nothing, where it used to
// return a bool that said whether IT had bootstrapped. That bool was always
// false, and the caller still logged "Infisical bootstrapped successfully" on
// the branch it could never take.
func (bc *BootstrapClient) Bootstrap(ctx context.Context) error {
	logger := log.FromContext(ctx)

	infisicalAuth := &corev1.Secret{}
	err := bc.k8sClient.Get(ctx, client.ObjectKey{
		Name:      SecretInfisicalAuth,
		Namespace: NamespaceOps,
	}, infisicalAuth)

	if err == nil {
		logger.Info("infisical-auth secret exists, bootstrap already completed by CLI")
		return nil
	}

	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check infisical-auth secret: %w", err)
	}

	// infisical-auth secret not found — the CLI has not run yet.
	// The operator must wait for the CLI to complete Day-0 bootstrap.
	logger.Info("infisical-auth secret not found — waiting for CLI Day-0 bootstrap (hub init-secrets)")
	return nil
}

// createInfisicalAdminSecret creates the infisical-admin secret for future admin operations
func (bc *BootstrapClient) createInfisicalAdminSecret(ctx context.Context, adminToken, orgID, projectID, projectSlug string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretInfisicalAdmin,
			Namespace: NamespaceOps,
			Labels: map[string]string{
				LabelManagedBy: ValueManagedBy,
				LabelComponent: ValueBootstrap,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			KeyAdminToken:  adminToken,
			KeyAdminEmail:  AdminEmail,
			KeyOrgID:       orgID,
			KeyProjectID:   projectID,
			KeyProjectSlug: projectSlug,
		},
	}

	if err := bc.k8sClient.Create(ctx, secret); err != nil {
		return fmt.Errorf("failed to create secret: %w", err)
	}

	return nil
}
