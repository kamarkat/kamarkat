package main

import (
	"strings"
	"testing"
)

// The four outcomes are decided by the material alone, never by whether something
// happens to exist being read as intent.
//
// The case that matters most is the last one: a spoke created with a name a previous
// spoke used has its own, newly generated authority, and the escrow still holds the
// predecessor's. Reading "an escrow artefact exists" as "this is a recovery" would
// hand the new cluster trust roots its nodes never had.
func TestDecidePKICoversCreationAndRecovery(t *testing.T) {
	original := map[string][]byte{"tls.crt": []byte("cert-A"), "tls.key": []byte("key-A")}
	different := map[string][]byte{"tls.crt": []byte("cert-B"), "tls.key": []byte("key-B")}
	sameBytes := map[string][]byte{"tls.crt": []byte("cert-A"), "tls.key": []byte("key-A")}

	for _, tc := range []struct {
		name      string
		escrowed  map[string][]byte
		live      map[string][]byte
		liveFound bool
		want      pkiDecision
	}{
		{"recovering a spoke this hub has no PKI for", original, nil, false, pkiRestore},
		{"re-running after it was already restored", original, sameBytes, true, pkiAlreadyPresent},
		{"a reused name, or a regenerated root", original, different, true, pkiConflict},
		{"recovery asked for with nothing escrowed", nil, nil, false, pkiMissing},
		{"nothing escrowed and a live secret present", nil, original, true, pkiMissing},
		{"an empty escrowed map is not material", map[string][]byte{}, nil, false, pkiMissing},
	} {
		if got := decidePKI(tc.escrowed, tc.live, tc.liveFound); got != tc.want {
			t.Errorf("%s: decided %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Writing a trust root underneath a running controller is the race the pause
// contract exists to prevent, so only a cluster held off by one of the two
// mechanisms is accepted.
func TestPausedGateAcceptsOnlyAPausedCluster(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"spec.paused", `{"spec":{"paused":true}}`, true},
		{"the paused annotation", `{"metadata":{"annotations":{"cluster.x-k8s.io/paused":""}}}`, true},
		{"both", `{"metadata":{"annotations":{"cluster.x-k8s.io/paused":"true"}},"spec":{"paused":true}}`, true},
		{"neither", `{"spec":{"paused":false}}`, false},
		{"an unrelated annotation", `{"metadata":{"annotations":{"other":"x"}}}`, false},
		{"spec.paused explicitly false with no annotation", `{"metadata":{},"spec":{"paused":false}}`, false},
	} {
		got, err := clusterIsPaused([]byte(tc.raw))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: paused=%v, want %v", tc.name, got, tc.want)
		}
	}

	if _, err := clusterIsPaused([]byte("not json")); err == nil {
		t.Error("an unreadable Cluster was treated as an answer; it must be an error, " +
			"because the safe reading of 'cannot tell' is not 'paused'")
	}
}

// A report identifies material without disclosing it, and the same bytes must always
// fingerprint the same way or the comparison in the report is meaningless.
func TestFingerprintIdentifiesWithoutDisclosing(t *testing.T) {
	secret := []byte("-----BEGIN RSA PRIVATE KEY-----aaaa")
	data := map[string][]byte{"tls.key": secret, "tls.crt": []byte("cert")}

	fp := fingerprintData(data)
	if fp == "" || len(fp) != 12 {
		t.Fatalf("fingerprint is %q; want 12 hex characters", fp)
	}
	if strings.Contains(fp, "PRIVATE") || strings.Contains(fp, string(secret[:8])) {
		t.Error("the fingerprint carries the material it is meant to stand in for")
	}

	// Key order must not change it: the map is marshalled with sorted keys, and a
	// fingerprint that varied by iteration order would report a false conflict.
	reordered := map[string][]byte{"tls.crt": []byte("cert"), "tls.key": secret}
	if fingerprintData(reordered) != fp {
		t.Error("the fingerprint depends on map order, so identical material would " +
			"report as a conflict")
	}

	if fingerprintData(map[string][]byte{"tls.crt": []byte("other")}) == fp {
		t.Error("different material fingerprinted identically")
	}
}
