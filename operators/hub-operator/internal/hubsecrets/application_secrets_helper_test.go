package hubsecrets

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, n int) string {
	t.Helper()
	// Test data only: production values come from OpenBao password policies.
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
