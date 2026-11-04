package infisical

import (
	"net"
	"strconv"
	"testing"
)

// The local port is an implementation detail of reaching the service. An occupied
// port must be worked around, never failed on and never resolved by disturbing
// whatever owns it — developer machines routinely have dev servers on 8081.
func TestFindFreePortSkipsOccupiedPorts(t *testing.T) {
	// Occupy a port the way a stray dev server would.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupied := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	got, err := findFreePort(occupied)
	if err != nil {
		t.Fatalf("findFreePort(%s): %v", occupied, err)
	}
	if got == occupied {
		t.Errorf("returned the occupied port %s instead of moving past it", got)
	}
	if portInUse(got) {
		t.Errorf("returned port %s which is also in use", got)
	}
}

// A free preferred port must be used as-is, so the URL stays predictable.
func TestFindFreePortPrefersRequested(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	free := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close() // now free

	got, err := findFreePort(free)
	if err != nil {
		t.Fatalf("findFreePort(%s): %v", free, err)
	}
	if got != free {
		t.Errorf("got %s, want the requested %s", got, free)
	}
}

func TestFindFreePortRejectsNonNumeric(t *testing.T) {
	if _, err := findFreePort("http"); err == nil {
		t.Error("expected an error for a non-numeric port")
	}
}

// LocalURL must follow the port actually chosen, or the client would be pointed at
// the very port that was rejected.
func TestLocalURLTracksSelectedPort(t *testing.T) {
	pf := NewPortForwardManager("ns", "svc", "8081", "8080", "")
	pf.localPort = "8093" // as Start() would set after re-selection
	if want := "http://localhost:8093"; pf.LocalURL() != want {
		t.Errorf("LocalURL() = %s, want %s", pf.LocalURL(), want)
	}
}
