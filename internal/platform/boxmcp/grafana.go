package boxmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GrafanaProxy relays /grafana to the box's mcp-grafana (ADR-116 §1).
//
// mcp-grafana authenticates its caller with a static token, which cannot be
// tied to a person, a role or a revocation -- so it is never exposed. This
// proxy is its only caller: it has already authenticated the person and checked
// they hold platform.observer, and it presents the static token in their place.
// The token is read from its Secret, never from the request, and the person's
// own token is not forwarded.
type GrafanaProxy struct {
	target *url.URL

	core       kubernetes.Interface
	secretNS   string
	secretName string

	mu      sync.Mutex
	token   string
	fetched time.Time
}

// NewGrafanaProxy proxies to target (mcp-grafana's /mcp), with the caller token
// from the Secret namespace/name, key `password` (an ESO Password generator).
func NewGrafanaProxy(target string, core kubernetes.Interface, secretRef string) (*GrafanaProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("mcp-grafana address %q: %w", target, err)
	}
	ns, name, ok := strings.Cut(secretRef, "/")
	if !ok {
		return nil, fmt.Errorf("the caller token secret must be namespace/name, got %q", secretRef)
	}
	return &GrafanaProxy{target: u, core: core, secretNS: ns, secretName: name}, nil
}

func (p *GrafanaProxy) callerToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && time.Since(p.fetched) < 5*time.Minute {
		return p.token, nil
	}
	s, err := p.core.CoreV1().Secrets(p.secretNS).Get(ctx, p.secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read mcp-grafana's caller token: %w", err)
	}
	t := string(s.Data["password"])
	if t == "" {
		return "", fmt.Errorf("secret %s/%s holds no password", p.secretNS, p.secretName)
	}
	p.token, p.fetched = t, time.Now()
	return t, nil
}

// ServeHTTP relays one request. The caller is authenticated and authorised
// before this is reached.
func (p *GrafanaProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, err := p.callerToken(r.Context())
	if err != nil {
		http.Error(w, "Grafana is not available on this box: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// A proxy per request: the token is this request's, and a shared proxy's
	// Rewrite would be written by one request while another reads it.
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(p.target)
			pr.Out.URL.Path = p.target.Path
			pr.Out.Host = p.target.Host
			pr.Out.Header.Del("Cookie")
			// mcp-grafana refuses a request carrying an Origin it does not allow;
			// this is a server-to-server call and has none of its own.
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		FlushInterval: -1, // stream SSE responses as they arrive
	}
	rp.ServeHTTP(w, r)
}
