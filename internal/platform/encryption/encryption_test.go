package encryption

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// fakeEscrowStore implements escrow.EscrowClient for testing.
type fakeEscrowStore struct {
	artifacts map[string]string
	readErr   error
	writeErr  error
}

func newFakeEscrowStore() *fakeEscrowStore {
	return &fakeEscrowStore{artifacts: make(map[string]string)}
}

func (f *fakeEscrowStore) BackupArtifact(ctx context.Context, id escrow.Identity, artifact, payload string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.artifacts[id.BoxID+"/"+id.ClusterName+"/"+artifact] = payload
	return nil
}

func (f *fakeEscrowStore) RestoreArtifact(ctx context.Context, id escrow.Identity, artifact string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.artifacts[id.BoxID+"/"+id.ClusterName+"/"+artifact], nil
}

func (f *fakeEscrowStore) BackupMasterKeys(ctx context.Context, id escrow.Identity, keys interface{}) error {
	return nil
}
func (f *fakeEscrowStore) RestoreMasterKeys(ctx context.Context, id escrow.Identity) (interface{}, error) {
	return nil, nil
}

// fakeSecretClient implements SecretClient for testing.
type fakeSecretClient struct {
	secrets map[string]*corev1.Secret
	getErr  error
}

func newFakeSecretClient() *fakeSecretClient {
	return &fakeSecretClient{secrets: make(map[string]*corev1.Secret)}
}

func (f *fakeSecretClient) GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	sec, ok := f.secrets[namespace+"/"+name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	return sec, nil
}

func (f *fakeSecretClient) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	f.secrets[secret.Namespace+"/"+secret.Name] = secret
	return nil
}

func TestEnsureClusterEncryptionFreshProvisioning(t *testing.T) {
	ctx := context.Background()
	store := newFakeEscrowStore()
	secClient := newFakeSecretClient()

	opts := EnsureOptions{
		BoxID:        "box-01",
		ClusterName:  "spoke-01",
		Namespace:    "platform-capi",
		EscrowClient: store,
		SecretClient: secClient,
	}

	res, err := EnsureClusterEncryption(ctx, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.KeyGenerated {
		t.Error("expected KeyGenerated to be true")
	}
	if !res.SecretCreated {
		t.Error("expected SecretCreated to be true")
	}

	// Verify key was saved to escrow
	keyInEscrow := store.artifacts["box-01/spoke-01/"+escrow.ArtifactSecretEncryptionKey]
	if keyInEscrow == "" {
		t.Fatal("expected key to be in escrow, but was empty")
	}
	if len(keyInEscrow) != 64 { // 32 hex bytes = 64 characters
		t.Errorf("expected 64 hex chars, got %d", len(keyInEscrow))
	}

	// Verify secret was created in platform-capi
	secName := assets.EncryptionSecretName("spoke-01")
	sec := secClient.secrets["platform-capi/"+secName]
	if sec == nil {
		t.Fatalf("expected secret platform-capi/%s to exist", secName)
	}

	encYaml := string(sec.Data["enc.yaml"])
	rawKey, _ := hex.DecodeString(keyInEscrow)
	expectedB64 := base64.StdEncoding.EncodeToString(rawKey)
	if !strings.Contains(encYaml, expectedB64) {
		t.Error("enc.yaml does not contain expected base64 key")
	}
}

func TestEnsureClusterEncryptionReuseExistingEscrowKey(t *testing.T) {
	ctx := context.Background()
	store := newFakeEscrowStore()
	secClient := newFakeSecretClient()

	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	preExistingKey := hex.EncodeToString(raw)
	store.artifacts["box-01/spoke-01/"+escrow.ArtifactSecretEncryptionKey] = preExistingKey

	opts := EnsureOptions{
		BoxID:        "box-01",
		ClusterName:  "spoke-01",
		Namespace:    "platform-capi",
		EscrowClient: store,
		SecretClient: secClient,
	}

	res, err := EnsureClusterEncryption(ctx, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.KeyRestored {
		t.Error("expected KeyRestored to be true")
	}
	if res.KeyGenerated {
		t.Error("expected KeyGenerated to be false")
	}
	if !res.SecretCreated {
		t.Error("expected SecretCreated to be true")
	}

	// Escrow key must remain unchanged
	if store.artifacts["box-01/spoke-01/"+escrow.ArtifactSecretEncryptionKey] != preExistingKey {
		t.Error("escrow key was mutated")
	}
}

func TestEnsureClusterEncryptionExistingSecretMatchesEscrow(t *testing.T) {
	ctx := context.Background()
	store := newFakeEscrowStore()
	secClient := newFakeSecretClient()

	// Provision first
	opts := EnsureOptions{
		BoxID:        "box-01",
		ClusterName:  "spoke-01",
		Namespace:    "platform-capi",
		EscrowClient: store,
		SecretClient: secClient,
	}

	_, err := EnsureClusterEncryption(ctx, opts)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}

	// Re-run second time: must recognize existing secret and verify without mutation
	res2, err := EnsureClusterEncryption(ctx, opts)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	if !res2.SecretAlreadyExisted {
		t.Error("expected SecretAlreadyExisted to be true")
	}
	if !res2.SecretVerified {
		t.Error("expected SecretVerified to be true")
	}
	if res2.SecretCreated {
		t.Error("expected SecretCreated to be false")
	}
}

func TestEnsureClusterEncryptionMismatchedSecretFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newFakeEscrowStore()
	secClient := newFakeSecretClient()

	// Set escrow key A
	rawA := make([]byte, 32)
	rawA[0] = 0xAA
	store.artifacts["box-01/spoke-01/"+escrow.ArtifactSecretEncryptionKey] = hex.EncodeToString(rawA)

	// Set Kubernetes Secret with key B
	rawB := make([]byte, 32)
	rawB[0] = 0xBB
	b64B := base64.StdEncoding.EncodeToString(rawB)
	secName := assets.EncryptionSecretName("spoke-01")
	secClient.secrets["platform-capi/"+secName] = &corev1.Secret{
		Data: map[string][]byte{
			"enc.yaml": []byte(fmt.Sprintf(`apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - secretbox:
          keys:
            - name: key1
              secret: %s
`, b64B)),
		},
	}

	opts := EnsureOptions{
		BoxID:        "box-01",
		ClusterName:  "spoke-01",
		Namespace:    "platform-capi",
		EscrowClient: store,
		SecretClient: secClient,
	}

	_, err := EnsureClusterEncryption(ctx, opts)
	if err == nil {
		t.Fatal("expected failure on mismatched existing secret, but got nil")
	}
	if !strings.Contains(err.Error(), "does not correspond to the escrowed key") {
		t.Errorf("expected mismatch error message, got: %v", err)
	}
}

func TestEnsureClusterEncryptionUnreadableEscrowFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newFakeEscrowStore()
	store.readErr = fmt.Errorf("escrow network unreachable")
	secClient := newFakeSecretClient()

	opts := EnsureOptions{
		BoxID:        "box-01",
		ClusterName:  "spoke-01",
		Namespace:    "platform-capi",
		EscrowClient: store,
		SecretClient: secClient,
	}

	_, err := EnsureClusterEncryption(ctx, opts)
	if err == nil {
		t.Fatal("expected error on unreadable escrow, got nil")
	}
	if !strings.Contains(err.Error(), "reading the escrow") {
		t.Errorf("expected reading the escrow error, got: %v", err)
	}
}

func (f *fakeEscrowStore) BackupProjectArtifact(context.Context, string, string) error { return nil }
func (f *fakeEscrowStore) RestoreProjectArtifact(context.Context, string) (string, error) { return "", nil }
