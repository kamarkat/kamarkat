package clusterclass

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kamarkat/kamarkat/internal/assets"
)

// Deployer deploys ClusterClass library to Management Cluster
type Deployer struct {
	Kubeconfig string
	Namespace  string
	ClassPaths []string
}

// Deploy applies all ClusterClass definitions
func (d *Deployer) Deploy(ctx context.Context) error {
	for _, classPath := range d.ClassPaths {
		manifest, err := assets.ReadManifest(classPath)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", classPath, err)
		}

		cmd := exec.CommandContext(ctx, "kubectl", "apply",
			"--kubeconfig", d.Kubeconfig,
			"-f", "-",
		)
		cmd.Stdin = bytes.NewReader(manifest)

		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to apply %s: %w\n%s", classPath, err, output)
		}

		fmt.Printf("[clusterclass-deploy] ✓ Applied %s\n", classPath)
	}

	return d.verify(ctx)
}

func (d *Deployer) verify(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", d.Kubeconfig,
		"get", "clusterclass",
		"-n", d.Namespace,
		"--no-headers",
	)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to verify ClusterClasses: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return fmt.Errorf("no ClusterClass found after applying manifests")
	}

	return nil
}
