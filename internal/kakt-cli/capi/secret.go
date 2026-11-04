package capi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/kamarkat/kamarkat/internal/assets"
)

// SecretManager manages CAPI secrets
type SecretManager struct {
	Kubeconfig string
	Context    string
	Namespace  string
}

func (m *SecretManager) kubectlArgs(args ...string) []string {
	var result []string
	
	// Skip --kubeconfig if using default location (kubectl v1.34+ bug workaround)
	homeDir, _ := os.UserHomeDir()
	defaultKubeconfig := filepath.Join(homeDir, ".kube", "config")
	if m.Kubeconfig != "" && m.Kubeconfig != defaultKubeconfig {
		result = append(result, "--kubeconfig", m.Kubeconfig)
	}
	
	if m.Context != "" {
		result = append(result, "--context", m.Context)
	}
	return append(result, args...)
}

func (m *SecretManager) CreateHetznerSecret(ctx context.Context, token string) error {
	// Read template from file
	tmplData, err := assets.ReadManifest("secrets/hetzner-credentials.yaml")
	if err != nil {
		return fmt.Errorf("failed to read hetzner secret template: %w", err)
	}
	
	// Parse and execute template
	tmpl, err := template.New("hetzner-secret").Parse(string(tmplData))
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}
	
	var buf bytes.Buffer
	data := struct {
		Namespace   string
		HCloudToken string
	}{
		Namespace:   m.Namespace,
		HCloudToken: token,
	}
	
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}
	
	cmd := exec.CommandContext(ctx, "kubectl", m.kubectlArgs("apply", "-f", "-")...)
	cmd.Stdin = &buf
	
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create secret: %w\n%s", err, output)
	}
	
	return nil
}
