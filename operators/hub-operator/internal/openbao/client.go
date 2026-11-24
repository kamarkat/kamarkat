// Package openbao is hub-operator's client for the box's secret store (ADR-117).
//
// hub-operator is an ordinary in-cluster client: it reaches OpenBao by service DNS
// over TLS verified against the listener CA (openbao-listener-trust), and logs in
// through Kubernetes auth with a projected ServiceAccount token (audience
// "openbao"), re-read on every login so token rotation needs no restart. It holds
// the hub-operator policy only: no root, recovery-key or token-sweep rights.
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
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultAddr      = "https://openbao.platform-data.svc:8200"
	defaultCACert    = "/etc/openbao/trust/ca.crt"
	defaultTokenFile = "/var/run/secrets/openbao/token"
	defaultRole      = "hub-operator"
	serverName       = "openbao.platform-data.svc"
)

// ErrNotFound is a 404: absent, as distinct from "could not ask".
var ErrNotFound = errors.New("not found")

// APIError is any other non-2xx answer.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("OpenBao answered %d: %s", e.Status, e.Body) }

// Client is safe for concurrent use by the controllers.
type Client struct {
	addr      string
	role      string
	tokenFile string
	http      *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewFromEnv builds the client from OPENBAO_ADDR, OPENBAO_CACERT,
// OPENBAO_TOKEN_FILE and OPENBAO_ROLE, defaulting to the in-cluster contract
// (config/manager/manager.yaml mounts the trust ConfigMap and the projected token).
func NewFromEnv() (*Client, error) {
	get := func(k, d string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return d
	}
	caPath := get("OPENBAO_CACERT", defaultCACert)
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read the OpenBao listener CA (%s): %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%s holds no certificate", caPath)
	}
	return &Client{
		addr:      strings.TrimSuffix(get("OPENBAO_ADDR", defaultAddr), "/"),
		role:      get("OPENBAO_ROLE", defaultRole),
		tokenFile: get("OPENBAO_TOKEN_FILE", defaultTokenFile),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12,
			}},
		},
	}, nil
}

func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	jwt, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read the projected ServiceAccount token (%s): %w", c.tokenFile, err)
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := c.raw(ctx, http.MethodPost, "auth/kubernetes/login", "",
		map[string]any{"role": c.role, "jwt": strings.TrimSpace(string(jwt))}, &out); err != nil {
		return "", fmt.Errorf("log in to OpenBao as %s: %w", c.role, err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("OpenBao issued no token for role %s", c.role)
	}
	c.token = out.Auth.ClientToken
	c.expires = time.Now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second)
	return c.token, nil
}

// Invalidate drops the cached token, e.g. after recovery revoked it.
func (c *Client) Invalidate() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return err
	}
	err = c.raw(ctx, method, path, tok, body, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden && strings.Contains(ae.Body, "permission denied") {
		// A token revoked underneath us (restored-store recovery sweeps every
		// restored token): log in again once.
		c.Invalidate()
		if tok, err = c.ensureToken(ctx); err != nil {
			return err
		}
		err = c.raw(ctx, method, path, tok, body, out)
	}
	return err
}

func (c *Client) raw(ctx context.Context, method, path, token string, body, out any) error {
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
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
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
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode OpenBao response from %s: %w", path, err)
		}
	}
	return nil
}

// Read, Write, List and Delete are the generic calls.
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
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// NewWithClient is a client for addr using hc and an already-issued token, for
// callers that authenticate elsewhere (and tests against a fake server).
func NewWithClient(addr string, hc *http.Client, token string) *Client {
	return &Client{addr: strings.TrimSuffix(addr, "/"), http: hc, token: token,
		expires: time.Now().Add(24 * time.Hour)}
}
