package controller

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/objectstore"
)

// objectStorageSecret is the box's object-storage credential, written at
// bootstrap from k8-secrets/s3 (the account the database backups use).
const (
	objectStorageSecretNamespace = "platform-ops"
	objectStorageSecretName      = "s3-object-storage"
)

// ensureLogsBucket makes sure cluster's Loki has its bucket (ADR-078 addendum 5
// §4). It never blocks the caller's reconcile: a cluster without its log bucket
// is degraded, not unprovisionable, so the outcome is logged and returned for the
// caller to log.
//
// OBJECT_STORAGE_ENDPOINT and OBJECT_STORAGE_REGION are required in the
// operator's environment; there is no default endpoint to fall back to.
func ensureLogsBucket(ctx context.Context, c client.Client, cluster string) error {
	if cluster == "" {
		return fmt.Errorf("no cluster name: cannot name its log bucket")
	}
	endpoint := os.Getenv("OBJECT_STORAGE_ENDPOINT")
	region := os.Getenv("OBJECT_STORAGE_REGION")
	if endpoint == "" || region == "" {
		return fmt.Errorf("OBJECT_STORAGE_ENDPOINT and OBJECT_STORAGE_REGION must be set to create log buckets")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: objectStorageSecretNamespace, Name: objectStorageSecretName}
	if err := c.Get(ctx, key, secret); err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	store, err := objectstore.New(endpoint, region,
		string(secret.Data["access-key-id"]), string(secret.Data["secret-access-key"]))
	if err != nil {
		return err
	}
	bucket := objectstore.LogsBucket(cluster)
	created, err := store.EnsureBucket(ctx, bucket)
	if err != nil {
		return err
	}
	if created {
		log.FromContext(ctx).Info("Created log bucket", "cluster", cluster, "bucket", bucket)
	}
	return nil
}
