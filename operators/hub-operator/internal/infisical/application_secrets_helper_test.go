package infisical

import (
	"testing"

	"github.com/kamarkat/kamarkat/operators/hub-operator/internal/secrets"
)

func mustHex(t *testing.T, n int) string {
	t.Helper()
	s, err := secrets.GenerateHexKey(n)
	if err != nil {
		t.Fatalf("GenerateHexKey(%d): %v", n, err)
	}
	return s
}
