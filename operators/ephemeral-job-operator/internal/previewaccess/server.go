package previewaccess

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// Server is the preview-access HTTP surface. It holds no environment logic:
// who is calling comes from Identity, how a browser is admitted from Session
// (mechanism.go).
type Server struct {
	Reader   client.Reader // the manager's cache
	Index    *Index
	Identity Identity
	Session  Session
	Log      logr.Logger
}

const apiPrefix = "/_platform/preview-access/v1/"

// ─── App API: behind the application's own login ───────────────────────────

// AppAPI serves the codes and state endpoints (ADR-108 §3, §3a).
func (s *Server) AppAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+apiPrefix+"codes", s.handleCodes)
	mux.HandleFunc("GET "+apiPrefix+"previews/{id}", s.handleState)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// caller identifies the request's caller, or writes the refusal itself.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	c, err := s.Identity.Caller(r)
	if err == nil {
		return c, true
	}
	var refused *Refused
	if errors.As(err, &refused) {
		writeJSON(w, refused.Status, map[string]string{"previewAccess": StateDenied})
		return Caller{}, false
	}
	s.Log.Error(err, "identifying the caller failed")
	http.Error(w, "caller lookup failed", http.StatusServiceUnavailable)
	return Caller{}, false
}

// errAmbiguous: more than one record answers the caller's previewId.
// Admission refuses the records that would collide; this is the race it can
// lose, refused rather than resolved by picking one.
var errAmbiguous = errors.New("previewId names more than one preview this application may embed")

// lookup finds the record previewId name names for c: in its own namespace,
// or one a sibling app of its tenant declared embeddable by it (ADR-108 §3b).
func (s *Server) lookup(ctx context.Context, c Caller, name string) (*computev1alpha1.PreviewAccess, error) {
	if c.App == "" {
		return s.record(ctx, c.Namespace, name)
	}
	found, err := Reachable(ctx, s.Reader, c.Tenant, c.App, name)
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return found[0], nil
	}
	s.Log.Info("audit", "event", "lookup-refused", "namespace", c.Namespace, "preview", name, "matches", len(found), "reason", errAmbiguous.Error())
	return nil, errAmbiguous
}

// lookupFailed answers a failed lookup.
func lookupFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, errAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	http.Error(w, "record lookup failed", http.StatusServiceUnavailable)
}

