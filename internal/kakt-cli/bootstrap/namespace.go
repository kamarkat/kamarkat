package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NamespaceManager manages Kubernetes namespaces
type NamespaceManager struct {
	Kubeconfig string
	Context    string
	Namespace  string
}

func (m *NamespaceManager) Create(ctx context.Context) error {
	var args []string

	// Skip --kubeconfig if using default location (kubectl v1.34+ bug workaround)
	homeDir, _ := os.UserHomeDir()
	defaultKubeconfig := filepath.Join(homeDir, ".kube", "config")
	if m.Kubeconfig != "" && m.Kubeconfig != defaultKubeconfig {
		args = append(args, "--kubeconfig", m.Kubeconfig)
	}

	if m.Context != "" {
		args = append(args, "--context", m.Context)
	}
	args = append(args, "create", "namespace", m.Namespace)

	cmd := exec.CommandContext(ctx, "kubectl", args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Ignore if already exists
		if strings.Contains(string(output), "already exists") ||
			strings.Contains(string(output), "AlreadyExists") {
			return nil
		}
		return fmt.Errorf("failed to create namespace: %w\n%s", err, output)
	}

	return nil
}
