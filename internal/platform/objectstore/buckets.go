// Package objectstore ensures the object-storage buckets the platform's stores
// write to exist.
//
// Nothing that writes to object storage creates its own bucket, and a missing
// bucket is not a configuration error any of them reports at start: Loki fails at
// the first flush and CloudNativePG at the first WAL archive, minutes later and
// far from the cause.
//
// Two callers, which is why this package is here rather than inside one of them:
// `kakt box init` creates the box's backup bucket while it holds the credential
// the tenant just handed over, and hub-operator creates each cluster's log bucket
// on reconcile (ADR-078 addendum 5 §4: one log bucket per cluster). It moved out
// of operators/hub-operator/internal/ for that reason -- an internal package is
// importable only inside the tree that declares it, so the CLI could not have
// reused it and would have grown a second S3 client.
package objectstore

import (
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Client is an S3-compatible endpoint with the box's credentials.
type Client struct {
	s3     *minio.Client
	region string
}

// New connects to endpoint (a host, without scheme) over TLS.
func New(endpoint, region, accessKeyID, secretAccessKey string) (*Client, error) {
	if endpoint == "" || region == "" || accessKeyID == "" || secretAccessKey == "" {
		return nil, fmt.Errorf("object storage needs an endpoint, a region and both keys; one is empty")
	}
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: true,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("object storage client for %s: %w", endpoint, err)
	}
	return &Client{s3: c, region: region}, nil
}

// EnsureBucket creates name when it does not exist. It reports whether it
// created it. A bucket that exists is left exactly as it is.
func (c *Client) EnsureBucket(ctx context.Context, name string) (bool, error) {
	exists, err := c.s3.BucketExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("checking bucket %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	if err := c.s3.MakeBucket(ctx, name, minio.MakeBucketOptions{Region: c.region}); err != nil {
		return false, fmt.Errorf("creating bucket %s: %w", name, err)
	}
	return true, nil
}

// LogsBucket is the name of a cluster's log bucket. Loki's configuration names
// the same bucket (templated-fields: `<cluster>-logs`); the two must agree.
func LogsBucket(cluster string) string {
	return cluster + "-logs"
}

// BackupBucket is the name of a BOX's management-state backup bucket.
//
// The box, not the cluster: a box outlives every management cluster it has
// (ADR-111 §1), and its successors restore from what its predecessors wrote. One
// bucket per box, with each cluster's archive under its own prefix inside it --
// which is what ADR-082 already keys the WAL archive on, so the prefix is the
// thing that must be unique and the bucket is the thing that must persist.
//
// It replaces the literal `hub-db-backups`, which was one bucket for every box
// that ever ran: two boxes sharing it wrote into the same prefix namespace, and
// barman refuses a non-empty archive it does not recognise -- which is how a
// second box could find its first backup already failed.
func BackupBucket(boxID string) string {
	return boxID
}

// BackupPrefix is where one cluster's archive lives inside its box's bucket.
func BackupPrefix(boxID, cluster string) string {
	return "s3://" + BackupBucket(boxID) + "/" + cluster + "/"
}

// Endpoint is the object-storage host for a region.
//
// Derived rather than configured: the platform's databases are given
// `endpointURL: https://<region>.your-objectstorage.com` by their own manifests,
// and a second place to state it is a second place for it to disagree.
func Endpoint(region string) string {
	return region + ".your-objectstorage.com"
}