func (s *Server) record(ctx context.Context, ns, name string) (*computev1alpha1.PreviewAccess, error) {
	var rec computev1alpha1.PreviewAccess
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &rec); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// handleCodes starts a browser's admission to one preview (ADR-108 §3).
func (s *Server) handleCodes(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	// Same-origin JSON only: a cross-site form cannot set this content type,
	// and a cross-site fetch carries a foreign Origin.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" ||
		c.Origin == "" || r.Header.Get("Origin") != c.Origin {
		writeJSON(w, http.StatusForbidden, map[string]string{"previewAccess": StateDenied})
		return
	}
	var body struct {
		PreviewID string `json:"previewId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.PreviewID == "" {
		http.Error(w, `{"error":"previewId is required"}`, http.StatusBadRequest)
		return
	}
	rec, err := s.lookup(r.Context(), c, body.PreviewID)
	if err != nil {
		lookupFailed(w, err)
		return
	}
	now := time.Now()
	switch {
	case rec == nil || RecordEnded(rec, now):
		writeJSON(w, http.StatusNotFound, map[string]string{"previewAccess": "not-found"})
		return
	case !Allowed(rec, c.Subject):
		s.Log.Info("audit", "event", "code-refused", "namespace", c.Namespace, "preview", rec.Name, "subject", c.Subject, "state", StateDenied)
		writeJSON(w, http.StatusForbidden, map[string]string{"previewAccess": StateDenied})
		return
	case rec.Status.Host == "":
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"previewAccess": StateUnavailable})
		return
	}

	handoff, lifetime, err := s.Session.Begin(rec, c, now)
	if err != nil {
		http.Error(w, "could not start the preview session", http.StatusInternalServerError)
		return
	}
	s.Log.Info("audit", "event", "code-issued", "namespace", c.Namespace, "preview", rec.Name, "subject", c.Subject, "host", rec.Status.Host)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"handoffUrl":           handoff,
		"codeExpiresInSeconds": int(CodeTTL / time.Second),
		"cookieMaxAgeSeconds":  int(lifetime / time.Second),
	})
}

// handleState is the authoritative answer the canvas acts on (ADR-108 §3a).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	rec, err := s.lookup(r.Context(), c, id)
	if err != nil {
		lookupFailed(w, err)
		return
	}
	state, until, err := s.Index.StateFor(r.Context(), rec, c.Subject, s.Session)
	if err != nil {
		http.Error(w, "state lookup failed", http.StatusServiceUnavailable)
		return
	}
	out := map[string]any{"previewId": id, "state": state, "host": nil, "cookieExpiresAt": nil}
	if rec != nil && state != StateEnded {
		out["host"] = rec.Status.Host
	}
	if until != nil {
		out["cookieExpiresAt"] = until.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// ─── Preview hosts: the untrusted origin ───────────────────────────────────

// Previews serves every preview host: the Session's own requests, then every
// request checked and proxied to the sandbox.
func (s *Server) Previews() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(strings.Split(r.Host, ":")[0])
		refuse := func(state, origin string) { s.refuse(w, r, host, state, origin) }
		if s.Session.ServeOwn(w, r, host, refuse) {
			return
		}
		s.handlePreview(w, r, host)
	})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request, host string) {
	rec, err := s.Index.ByHost(r.Context(), host)
	if err != nil {
		http.Error(w, "preview lookup failed", http.StatusServiceUnavailable)
		return
	}
	if rec == nil || RecordEnded(rec, time.Now()) {
		s.refuse(w, r, host, StateEnded, "")
		return
	}
	_, origin, state := s.Session.Admit(r, rec)
	if state != "" {
		s.refuse(w, r, host, state, origin)
		return
	}
	target, serving, err := s.Index.Resolve(r.Context(), rec)
	if err != nil {
		http.Error(w, "target lookup failed", http.StatusServiceUnavailable)
		return
	}
	if !serving {
		s.refuse(w, r, host, StateUnavailable, origin)
		return
	}
	s.proxy(target.Addr).ServeHTTP(w, r)
}

// proxy forwards to the sandbox, HTTP and WebSocket alike, with the preview
// cookie removed on the way in and the platform's headers removed on the way
// out: the sandbox never sees the credential, and cannot forge a refusal or
// overwrite the cookie.
func (s *Server) proxy(addr string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: addr})
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			stripCookie(pr.Out.Header, CookieName)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("X-Preview-Access")
			kept := resp.Header.Values("Set-Cookie")[:0:0]
			for _, v := range resp.Header.Values("Set-Cookie") {
				if !strings.HasPrefix(strings.TrimSpace(v), CookieName+"=") {
					kept = append(kept, v)
				}
			}
			resp.Header.Del("Set-Cookie")
			for _, v := range kept {
				resp.Header.Add("Set-Cookie", v)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.Log.Info("upstream error", "addr", addr, "error", err.Error())
			w.Header().Set("X-Preview-Access", StateUnavailable)
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	}
}

func stripCookie(h http.Header, name string) {
	var kept []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			if p := strings.TrimSpace(part); p != "" && !strings.HasPrefix(p, name+"=") {
				kept = append(kept, p)
			}
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

var refusalPage = template.Must(template.New("r").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Preview unavailable</title></head><body>
{{if .Origins}}<script nonce="{{.Nonce}}">
{{range .Origins}}parent.postMessage({type: "zero-ops.preview-access", state: {{$.State}}, previewId: {{$.Preview}}}, {{.}});
{{end}}</script>{{end}}
</body></html>
`))

// refuse writes the refusal for state, in the shape ADR-108 §6 fixes: the
// header on every refusal; for a document, a page posting the state to the
// embedding app (a hint -- the canvas confirms against the state endpoint);
// otherwise a JSON body. When the embedder is not known from the request, the
// page posts to every origin that may embed it: postMessage with a non-matching
// targetOrigin is dropped by the browser, so only the actual parent receives it.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, host, state, origin string) {
	code := http.StatusForbidden
	switch state {
	case StateExpired:
		code = http.StatusUnauthorized
	case StateUnavailable:
		code = http.StatusServiceUnavailable
	}
	preview, ns := "", ""
	var origins, embeddableBy []string
	if origin != "" {
		origins = []string{origin}
	}
	rec, err := s.Index.ByHost(r.Context(), host)
	if err != nil {
		s.Log.Error(err, "refusal: record lookup failed", "host", host)
		http.Error(w, "preview lookup failed", http.StatusServiceUnavailable)
		return
	}
	if rec != nil {
		preview, ns, embeddableBy = rec.Name, rec.Namespace, rec.Spec.EmbeddableBy
	} else if n, e, ok := s.Index.endedRecord(host); ok {
		ns, embeddableBy = n, e
	}
	if len(origins) == 0 && ns != "" {
		if origins, err = s.Identity.AppOrigins(r.Context(), ns, embeddableBy); err != nil {
			s.Log.Error(err, "refusal: app origin lookup failed", "namespace", ns)
			http.Error(w, "preview lookup failed", http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("X-Preview-Access", state)
	w.Header().Set("Cache-Control", "no-store")
	dest := r.Header.Get("Sec-Fetch-Dest")
	if dest == "document" || dest == "iframe" {
		nonce := make([]byte, 16)
		_, _ = rand.Read(nonce)
		n := base64.RawStdEncoding.EncodeToString(nonce)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; script-src 'nonce-%s'", n))
		w.WriteHeader(code)
		_ = refusalPage.Execute(w, map[string]any{"State": state, "Preview": preview, "Origins": origins, "Nonce": n})
		return
	}
	writeJSON(w, code, map[string]string{"previewAccess": state})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
