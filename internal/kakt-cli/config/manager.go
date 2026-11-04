package config

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Manager manages kubeconfig
type Manager struct {
	BootstrapKubeconfig string
	ClusterName         string
	Namespace           string
}

// SaveKubeconfig retrieves and saves kubeconfig locally
func (m *Manager) SaveKubeconfig(ctx context.Context) (string, error) {
	secretName := fmt.Sprintf("%s-kubeconfig", m.ClusterName)
	
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", m.BootstrapKubeconfig,
		"get", "secret", secretName,
		"-n", m.Namespace,
		"-o", "jsonpath={.data.value}",
	)
	
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	
	decoded, err := base64.StdEncoding.DecodeString(string(output))
	if err != nil {
		return "", fmt.Errorf("failed to decode kubeconfig: %w", err)
	}
	
	// Save to k8-secrets/kubeconfig directory
	dir := filepath.Join("k8-secrets", "kubeconfig")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create kubeconfig directory: %w", err)
	}
	
	filename := fmt.Sprintf("%s.kubeconfig", m.ClusterName)
	path := filepath.Join(dir, filename)
	
	if err := os.WriteFile(path, decoded, 0600); err != nil {
		return "", fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	
	return path, nil
}

// MergeKubeconfig merges kubeconfig into ~/.kube/config
func (m *Manager) MergeKubeconfig(ctx context.Context, sourcePath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	
	homeKubeconfig := filepath.Join(homeDir, ".kube", "config")
	
	// Backup existing kubeconfig
	backupPath := fmt.Sprintf("%s.backup-%d", homeKubeconfig, os.Getpid())
	if err := m.copyFile(homeKubeconfig, backupPath); err != nil {
		return fmt.Errorf("failed to backup kubeconfig: %w", err)
	}
	
	// Merge configs
	cmd := exec.CommandContext(ctx, "kubectl", "config", "view",
		"--kubeconfig", homeKubeconfig,
		"--kubeconfig", sourcePath,
		"--flatten",
	)
	
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to merge kubeconfig: %w", err)
	}
	
	// Write merged config
	if err := os.WriteFile(homeKubeconfig, output, 0600); err != nil {
		return fmt.Errorf("failed to write merged kubeconfig: %w", err)
	}
	
	return nil
}

func (m *Manager) copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Source doesn't exist, skip backup
		}
		return err
	}
	return os.WriteFile(dst, data, 0600)
}
