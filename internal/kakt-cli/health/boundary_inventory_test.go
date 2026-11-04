package health

import (
	"os"
	"path/filepath"
	"testing"
)

// The descriptor count is one half of the expected inventory, and getting it
// wrong in either direction defeats the gate: too low and an empty boundary
// passes, too high and every bootstrap stalls. It is pure filesystem, so it is
// the part worth testing directly.
func TestDescriptorCount(t *testing.T) {
	tests := []struct {
		name     string
		boundary string
		files    []string
		want     int
	}{
		{
			name:     "counts only yaml files",
			boundary: "03",
			files:    []string{"a.yaml", "b.yaml", "README.md", "notes.txt"},
			want:     2,
		},
		{
			name:     "a boundary with no descriptor directory counts zero, not an error",
			boundary: "09",
			files:    nil,
			want:     0,
		},
		{
			name:     "an empty descriptor directory counts zero",
			boundary: "04",
			files:    []string{},
			want:     0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.files != nil {
				dir := filepath.Join(root, "manifests", "argocd", "components", tc.boundary)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				for _, f := range tc.files {
					if err := os.WriteFile(filepath.Join(dir, f), []byte("appName: x\n"), 0o644); err != nil {
						t.Fatalf("write %s: %v", f, err)
					}
				}
			}

			c := NewBoundaryInventoryChecker(tc.boundary, root)
			got, err := c.descriptorCount()
			if err != nil {
				t.Fatalf("descriptorCount: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("descriptorCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// A subdirectory is not a descriptor. Counting one would inflate the expected
// inventory and stall every bootstrap of that boundary.
func TestDescriptorCountIgnoresSubdirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "manifests", "argocd", "components", "03")
	if err := os.MkdirAll(filepath.Join(dir, "nested.yaml"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.yaml"), []byte("appName: x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := NewBoundaryInventoryChecker("03", root)
	got, err := c.descriptorCount()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("descriptorCount = %d, want 1 (a directory named *.yaml is not a descriptor)", got)
	}
}

// The checker names the boundary in its output; a bootstrap failure is only
// attributable if the message says which boundary it came from.
func TestNameIdentifiesTheBoundary(t *testing.T) {
	c := NewBoundaryInventoryChecker("03", "")
	if got, want := c.Name(), "boundary 03 inventory"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// The real repository must agree with the gate: boundary 03's descriptors are
// countable from the tree as checked in. This is what stops a rename of the
// descriptor directory from silently reducing the expected count to zero.
func TestRepositoryDescriptorsAreCountable(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("not running from the repository tree: %v", err)
	}

	c := NewBoundaryInventoryChecker("03", root)
	got, err := c.descriptorCount()
	if err != nil {
		t.Fatalf("descriptorCount: %v", err)
	}
	if got == 0 {
		t.Error("boundary 03 has zero descriptors in the repository; either the directory moved or the gate is looking in the wrong place")
	}
}
