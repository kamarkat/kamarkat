package binaries

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Manager handles binary downloads and verification
type Manager struct {
	binDir string // ~/.zero-ops/bin/
}

// NewManager creates a new binary manager
func NewManager() (*Manager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	binDir := filepath.Join(homeDir, ".zero-ops", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create bin directory: %w", err)
	}

	return &Manager{binDir: binDir}, nil
}

// isSupportedPlatform checks if the current OS/arch is supported
func isSupportedPlatform(os, arch string) bool {
	supported := map[string][]string{
		"linux":  {"amd64", "arm64"},
		"darwin": {"amd64", "arm64"},
	}

	archs, ok := supported[os]
	if !ok {
		return false
	}

	for _, a := range archs {
		if a == arch {
			return true
		}
	}
	return false
}

// downloadWithChecksum downloads a binary, fetches its checksum file, verifies it, and makes it executable.
// targetFilename is used to search multi-file checksum manifests (e.g. Packer's SHA256SUMS). If empty, it assumes a single-hash file (like CAPI).
func downloadWithChecksum(ctx context.Context, binaryURL, checksumURL, dest, targetFilename string) error {
	// 1. Fetch checksum file
	expectedHash, err := fetchAndParseChecksum(ctx, checksumURL, targetFilename)
	if err != nil {
		return fmt.Errorf("failed to get expected checksum: %w", err)
	}

	// 2. Download binary
	if err := downloadFile(ctx, binaryURL, dest); err != nil {
		return fmt.Errorf("failed to download binary: %w", err)
	}

	// 3. Verify hash
	if err := verifyFileHash(dest, expectedHash); err != nil {
		os.Remove(dest) // Clean up corrupted/malicious file
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	// 4. Make executable
	return os.Chmod(dest, 0755)
}

// fetchAndParseChecksum downloads the checksum file and extracts the relevant hash
func fetchAndParseChecksum(ctx context.Context, url, targetFilename string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching checksum from %s", resp.StatusCode, url)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")

	// If a specific filename is provided (e.g. an arch-suffixed zip name), search for it
	if targetFilename != "" {
		for _, line := range lines {
			if strings.Contains(line, targetFilename) {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					return fields[0], nil
				}
			}
		}
		return "", fmt.Errorf("hash for %s not found in checksum file", targetFilename)
	}

	// Fallback for single-file format (e.g., CAPI .sha256). Grab the first valid hash.
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			return fields[0], nil
		}
	}

	return "", fmt.Errorf("no valid hash found in checksum file")
}

func verifyFileHash(filepath, expectedHash string) error {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return err
	}

	hash := sha256.Sum256(data)
	actual := hex.EncodeToString(hash[:])

	if actual != expectedHash {
		return fmt.Errorf("expected %s, got %s", expectedHash, actual)
	}

	return nil
}

// downloadFile downloads a file from URL to destination using streaming
func downloadFile(ctx context.Context, url, dest string) error {
	maxRetries := 3
	var lastErr error
	
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		
		f, err := os.Create(dest)
		if err != nil {
			return err
		}
		defer f.Close()
		
		_, err = io.Copy(f, resp.Body)
		if err != nil {
			lastErr = err
			os.Remove(dest)
			continue
		}
		
		return nil
	}
	
	return fmt.Errorf("download failed after %d attempts: %w", maxRetries, lastErr)
}

// exists checks if a file exists
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// GetOS returns the current operating system
func GetOS() string {
	return runtime.GOOS
}

// GetArch returns the current architecture
func GetArch() string {
	return runtime.GOARCH
}
