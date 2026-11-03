package main

import (
	"strings"
	"testing"
)

// SSE-C takes a 256-bit key and nothing else, so a wrong length is refused before any
// Secret is written rather than discovered when a backup cannot be read.
func TestDecodeSSECKeyAcceptsOnly32Bytes(t *testing.T) {
	good := strings.Repeat("ab", sseCKeyBytes) // 64 hex chars -> 32 bytes
	raw, err := decodeSSECKey(good)
	if err != nil {
		t.Fatalf("a valid key was refused: %v", err)
	}
	if len(raw) != sseCKeyBytes {
		t.Fatalf("decoded %d bytes, want %d", len(raw), sseCKeyBytes)
	}

	// Surrounding whitespace is an escrow artefact, not a different key.
	if _, err := decodeSSECKey("  " + good + "\n"); err != nil {
		t.Errorf("whitespace made a valid key invalid: %v", err)
	}

	for name, in := range map[string]string{
		"too short":    strings.Repeat("ab", 16),
		"too long":     strings.Repeat("ab", 40),
		"not hex":      strings.Repeat("zz", 32),
		"empty":        "",
		"odd length":   strings.Repeat("a", 63),
		"raw 32 chars": strings.Repeat("k", 32), // 32 characters, not 32 bytes of key
	} {
		if _, err := decodeSSECKey(in); err == nil {
			t.Errorf("%s was accepted as an SSE-C key", name)
		}
	}
}
