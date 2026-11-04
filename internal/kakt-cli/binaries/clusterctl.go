package binaries

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const clusterctlVersion = "v1.13.7"

// Known-good SHA256 checksums for clusterctl v1.13.7
// Computed manually at pin time from official GitHub releases
var clusterctlChecksums = map[string]string{
	"linux/amd64":   "0952cb107303ead3b76242f539fdd98c82f0edecc860ccdd7cd43ac41990897d",
	"linux/arm64":   "a9c2c21bded3f7caa61d198a293b05447bf4ba02db86cd08e1d103e90121c548",
	"darwin/amd64":  "c294eb19e8b53a5899f1d74a26c6d9ff771f9b8ec8012803f1c87b0e02ed90dc",
	"darwin/arm64":  "1eff80d7dc0da472722d874b7273ff695f3baa0ba97d889d804db430bd9426e6",
}

// ClusterctlManager manages the clusterctl binary
type ClusterctlManager struct {
	manager *Manager
	binPath string
	version string
}

// NewClusterctlManager creates a new clusterctl manager
func NewClusterctlManager() (*ClusterctlManager, error) {
	manager, err := NewManager()
	if err != nil {
		return nil, err
	}

	return &ClusterctlManager{
		manager: manager,
		binPath: filepath.Join(manager.binDir, "clusterctl"),
		version: clusterctlVersion,
	}, nil
}

// EnsureInstalled ensures clusterctl is installed and verified
func (m *ClusterctlManager) EnsureInstalled(ctx context.Context) error {
	// First, check if clusterctl is already available in PATH
	if path, err := exec.LookPath("clusterctl"); err == nil {
		m.binPath = path
		return nil
	}

	osName := runtime.GOOS
	arch := runtime.GOARCH

	// Validate supported platform for auto-download
	if !isSupportedPlatform(osName, arch) {
		return fmt.Errorf("unsupported architecture (%s/%s). Please install clusterctl manually in PATH", osName, arch)
	}

	// Check if binary exists
	if exists(m.binPath) {
		return nil
	}

	// Get expected checksum
	checksumKey := fmt.Sprintf("%s/%s", osName, arch)
	expectedHash, ok := clusterctlChecksums[checksumKey]
	if !ok {
		return fmt.Errorf("no checksum available for %s", checksumKey)
	}

	// Download binary
	binaryFilename := fmt.Sprintf("clusterctl-%s-%s", osName, arch)
	binaryURL := fmt.Sprintf("https://github.com/kubernetes-sigs/cluster-api/releases/download/%s/%s", m.version, binaryFilename)

	if err := downloadFile(ctx, binaryURL, m.binPath); err != nil {
		return fmt.Errorf("failed to download clusterctl: %w", err)
	}

	// Verify checksum
	if err := verifyFileHash(m.binPath, expectedHash); err != nil {
		os.Remove(m.binPath)
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	// Make executable
	if err := os.Chmod(m.binPath, 0755); err != nil {
		return fmt.Errorf("failed to make executable: %w", err)
	}

	return nil
}

// GetPath returns the path to the clusterctl binary.
// Prefers a system-installed clusterctl found in PATH over the managed binary.
func (m *ClusterctlManager) GetPath() string {
	if path, err := exec.LookPath("clusterctl"); err == nil {
		return path
	}
	return m.binPath
}

// remove removes the clusterctl binary
func (m *ClusterctlManager) remove() error {
	if exists(m.binPath) {
		return os.Remove(m.binPath)
	}
	return nil
}
