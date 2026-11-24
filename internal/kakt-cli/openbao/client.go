package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Audience every OpenBao role requires (ADR-117 §4).
	Audience = "openbao"
	// ServerName is what the listener certificate is issued for and what every
	// client verifies, port-forwarded or not.
	ServerName = "openbao.platform-data.svc"
	activeSvc  = "openbao-active"
)

// Client talks to the box's OpenBao through a kubectl port-forward, verifying the
// listener against the CA published in openbao-listener-trust. It never skips TLS
// verification.
type Client struct {
	addr  string
	http  *http.Client
	token string
	stop  func()
}

// Connect opens a port-forward to the active OpenBao node and a verified TLS client.
func Connect(ctx context.Context, cs kubernetes.Interface, kubeconfig string) (*Client, error) {
	cm, err := cs.CoreV1().ConfigMaps(Namespace).Get(ctx, ListenerTrustConfig, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read the OpenBao listener trust (%s/%s): %w", Namespace, ListenerTrustConfig, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cm.Data["ca.crt"])) {
		return nil, fmt.Errorf("%s/%s holds no certificate", Namespace, ListenerTrustConfig)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	pfCtx, cancel := context.WithCancel(context.Background())
	args := []string{"port-forward", "-n", Namespace, "svc/" + activeSvc, strconv.Itoa(port) + ":8200"}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	cmd := exec.CommandContext(pfCtx, "kubectl", args...)
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start port-forward to %s/%s: %w", Namespace, activeSvc, err)
	}
	c := &Client{
		addr: "https://127.0.0.1:" + strconv.Itoa(port),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs: pool, ServerName: ServerName, MinVersion: tls.VersionTLS12,
			}},
		},
		stop: func() { cancel(); _ = cmd.Wait() },
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if conn, derr := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond); derr == nil {
			conn.Close()
			return c, nil
		}
		if time.Now().After(deadline) {
			c.Close()
			return nil, fmt.Errorf("port-forward to %s/%s did not become ready", Namespace, activeSvc)
		}
		select {
		case <-ctx.Done():
			c.Close()
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Close stops the port-forward.
func (c *Client) Close() {
	if c.stop != nil {
		c.stop()
		c.stop = nil
	}
}

// LoginAs authenticates through Kubernetes auth as role, with a fresh, short-lived
// token for serviceAccount in Namespace. Requesting that token needs
// `create serviceaccounts/token`, which only cluster administrators hold for the
// narrow identities (openbao-platform.yaml).
func (c *Client) LoginAs(ctx context.Context, cs kubernetes.Interface, serviceAccount, role string) error {
	exp := int64(600)
	tr, err := cs.CoreV1().ServiceAccounts(Namespace).CreateToken(ctx, serviceAccount, &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{Audiences: []string{Audience}, ExpirationSeconds: &exp},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("request a token for %s/%s: %w", Namespace, serviceAccount, err)
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, "auth/kubernetes/login",
		map[string]any{"role": role, "jwt": tr.Status.Token}, &out); err != nil {
		return fmt.Errorf("log in to OpenBao as %s: %w", role, err)
	}
	if out.Auth.ClientToken == "" {
		return fmt.Errorf("OpenBao issued no token for role %s", role)
	}
	c.token = out.Auth.ClientToken
	return nil
}

// Status is sys/seal-status, which needs no token.
type Status struct {
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	Version     string `json:"version"`
}

func (c *Client) SealStatus(ctx context.Context) (Status, error) {
	var s Status
	err := c.do(ctx, http.MethodGet, "sys/seal-status", nil, &s)
	return s, err
}

// ErrNotFound reports a 404 from OpenBao, so callers can tell "absent" from
// "could not ask".
var ErrNotFound = errors.New("not found")

// APIError is any non-2xx response other than 404.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("OpenBao answered %d: %s", e.Status, e.Body) }

// Read, Write, List and Delete are the four calls the CLI needs.
func (c *Client) Read(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}
func (c *Client) Write(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}
func (c *Client) List(ctx context.Context, path string, out any) error {
	return c.do(ctx, "LIST", path, nil, out)
}
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+"/v1/"+strings.TrimPrefix(path, "/"), rdr)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// Error bodies from OpenBao carry messages, never secret values.
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode OpenBao response from %s: %w", path, err)
		}
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free local port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
