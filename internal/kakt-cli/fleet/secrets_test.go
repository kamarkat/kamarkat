package fleet

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fleetFixture() Fleet {
	return Fleet{
		TenantID: "acme",
		AppID:    "storefront",
		CellID:   "nutgraf-01",
		Secrets: []Declaration{
			{Name: "ACME_INTERNAL_TOKEN", Capability: "the BFF/SDK channel", Workloads: []string{"bff", "sdk"}},
			{Name: "AI_GATEWAY_API_KEY", Capability: "the AI gateway", Workloads: []string{"sdk"}},
			{Name: "S3_ACCESS_KEY_ID", Capability: "object storage", Workloads: []string{"sdk"}},
		},
	}
}

// The path is derived from the fleet's own declaration. Written out per key, it
// was twenty-three chances to name the wrong cell.
func TestSecretPathIsDerived(t *testing.T) {
	// The segment is the APP. That is what it always held -- before ADR-088 the
	// field feeding it was called tenantId but carried a product -- so the
	// content was right and only the label was wrong. Correcting the label
	// means moving live key material, which is deferred to its own change.
	if got, want := fleetFixture().SecretPath(), "/spoke-pool/nutgraf-01/tenants/storefront"; got != want {
		t.Fatalf("SecretPath() = %q, want %q", got, want)
	}
}

func TestJoinSeparatesPresentAbsentAndOrphan(t *testing.T) {
	// db-credentials is platform-written and appears in no fleet's `secrets:`.
	s := Join(fleetFixture(), []string{"ACME_INTERNAL_TOKEN", "db-credentials"})

	var present, absent, orphan []string
	for _, e := range s.Entries {
		switch {
		case e.Orphan:
			orphan = append(orphan, e.Name)
		case e.Present:
			present = append(present, e.Name)
		default:
			absent = append(absent, e.Name)
		}
	}

	if want := []string{"ACME_INTERNAL_TOKEN"}; !reflect.DeepEqual(present, want) {
		t.Errorf("present = %v, want %v", present, want)
	}
	if want := []string{"AI_GATEWAY_API_KEY", "S3_ACCESS_KEY_ID"}; !reflect.DeepEqual(absent, want) {
		t.Errorf("absent = %v, want %v", absent, want)
	}
	// Reported, not removed: the platform owns this key and the fleet cannot
	// know that from its own file.
	if want := []string{"db-credentials"}; !reflect.DeepEqual(orphan, want) {
		t.Errorf("orphan = %v, want %v", orphan, want)
	}
}

// The consequence a per-key list understates. One absent key fails the whole
// ExternalSecret, so the SDK loses all three of its keys -- not the one.
func TestWithheldWorkloadsReportsTheWholeObject(t *testing.T) {
	s := Join(fleetFixture(), []string{"ACME_INTERNAL_TOKEN"})

	got := s.WithheldWorkloads()
	if got["sdk"] != 3 {
		t.Errorf("sdk withheld = %d, want 3 (every key in its ExternalSecret, not just the absent ones)", got["sdk"])
	}
	// The BFF's only key IS present, so nothing of the BFF's is withheld. This
	// is the property per-workload grouping buys: the SDK's missing credential
	// does not stop the BFF.
	if _, broken := got["bff"]; broken {
		t.Errorf("bff reported as withheld, but its only key is present — per-workload grouping is not isolating failures")
	}
}

func TestWithheldIsEmptyWhenEverythingIsSupplied(t *testing.T) {
	s := Join(fleetFixture(), []string{"ACME_INTERNAL_TOKEN", "AI_GATEWAY_API_KEY", "S3_ACCESS_KEY_ID"})
	if n := len(s.Missing()); n != 0 {
		t.Fatalf("Missing() = %d entries, want 0", n)
	}
	if n := len(s.WithheldWorkloads()); n != 0 {
		t.Fatalf("WithheldWorkloads() = %v, want empty", s.WithheldWorkloads())
	}
}

// No type in this package has a field a secret value could occupy. Asserted
// structurally rather than by reviewing the formatter: a value that cannot be
// held cannot be printed, and this fails if a field is ever added.
func TestNoTypeCanHoldASecretValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  any
	}{
		{"Declaration", Declaration{}},
		{"Entry", Entry{}},
		{"Status", Status{}},
		{"Fleet", Fleet{}},
	} {
		v := reflect.TypeOf(tc.typ)
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			lower := strings.ToLower(f.Name)
			if strings.Contains(lower, "value") || strings.Contains(lower, "secretvalue") || lower == "password" {
				t.Errorf("%s has field %q: this package reports secrets BY NAME, and a field "+
					"that can hold a value is one formatting mistake from a credential in a "+
					"scrollback buffer", tc.name, f.Name)
			}
		}
	}
}

func TestLoadRejectsAFleetWithNoPath(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"no tenantId", "cellId: nutgraf-01\n", "tenantId"},
		{"no cellId", "tenantId: acme\n", "cellId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			envDir := filepath.Join(dir, "environments", "dev", "acme")
			if err := os.MkdirAll(envDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(envDir, "values.yaml"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}

			// Half a path is worse than none: it produces a valid string naming
			// a folder that will never hold anything.
			_, err := Load(dir, "dev", "acme")
			if err == nil {
				t.Fatalf("Load() accepted a fleet with no %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name the missing field %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadReadsDeclarations(t *testing.T) {
	dir := t.TempDir()
	envDir := filepath.Join(dir, "environments", "dev", "acme")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
tenantId: acme
cellId: nutgraf-01
secrets:
  - name: AI_GATEWAY_API_KEY
    capability: "the AI gateway the SDK calls"
    workloads: [sdk]
`
	if err := os.WriteFile(filepath.Join(envDir, "values.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := Load(dir, "dev", "acme")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := f.Declared("AI_GATEWAY_API_KEY")
	if !ok {
		t.Fatal("declared key not found")
	}
	if d.Capability == "" {
		t.Error("capability was not read: it is what a report names, so losing it makes the report unactionable")
	}
	if _, ok := f.Declared("NOT_DECLARED"); ok {
		t.Error("Declared() returned true for a key the fleet does not declare")
	}
}
