package encryption

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/platform/escrow"
)

// SecretClient is the interface needed to inspect and create Kubernetes Secrets.
type SecretClient interface {
	GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error)
	CreateSecret(ctx context.Context, secret *corev1.Secret) error
}

// KubeSecretClient implements SecretClient using client-go kubernetes.Interface.
type KubeSecretClient struct {
	Clientset kubernetes.Interface
}

func (k KubeSecretClient) GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return k.Clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k KubeSecretClient) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	_, err := k.Clientset.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	return err
}

// CtrlSecretClient implements SecretClient using controller-runtime client.Client.
type CtrlSecretClient struct {
	Client client.Client
}

func (c CtrlSecretClient) GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	var sec corev1.Secret
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec); err != nil {
		return nil, err
	}
	return &sec, nil
}

func (c CtrlSecretClient) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	return c.Client.Create(ctx, secret)
}

// EnsureOptions defines parameters for provisioning cluster encryption.
type EnsureOptions struct {
	BoxID               string
	ClusterName         string
	Namespace           string
	NoPlaintextFallback bool
	DryRun              bool
	EscrowClient        escrow.EscrowClient
	SecretClient        SecretClient
}

// EnsureResult summarizes the outcome of the idempotent provisioning.
type EnsureResult struct {
	KeyRestored         bool
	KeyGenerated        bool
	SecretCreated       bool
	SecretAlreadyExisted bool
	SecretVerified      bool
}

// encryptionConfigSchema matches the providers block in enc.yaml.
type encryptionConfigSchema struct {
	Resources []struct {
		Providers []struct {
			Secretbox *struct {
				Keys []struct {
					Name   string `yaml:"name"`
					Secret string `yaml:"secret"`
				} `yaml:"keys"`
			} `yaml:"secretbox"`
		} `yaml:"providers"`
	} `yaml:"resources"`
}

