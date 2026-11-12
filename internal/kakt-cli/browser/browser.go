// Package browser opens a page on the operator's own machine.
//
// A CLI step that needs something only a browser can do -- signing in, issuing a
// token on a provider's console -- is finished faster by opening the page than by
// printing a URL for someone to copy. The URL is printed as well, always: the
// opener fails silently on a machine with no desktop session, and a step whose
// only instruction went to a browser that never appeared is a dead end.
package browser

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Suppressed turns every Open into a no-op, for an operator who does not want
// pages appearing -- a remote shell, a shared screen, a recorded session.
//
// KAKT_NO_BROWSER is read once per call rather than cached, so it can be set for
// a single command without the process having to be restarted.
var Suppressed bool

// Open launches the system browser on url, or reports why it could not.
//
// Start, not Run: the point is to hand the page over and carry on, and waiting
// for the browser to exit would hang the step that opens it.
func Open(url string) error {
	if Suppressed || strings.TrimSpace(os.Getenv("KAKT_NO_BROWSER")) != "" || strings.TrimSpace(url) == "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return fmt.Errorf("no browser opener for %s", runtime.GOOS)
	}
}
