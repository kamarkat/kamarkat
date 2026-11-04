package cluster

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/kamarkat/kamarkat/internal/assets"
	"gopkg.in/yaml.v3"
)

// ADR-003 section 6: secretbox at-rest encryption mode.

func TestTheDefaultModeIsSecretboxAndNotAnError(t *testing.T) {
	// EVERY EXISTING BOX HAS AN EMPTY Encryption FIELD. If an empty value were
	// rejected, adding this flag would have changed what every bootstrap does -- and
	// the change would surface as a control plane that will not start, after the
	// infrastructure exists.
	p := &Provisioner{Config: &Config{ClusterName: "nutgraf-hub", Namespace: "platform-capi"}}
	err := p.applyEncryptionConfig(context.Background())
	if err == nil {
		t.Fatal("expected the secretbox path to refuse without an escrow")
	}
	// It must fail for the ESCROW reason, which proves it took the secretbox branch.
	if !strings.Contains(err.Error(), "escrow") {
		t.Fatalf("an empty mode did not take the secretbox path: %v", err)
	}
}

func TestAnUnknownModeIsRefusedByName(t *testing.T) {
	p := &Provisioner{Config: &Config{
		ClusterName: "nutgraf-hub", Namespace: "platform-capi", Encryption: "aes-gcm",
	}}
	err := p.applyEncryptionConfig(context.Background())
	if err == nil {
		t.Fatal("an unknown encryption mode was accepted")
	}
	for _, want := range []string{"aes-gcm", "secretbox"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func renderSecretbox(t *testing.T, noFallback bool) []byte {
	t.Helper()
	rendered, err := renderManifest("secrets/secret-encryption-config.yaml", map[string]any{
		"SecretName":          assets.EncryptionSecretName("nutgraf-01"),
		"Namespace":           "platform-capi",
		"PrimaryKeyName":      assets.EncryptionKeyName(1),
		"PrimaryKeyB64":       base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"NoPlaintextFallback": noFallback,
	})
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return rendered
}

func encryptionConfigFrom(t *testing.T, rendered []byte) map[string]any {
	t.Helper()
	var secret struct {
		StringData struct {
			EncYAML string `yaml:"enc.yaml"`
		} `yaml:"stringData"`
	}
	if err := yaml.Unmarshal(rendered, &secret); err != nil {
		t.Fatalf("the rendered Secret is not valid YAML: %v\n%s", err, rendered)
	}
	if strings.TrimSpace(secret.StringData.EncYAML) == "" {
		t.Fatalf("the Secret carries no enc.yaml:\n%s", rendered)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(secret.StringData.EncYAML), &cfg); err != nil {
		t.Fatalf("enc.yaml is not valid YAML: %v\n%s", err, secret.StringData.EncYAML)
	}
	return cfg
}

func providersOf(t *testing.T, cfg map[string]any) []map[string]any {
	t.Helper()
	resources, ok := cfg["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("expected exactly one resources entry, got %v", cfg["resources"])
	}
	entry := resources[0].(map[string]any)
	raw, ok := entry["providers"].([]any)
	if !ok {
		t.Fatalf("no providers list: %v", entry)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.(map[string]any))
	}
	return out
}

func TestSecretboxProviderIsFirstAndIdentitySecond(t *testing.T) {
	providers := providersOf(t, encryptionConfigFrom(t, renderSecretbox(t, false)))
	if len(providers) != 2 {
		t.Fatalf("expected secretbox then identity, got %d providers: %v", len(providers), providers)
	}
	if _, ok := providers[0]["secretbox"]; !ok {
		t.Fatalf("the FIRST provider is not secretbox, so writes are not encrypted: %v", providers[0])
	}
	if _, ok := providers[1]["identity"]; !ok {
		t.Fatalf("the second provider is not identity: %v", providers[1])
	}
}

func TestSecretboxDroppingPlaintextFallbackRemovesIdentity(t *testing.T) {
	providers := providersOf(t, encryptionConfigFrom(t, renderSecretbox(t, true)))
	if len(providers) != 1 {
		t.Fatalf("expected only secretbox provider, got %d providers: %v", len(providers), providers)
	}
	if _, ok := providers[0]["secretbox"]; !ok {
		t.Fatalf("the remaining provider is not secretbox: %v", providers[0])
	}
}

func TestSecretboxSecretNameAndNamespace(t *testing.T) {
	var secret struct {
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(renderSecretbox(t, false), &secret); err != nil {
		t.Fatal(err)
	}
	if secret.Metadata.Name != "nutgraf-01-encryption-config" {
		t.Errorf("name is %q, want %q", secret.Metadata.Name, "nutgraf-01-encryption-config")
	}
	if secret.Metadata.Namespace != "platform-capi" {
		t.Errorf("namespace is %q, want %q", secret.Metadata.Namespace, "platform-capi")
	}
}
