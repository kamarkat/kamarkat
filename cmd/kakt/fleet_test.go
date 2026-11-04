package main

import (
	"os"
	"path/filepath"
	"testing"
)

// readEnvFile must return an empty string for a declared-but-unfilled entry,
// and the import path must treat that as "not supplied yet" rather than as a
// value.
//
// The skeleton `kakt fleet secrets template` emits is entirely empty entries.
// Importing one with --confirm, before this distinction existed, would have
// written empty strings over every secret already supplied -- turning a
// convenience into the one operation that can destroy a working fleet's
// credentials in a single command.
func TestReadEnvFileDistinguishesEmptyFromAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := `# a comment
FILLED=a-value
EMPTY=
QUOTED="quoted value"
SPACES=   
NOT_AN_ASSIGNMENT
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if got["FILLED"] != "a-value" {
		t.Errorf("FILLED = %q, want %q", got["FILLED"], "a-value")
	}
	if got["QUOTED"] != "quoted value" {
		t.Errorf("QUOTED = %q, want surrounding quotes stripped", got["QUOTED"])
	}

	// Present as a key, empty as a value. The import path decides what that
	// means; conflating it with absence here would remove the distinction
	// before anything could act on it.
	if v, ok := got["EMPTY"]; !ok || v != "" {
		t.Errorf("EMPTY: got (%q, present=%v), want present with an empty value", v, ok)
	}
	// Whitespace-only is empty too: a skeleton entry someone tabbed past is not
	// a value, and writing it would blank a live secret just as surely.
	if v, ok := got["SPACES"]; !ok || v != "" {
		t.Errorf("SPACES: got (%q, present=%v), want present and empty after trimming", v, ok)
	}
	if _, ok := got["NOT_AN_ASSIGNMENT"]; ok {
		t.Error("a line with no '=' became a key")
	}
	if _, ok := got["# a comment"]; ok {
		t.Error("a comment became a key")
	}
}