// EnsureClusterEncryption idempotently ensures that:
// 1. A cluster-scoped encryption key exists in the escrow (restored if present, generated if absent).
// 2. Newly generated key material is durably persisted to the escrow BEFORE Kubernetes delivery.
// 3. The encryption configuration Secret exists in platform-capi.
// 4. If the Secret already exists, it is verified to match the escrowed key without exposure.
// 5. Mismatched or unreadable existing keys fail closed without overwrite.
func EnsureClusterEncryption(ctx context.Context, opts EnsureOptions) (*EnsureResult, error) {
	if opts.BoxID == "" {
		return nil, fmt.Errorf("ensure encryption: boxID is required")
	}
	if opts.ClusterName == "" {
		return nil, fmt.Errorf("ensure encryption: clusterName is required")
	}
	if opts.EscrowClient == nil {
		return nil, fmt.Errorf("ensure encryption: escrow client is required")
	}
	if opts.Namespace == "" {
		opts.Namespace = "platform-capi"
	}

	escrowScope := escrow.ForCluster(opts.BoxID, opts.ClusterName)

	// Step 1: Restore existing key or fail if escrow is unreadable.
	key, err := opts.EscrowClient.RestoreArtifact(ctx, escrowScope, escrow.ArtifactSecretEncryptionKey)
	if err != nil {
		// Unreadable is not absent. Treating it as absent would mint a second key for
		// a cluster that already has one, leading to irrecoverable data loss.
		return nil, fmt.Errorf("reading the escrow for cluster %q in box %q: %w", opts.ClusterName, opts.BoxID, err)
	}

	generated := false
	if key == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generating the encryption key: %w", err)
		}
		key = hex.EncodeToString(raw)
		generated = true
	}

	rawKey, err := hex.DecodeString(key)
	if err != nil || len(rawKey) != 32 {
		return nil, fmt.Errorf("the encryption key must be 32 hex-encoded bytes, got %d (decode error: %v); "+
			"a key of any other length is refused by the API server at start", len(rawKey), err)
	}

	// Step 2: Persist newly generated key material to escrow BEFORE Kubernetes delivery.
	if generated {
		if !opts.DryRun {
			if err := opts.EscrowClient.BackupArtifact(ctx, escrowScope, escrow.ArtifactSecretEncryptionKey, key); err != nil {
				return nil, fmt.Errorf("the encryption key was generated but could not be escrowed for cluster %q: %w", opts.ClusterName, err)
			}
		}
	}

	secretName := assets.EncryptionSecretName(opts.ClusterName)
	expectedB64 := base64.StdEncoding.EncodeToString(rawKey)

	// Step 3: Check if the Secret already exists.
	if opts.SecretClient != nil {
		existing, err := opts.SecretClient.GetSecret(ctx, opts.Namespace, secretName)
		if err == nil && existing != nil {
			// Secret exists: verify it corresponds to the escrowed key without exposing contents.
			rawEncYaml, ok := existing.Data["enc.yaml"]
			if !ok && existing.StringData != nil {
				if s, okString := existing.StringData["enc.yaml"]; okString {
					rawEncYaml = []byte(s)
					ok = true
				}
			}
			if !ok || len(rawEncYaml) == 0 {
				return nil, fmt.Errorf("existing encryption secret %s/%s contains no enc.yaml data; refusing to overwrite",
					opts.Namespace, secretName)
			}

			var parsedConfig encryptionConfigSchema
			if err := yaml.Unmarshal(rawEncYaml, &parsedConfig); err != nil {
				return nil, fmt.Errorf("parsing existing encryption secret %s/%s: %w", opts.Namespace, secretName, err)
			}

			matched := false
			for _, res := range parsedConfig.Resources {
				for _, prov := range res.Providers {
					if prov.Secretbox != nil {
						for _, k := range prov.Secretbox.Keys {
							if subtle.ConstantTimeCompare([]byte(k.Secret), []byte(expectedB64)) == 1 {
								matched = true
								break
							}
						}
					}
					if matched {
						break
					}
				}
				if matched {
					break
				}
			}

			if !matched {
				return nil, fmt.Errorf("existing encryption secret %s/%s does not correspond to the escrowed key for cluster %q; refusing to overwrite",
					opts.Namespace, secretName, opts.ClusterName)
			}

			return &EnsureResult{
				KeyRestored:          !generated,
				KeyGenerated:         generated,
				SecretAlreadyExisted: true,
				SecretVerified:       true,
			}, nil
		} else if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("inspecting existing encryption secret %s/%s: %w", opts.Namespace, secretName, err)
		}
	}

	// Step 4: Secret does not exist; render and create it.
	tmplData, err := assets.ReadManifest("secrets/secret-encryption-config.yaml")
	if err != nil {
		return nil, fmt.Errorf("reading secret-encryption-config template: %w", err)
	}

	tmpl, err := template.New("enc").Parse(string(tmplData))
	if err != nil {
		return nil, fmt.Errorf("parsing secret-encryption-config template: %w", err)
	}

	var secretOut bytes.Buffer
	if err := tmpl.Execute(&secretOut, map[string]any{
		"SecretName":          secretName,
		"Namespace":           opts.Namespace,
		"PrimaryKeyName":      assets.EncryptionKeyName(1),
		"PrimaryKeyB64":       expectedB64,
		"NoPlaintextFallback": opts.NoPlaintextFallback,
	}); err != nil {
		return nil, fmt.Errorf("rendering encryption configuration: %w", err)
	}

	if opts.DryRun {
		return &EnsureResult{
			KeyRestored:    !generated,
			KeyGenerated:   generated,
			SecretCreated:  true,
			SecretVerified: true,
		}, nil
	}

	var sec corev1.Secret
	if err := yaml.Unmarshal(secretOut.Bytes(), &sec); err != nil {
		return nil, fmt.Errorf("unmarshaling rendered encryption secret: %w", err)
	}
	sec.Name = secretName
	sec.Namespace = opts.Namespace
	if sec.Data == nil && sec.StringData != nil {
		sec.Data = make(map[string][]byte)
		for k, v := range sec.StringData {
			sec.Data[k] = []byte(v)
		}
	}

	if opts.SecretClient != nil {
		if err := opts.SecretClient.CreateSecret(ctx, &sec); err != nil {
			return nil, fmt.Errorf("creating encryption secret %s/%s: %w", opts.Namespace, secretName, err)
		}
	}

	return &EnsureResult{
		KeyRestored:    !generated,
		KeyGenerated:   generated,
		SecretCreated:  true,
		SecretVerified: true,
	}, nil
}
