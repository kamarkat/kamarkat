package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/kamarkat/kamarkat/internal/assets"
	"github.com/kamarkat/kamarkat/internal/kakt-cli/binaries"
)

// FlatcarImageValidator validates Flatcar image availability
type FlatcarImageValidator struct {
	Token       string
	ImageID     *string // Pointer to allow updating
	BuildImage  bool
	Region      string
	ClusterName string
}

func (v *FlatcarImageValidator) Validate(ctx context.Context) error {
	if v.BuildImage {
		return v.buildFlatcarImage(ctx)
	}
	
	if v.ImageID == nil || *v.ImageID == "" {
		return fmt.Errorf("--image-id required for Flatcar (or use --build-flatcar-image)")
	}
	
	fmt.Printf("  ✓ Using Flatcar image: %s\n", *v.ImageID)
	return nil
}

func (v *FlatcarImageValidator) buildFlatcarImage(ctx context.Context) error {
	fmt.Println("  Building Flatcar snapshot with Packer...")
	
	snapshotName := fmt.Sprintf("flatcar-%s", v.ClusterName)
	
	// Check if snapshot already exists
	existingID, err := v.findExistingSnapshot(snapshotName)
	if err == nil && existingID != "" {
		fmt.Printf("  ✓ Reusing existing Flatcar snapshot: %s (ID: %s, selector: snapshot-name==%s)\n", snapshotName, existingID, snapshotName)
		*v.ImageID = fmt.Sprintf("snapshot-name==%s", snapshotName)
		return nil
	}
	
	// Download Packer if needed
	packerMgr, err := binaries.NewPackerManager()
	if err != nil {
		return fmt.Errorf("failed to create packer manager: %w", err)
	}
	
	if err := packerMgr.EnsureInstalled(ctx); err != nil {
		return fmt.Errorf("failed to install packer: %w", err)
	}
	
	// Get Packer template
	packerConfig, err := assets.ReadManifest("packer/hetzner-flatcar.pkr.hcl")
	if err != nil {
		return fmt.Errorf("failed to read packer config: %w", err)
	}
	
	tmpDir, err := os.MkdirTemp("", "flatcar-packer-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	
	configPath := filepath.Join(tmpDir, "hetzner-flatcar.pkr.hcl")
	if err := os.WriteFile(configPath, packerConfig, 0644); err != nil {
		return err
	}
	
	// Run packer init
	initCmd := exec.CommandContext(ctx, packerMgr.GetPath(), "init", configPath)
	if output, err := initCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("packer init failed: %w\n%s", err, output)
	}
	
	// Run packer build
	buildCmd := exec.CommandContext(ctx, packerMgr.GetPath(), "build",
		"-var", fmt.Sprintf("hcloud_token=%s", v.Token),
		"-var", fmt.Sprintf("snapshot_name=%s", snapshotName),
		"-var", fmt.Sprintf("location=%s", v.Region),
		configPath,
	)
	
	var stdout, stderr bytes.Buffer
	buildCmd.Stdout = &stdout
	buildCmd.Stderr = &stderr
	
	if err := buildCmd.Run(); err != nil {
		combinedOutput := stdout.String() + stderr.String()
		
		// Try to extract ID from Packer output (id=364340504)
		if idMatch := regexp.MustCompile(`id=(\d+)`).FindStringSubmatch(combinedOutput); len(idMatch) > 1 {
			*v.ImageID = idMatch[1]
			fmt.Printf("  ✓ Flatcar snapshot exists: %s (ID: %s)\n", snapshotName, *v.ImageID)
			return nil
		}
		
		// Check if snapshot was created despite error
		if snapshotID, findErr := v.findExistingSnapshot(snapshotName); findErr == nil && snapshotID != "" {
			fmt.Printf("  ✓ Flatcar snapshot reused: %s (ID: %s)\n", snapshotName, snapshotID)
			*v.ImageID = snapshotID
			return nil
		}
		
		return fmt.Errorf("packer build failed: %w\n%s", err, combinedOutput)
	}
	
	// Get snapshot ID
	snapshotID, err := v.findExistingSnapshot(snapshotName)
	if err != nil {
		return fmt.Errorf("snapshot created but ID not found: %w", err)
	}
	
	// Use label selector format for CAPH (it searches by label first, then name)
	// Format: label-key==label-value
	*v.ImageID = fmt.Sprintf("snapshot-name==%s", snapshotName)
	fmt.Printf("  ✓ Flatcar snapshot created: %s (ID: %s, selector: snapshot-name==%s)\n", snapshotName, snapshotID, snapshotName)
	return nil
}

func (v *FlatcarImageValidator) findExistingSnapshot(name string) (string, error) {
	cmd := exec.Command("curl", "-s",
		"-H", fmt.Sprintf("Authorization: Bearer %s", v.Token),
		"https://api.hetzner.cloud/v1/images?type=snapshot",
	)
	
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	
	var result struct {
		Images []struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"images"`
	}
	
	if err := json.Unmarshal(output, &result); err != nil {
		return "", err
	}
	
	for _, img := range result.Images {
		// Packer sets description, not name
		if img.Description == name {
			return fmt.Sprintf("%d", img.ID), nil
		}
	}
	
	return "", fmt.Errorf("snapshot not found")
}
