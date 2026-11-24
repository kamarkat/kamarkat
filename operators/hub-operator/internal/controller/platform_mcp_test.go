package controller

import (
	"encoding/base64"
	"testing"
)

func TestPlatformMcpDeclarationsSurviveRendering(t *testing.T) {
	// The capability renders redirect URIs comma-joined and the key as base64,
	// because a templated value is one single-quoted line.
	got := lines("https://kakt.example/settings/connections/box/callback,http://localhost:3002/settings/connections/box/callback")
	if len(got) != 2 || got[1] != "http://localhost:3002/settings/connections/box/callback" {
		t.Fatalf("lines: %q", got)
	}
	pem := "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----"
	if decodePublicKey(base64.StdEncoding.EncodeToString([]byte(pem))) != pem {
		t.Fatal("a base64 key was not decoded")
	}
	if decodePublicKey(pem) != pem || decodePublicKey("") != "" {
		t.Fatal("a PEM written by hand, or none, was not read as is")
	}
}
