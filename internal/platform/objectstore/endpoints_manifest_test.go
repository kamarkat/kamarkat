package objectstore

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A hardcoded region endpoint is wrong on every box in another region: CNPG's
// WAL archiving to the box bucket failed with LocationConstraintConflict on an
// fsn1 box because the hub database manifests named hel1. Endpoints for the
// box's own buckets derive from global.region (PLACEHOLDER_REGION).
var hardcodedEndpoint = regexp.MustCompile(`\b[a-z]{3}[0-9]\.your-objectstorage\.com`)

// Files that may name a region, because the bucket they use is in that region
// whatever the box's region is. Adding a file here needs that reason; a file
// that stops naming a region must be removed from here.
var fixedRegionEndpoints = map[string]string{}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "manifests")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}

func TestNoHardcodedObjectStorageRegion(t *testing.T) {
	root := repoRoot(t)
	found := map[string]bool{}
	for _, top := range []string{"manifests", "operators"} {
		filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".yaml" && ext != ".yml" && ext != ".tpl" && ext != ".json" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil || !hardcodedEndpoint.Match(b) {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			found[filepath.ToSlash(rel)] = true
			return nil
		})
	}

	var unexpected []string
	for f := range found {
		if _, ok := fixedRegionEndpoints[f]; !ok {
			unexpected = append(unexpected, f)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("object-storage endpoint hardcoded to a region in:\n  %s\n"+
			"derive it from global.region (https://PLACEHOLDER_REGION.your-objectstorage.com "+
			"with a templated-fields entry), or add the file to fixedRegionEndpoints "+
			"with the reason its bucket is in that region",
			strings.Join(unexpected, "\n  "))
	}
	for f := range fixedRegionEndpoints {
		if !found[f] {
			t.Errorf("%s no longer names a region; remove it from fixedRegionEndpoints", f)
		}
	}
}
