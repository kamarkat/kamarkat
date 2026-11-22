package controller

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
	opsv1alpha1 "github.com/kamarkat/kamarkat/operators/hub-operator/api/v1alpha1"
)

type mockEscrowStore struct {
	artifacts map[string]string
	readErr   error
	writeErr  error
}

func newMockEscrowStore() *mockEscrowStore {
	return &mockEscrowStore{artifacts: make(map[string]string)}
}

func (m *mockEscrowStore) BackupArtifact(ctx context.Context, id escrow.Identity, artifact, payload string) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.artifacts[id.BoxID+"/"+id.ClusterName+"/"+artifact] = payload
	return nil
}

func (m *mockEscrowStore) RestoreArtifact(ctx context.Context, id escrow.Identity, artifact string) (string, error) {
	if m.readErr != nil {
		return "", m.readErr
	}
	return m.artifacts[id.BoxID+"/"+id.ClusterName+"/"+artifact], nil
}

func (m *mockEscrowStore) BackupMasterKeys(ctx context.Context, id escrow.Identity, keys interface{}) error {
	return nil
}

func (m *mockEscrowStore) RestoreMasterKeys(ctx context.Context, id escrow.Identity) (interface{}, error) {
	return nil, nil
}

func newSpokePoolTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := opsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add opsv1alpha1 to scheme: %v", err)
	}
	spokeGVK := schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "SpokePool",
	}
	s.AddKnownTypeWithName(spokeGVK, &unstructured.Unstructured{})
	listGVK := spokeGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func TestSpokePoolReconcilerEnsuresEncryptionSecret(t *testing.T) {
	ctx := context.Background()
	scheme := newSpokePoolTestScheme(t)

	hubEnv := &opsv1alpha1.HubEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hub-environment",
		},
		Spec: opsv1alpha1.HubEnvironmentSpec{
			BoxID:       "nutgraf-1794929668",
			ClusterName: "nutgraf-mgmt-1794929668",
		},
	}

	spokePool := &unstructured.Unstructured{}
	spokePool.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "SpokePool",
	})
	spokePool.SetName("nutgraf-stg-01")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(hubEnv, spokePool).
		Build()

	escrowStore := newMockEscrowStore()

	r := &SpokePoolReconciler{
		Client:       cl,
		EscrowClient: escrowStore,
	}

	// First reconcile: Secret is missing; it should generate key, escrow it, and create Secret in platform-capi.
	if err := r.ensureEncryptionSecret(ctx, spokePool); err != nil {
		t.Fatalf("ensureEncryptionSecret failed: %v", err)
	}

	// 1. Verify key was escrowed under BoxID and ClusterName
	keyInEscrow := escrowStore.artifacts["nutgraf-1794929668/nutgraf-stg-01/"+escrow.ArtifactSecretEncryptionKey]
	if keyInEscrow == "" {
		t.Fatal("expected encryption key to be saved in escrow, but was empty")
	}

	// 2. Verify Secret exists in platform-capi
	secName := assets.EncryptionSecretName("nutgraf-stg-01")
	var sec corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "platform-capi", Name: secName}, &sec); err != nil {
		t.Fatalf("expected secret platform-capi/%s to be created: %v", secName, err)
	}

	encYaml := string(sec.Data["enc.yaml"])
	if encYaml == "" && sec.StringData != nil {
		encYaml = sec.StringData["enc.yaml"]
	}
	rawKey, _ := hex.DecodeString(keyInEscrow)
	expectedB64 := base64.StdEncoding.EncodeToString(rawKey)
	if !strings.Contains(encYaml, expectedB64) {
		t.Errorf("enc.yaml does not contain expected base64 key: %s", expectedB64)
	}

	// Second reconcile: Secret already exists; it should verify matching key and succeed idempotently.
	if err := r.ensureEncryptionSecret(ctx, spokePool); err != nil {
		t.Fatalf("second ensureEncryptionSecret failed: %v", err)
	}
}

func TestSpokePoolReconcilerMismatchedSecretFailsClosed(t *testing.T) {
	ctx := context.Background()
	scheme := newSpokePoolTestScheme(t)

	hubEnv := &opsv1alpha1.HubEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hub-environment",
		},
		Spec: opsv1alpha1.HubEnvironmentSpec{
			BoxID:       "nutgraf-1794929668",
			ClusterName: "nutgraf-mgmt-1794929668",
		},
	}

	spokePool := &unstructured.Unstructured{}
	spokePool.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "nutgraf.in",
		Version: "v1alpha1",
		Kind:    "SpokePool",
	})
	spokePool.SetName("nutgraf-stg-01")

	escrowStore := newMockEscrowStore()
	rawA := make([]byte, 32)
	rawA[0] = 0xAA
	escrowStore.artifacts["nutgraf-1794929668/nutgraf-stg-01/"+escrow.ArtifactSecretEncryptionKey] = hex.EncodeToString(rawA)

	rawB := make([]byte, 32)
	rawB[0] = 0xBB
	b64B := base64.StdEncoding.EncodeToString(rawB)
	secName := assets.EncryptionSecretName("nutgraf-stg-01")
	existingSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "platform-capi",
			Name:      secName,
		},
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

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(hubEnv, spokePool, existingSec).
		Build()

	r := &SpokePoolReconciler{
		Client:       cl,
		EscrowClient: escrowStore,
	}

	err := r.ensureEncryptionSecret(ctx, spokePool)
	if err == nil {
		t.Fatal("expected failure on mismatched secret, but got nil")
	}
	if !strings.Contains(err.Error(), "does not correspond to the escrowed key") {
		t.Errorf("expected does not correspond to escrowed key error, got: %v", err)
	}
}
