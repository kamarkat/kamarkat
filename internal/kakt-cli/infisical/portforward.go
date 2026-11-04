package infisical

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PortForwardManager manages a kubectl port-forward subprocess to tunnel
// into the Infisical service from a local port. The Go binary uses this
// to reach the Infisical API without depending on the shell script's
// background port-forward (which dies when the Infisical pod restarts).
type PortForwardManager struct {
	namespace  string
	svcName    string
	localPort  string
	remotePort string
	kubeconfig string
	cmd        *exec.Cmd
	stopFn     context.CancelFunc
}

func NewPortForwardManager(namespace, svcName, localPort, remotePort, kubeconfig string) *PortForwardManager {
	return &PortForwardManager{
		namespace:  namespace,
		svcName:    svcName,
		localPort:  localPort,
		remotePort: remotePort,
		kubeconfig: kubeconfig,
	}
}

func (pf *PortForwardManager) LocalURL() string {
	return "http://localhost:" + pf.localPort
}

// portInUse reports whether something is already listening locally.
func portInUse(port string) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// findFreePort returns the preferred port, or the next free one above it.
//
// The local port is an implementation detail of reaching the service — nothing
// outside this process depends on which number is used, so an occupied port is no
// reason to fail, and certainly no reason to disturb whatever owns it. Developer
// machines routinely have dev servers on 8081.
//
// This matters because kubectl port-forward does not fail fast when it cannot
// bind: it can outlive the caller's wait, leaving IsRunning() true while the OTHER
// process answers. A run hit exactly that — a local dev server returned
// 200 text/html for every path, and the bootstrap failed much later with
// "failed to decode login response: invalid character '<'".
func findFreePort(preferred string) (string, error) {
	start, err := strconv.Atoi(preferred)
	if err != nil {
		return "", fmt.Errorf("invalid local port %q: %w", preferred, err)
	}
	const scan = 50
	for candidate := start; candidate < start+scan; candidate++ {
		if !portInUse(strconv.Itoa(candidate)) {
			return strconv.Itoa(candidate), nil
		}
	}
	return "", fmt.Errorf("no free local port in range %d-%d", start, start+scan-1)
}

func (pf *PortForwardManager) Start() error {
	// Re-select every start: a port that was free last time may not be now, and a
	// restarted port-forward must not silently reuse a number someone else took.
	port, err := findFreePort(pf.localPort)
	if err != nil {
		return err
	}
	if port != pf.localPort {
		fmt.Printf("[port-forward] local port %s is in use; using %s instead\n", pf.localPort, port)
		pf.localPort = port
	}

	args := []string{"port-forward", "-n", pf.namespace, "svc/" + pf.svcName, pf.localPort + ":" + pf.remotePort}
	if pf.kubeconfig != "" {
		args = append([]string{"--kubeconfig", pf.kubeconfig}, args...)
	}

	// Create a cancellable context so Stop() can kill the subprocess
	ctx, cancel := context.WithCancel(context.Background())
	pf.stopFn = cancel

	pf.cmd = exec.CommandContext(ctx, "kubectl", withKubeconfig(args...)...)
	if err := pf.cmd.Start(); err != nil {
		cancel()
		pf.stopFn = nil
		return fmt.Errorf("start port-forward: %w", err)
	}

	return nil
}

func (pf *PortForwardManager) IsRunning() bool {
	if pf.cmd == nil || pf.cmd.Process == nil {
		return false
	}
	return pf.cmd.ProcessState == nil || !pf.cmd.ProcessState.Exited()
}

func (pf *PortForwardManager) Stop() {
	if pf.stopFn != nil {
		pf.stopFn()
		pf.stopFn = nil
	}
	if pf.cmd != nil && pf.cmd.Process != nil {
		pf.cmd.Process.Kill()
	}
}

func (pf *PortForwardManager) WaitForAPI(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	checkURL := pf.LocalURL() + "/api/v1/auth/checkAuth"

	for time.Now().Before(deadline) {
		if !pf.IsRunning() {
			return fmt.Errorf("port-forward process exited before API became available")
		}

		resp, err := http.Get(checkURL)
		if err == nil {
			ct := resp.Header.Get("Content-Type")
			resp.Body.Close()

			// Status is deliberately not checked — 401 from checkAuth is a healthy
			// unauthenticated response. What must be checked is WHO answered.
			//
			// "Any response means the API is reachable" was wrong: if another
			// process already holds the local port, kubectl port-forward cannot bind
			// and that process answers instead. A local dev server returning
			// 200 text/html for every path satisfied this probe, and the bootstrap
			// then spoke to it, failing later with the genuinely baffling
			// "failed to decode login response: invalid character '<'".
			if strings.Contains(ct, "application/json") {
				return nil
			}
			return fmt.Errorf("port %s is answering but is not Infisical (Content-Type %q).\n"+
				"Another process is bound to that port — kubectl port-forward could not take it.\n"+
				"Free it (lsof -nP -iTCP:%s -sTCP:LISTEN) and re-run",
				pf.localPort, ct, pf.localPort)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return fmt.Errorf("timeout after %v waiting for Infisical API at %s", timeout, checkURL)
}

// EnsureAPIAccess starts the port-forward (if not running), waits for the API
// to respond, and sets INFISICAL_API_URL so infisical.NewClient picks it up.
func (pf *PortForwardManager) EnsureAPIAccess(ctx context.Context) error {
	if !pf.IsRunning() {
		fmt.Println("[port-forward] Starting kubectl port-forward to Infisical service...")
		if err := pf.Start(); err != nil {
			return fmt.Errorf("start port-forward: %w", err)
		}
		// Brief pause for the PF to bind the local port
		time.Sleep(1 * time.Second)
	} else {
		fmt.Println("[port-forward] Port-forward already running")
	}

	if err := pf.WaitForAPI(ctx, 30*time.Second); err != nil {
		return fmt.Errorf("wait for Infisical API: %w", err)
	}

	localURL := pf.LocalURL()
	if current := os.Getenv("INFISICAL_API_URL"); current != localURL {
		os.Setenv("INFISICAL_API_URL", localURL)
		fmt.Printf("[port-forward] INFISICAL_API_URL set to %s\n", localURL)
	}

	return nil
}

// RunWithPortForward wraps a function with port-forward lifecycle management.
// It starts the PF, ensures the API is reachable, runs fn, and stops the PF.
// If fn returns a connection error, the PF is restarted and fn is retried.
func RunWithPortForward(ctx context.Context, namespace, svcName, localPort, remotePort, kubeconfig string, retries int, fn func() error) error {
	pf := NewPortForwardManager(namespace, svcName, localPort, remotePort, kubeconfig)

	for attempt := 0; attempt <= retries; attempt++ {
		if err := pf.EnsureAPIAccess(ctx); err != nil {
			return fmt.Errorf("port-forward setup failed: %w", err)
		}

		err := fn()
		if err == nil {
			pf.Stop()
			return nil
		}

		if isConnectionError(err) && attempt < retries {
			fmt.Printf("[port-forward] connection error (attempt %d/%d), restarting port-forward: %v\n", attempt+1, retries, err)
			pf.Stop()
			time.Sleep(2 * time.Second)
			continue
		}

		pf.Stop()
		return err
	}

	return nil // unreachable
}

func isConnectionError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "EOF")
}
