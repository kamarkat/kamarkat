package previewaccess

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// The platform's two mechanisms (ADR-108 §3, §4): the caller is a tenant
// gateway pod presenting a verified session credential, and a browser is
// admitted by a single-use handoff that sets a signed, partitioned cookie.

// ─── Identity: tenant gateway plus verified session ────────────────────────

// Callers resolves which application namespace a connection on the app API
// came from, by the source address of the tenant gateway pod that made it.
//
// The namespace is never taken from the request: a header or path naming it
// would be something any caller could write. A gateway pod's address is
// assigned by the cluster, and the platform renders every gateway's routes, so
// "this connection came from namespace X's gateway" is a fact, not a claim.
type Callers interface {
	NamespaceFor(ip string) (string, bool)
}

// GatewayIdentity is the platform's Identity.
type GatewayIdentity struct {
	Reader  client.Reader
	Callers Callers
	Verify  *Verifier
	Log     logr.Logger
}

func (g *GatewayIdentity) Caller(r *http.Request) (Caller, error) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	ns, ok := g.Callers.NamespaceFor(ip)
	if !ok {
		g.Log.Info("audit", "event", "api-refused", "reason", "not a tenant gateway", "remote", ip)
		return Caller{}, &Refused{Status: http.StatusForbidden, Reason: "not a tenant gateway"}
	}
	var n corev1.Namespace
	if err := g.Reader.Get(r.Context(), client.ObjectKey{Name: ns}, &n); err != nil {
		return Caller{}, fmt.Errorf("reading the caller's namespace %s: %w", ns, err)
	}
	origin := n.Annotations[AppOriginAnnotation]
	tenant, app := n.Labels[LabelTenant], n.Labels[LabelApp]
	if origin == "" || tenant == "" || app == "" {
		g.Log.Info("audit", "event", "api-refused", "reason", "namespace is not a preview-enabled application", "namespace", ns)
		return Caller{}, &Refused{Status: http.StatusForbidden, Reason: "namespace declares no app origin, tenant or app"}
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return Caller{}, &Refused{Status: http.StatusUnauthorized, Reason: "no session credential"}
	}
	claims, err := g.Verify.Verify(r.Context(), raw)
	if err != nil {
		g.Log.Info("audit", "event", "api-refused", "reason", err.Error(), "namespace", ns)
		return Caller{}, &Refused{Status: http.StatusUnauthorized, Reason: err.Error()}
	}
	return Caller{Namespace: ns, Tenant: tenant, App: app, Origin: origin, Subject: claims.Subject}, nil
}

// AppOrigins reads the origin the universal-tenant chart declares on each
// application namespace that may embed the preview: the record's own, and
// those of the apps of its tenant it names in embeddableBy. On the platform
// each application has its own namespace. One that is gone, or declares no
// origin, has no app to post to.
func (g *GatewayIdentity) AppOrigins(ctx context.Context, namespace string, embeddableBy []string) ([]string, error) {
	var n corev1.Namespace
	if err := g.Reader.Get(ctx, client.ObjectKey{Name: namespace}, &n); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	var origins []string
	add := func(n *corev1.Namespace) {
		if o := n.Annotations[AppOriginAnnotation]; o != "" && !slices.Contains(origins, o) {
			origins = append(origins, o)
		}
	}
	add(&n)
	tenant := n.Labels[LabelTenant]
	if tenant == "" {
		return origins, nil
	}
	for _, app := range embeddableBy {
		ns, err := AppNamespace(ctx, g.Reader, tenant, app)
		if err != nil {
			return nil, err
		}
		if ns == "" {
			continue
		}
		var sib corev1.Namespace
		if err := g.Reader.Get(ctx, client.ObjectKey{Name: ns}, &sib); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return nil, err
		}
		add(&sib)
	}
	return origins, nil
}

// ─── Session: single-use handoff, then a signed partitioned cookie ─────────

// CookieSession is the platform's Session.
type CookieSession struct {
	Index  *Index
	Codes  *Codes
	Signer *Signer
	Log    logr.Logger

	mu       sync.Mutex
	redeemed map[redeemKey]time.Time // newest cookie expiry per (record, subject)
}

type redeemKey struct {
	record  types.UID
	subject string
}

func (s *CookieSession) Begin(rec *computev1alpha1.PreviewAccess, c Caller, now time.Time) (string, time.Duration, error) {
	lifetime := SessionLifetime(rec, now)
	k, err := s.Codes.Issue(ticket{
		Record: string(rec.UID), Host: rec.Status.Host, Subject: c.Subject,
		Expiry: now.Add(lifetime).Unix(), Origin: c.Origin,
	})
	if err != nil {
		return "", 0, err
	}
	return "https://" + rec.Status.Host + "/_handoff?code=" + url.QueryEscape(k), lifetime, nil
}

func (s *CookieSession) ServeOwn(w http.ResponseWriter, r *http.Request, host string, refuse RefuseFunc) bool {
	if r.URL.Path != "/_handoff" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	t, err := s.Codes.Redeem(r.URL.Query().Get("code"), host)
	if err != nil {
		s.Log.Info("audit", "event", "handoff-refused", "host", host, "reason", err.Error())
		refuse(StateExpired, "")
		return true
	}
	rec, err := s.Index.ByHost(r.Context(), host)
	if err != nil || rec == nil || string(rec.UID) != t.Record || !Allowed(rec, t.Subject) {
		s.Log.Info("audit", "event", "handoff-refused", "host", host, "subject", t.Subject, "reason", "record changed since the code was issued")
		refuse(StateRevoked, t.Origin)
		return true
	}
	exp := time.Unix(t.Expiry, 0)
	http.SetCookie(w, s.Signer.cookie(t, time.Until(exp)))
	s.noteRedeemed(rec.UID, t.Subject, exp)
	s.Log.Info("audit", "event", "handoff-redeemed", "namespace", rec.Namespace, "preview", rec.Name, "subject", t.Subject, "host", host)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return true
}

func (s *CookieSession) Admit(r *http.Request, rec *computev1alpha1.PreviewAccess) (string, string, string) {
	var t ticket
	ok := false
	if ck, err := r.Cookie(CookieName); err == nil {
		if t, err = s.Signer.open(ck.Value); err == nil {
			ok = t.Record == string(rec.UID) && t.Host == rec.Status.Host && time.Now().Before(time.Unix(t.Expiry, 0))
		}
	}
	switch {
	case !ok:
		return t.Subject, t.Origin, StateExpired
	case !Allowed(rec, t.Subject):
		s.Log.Info("audit", "event", "request-refused", "namespace", rec.Namespace, "preview", rec.Name, "subject", t.Subject, "state", StateRevoked)
		return t.Subject, t.Origin, StateRevoked
	}
	return t.Subject, t.Origin, ""
}

func (s *CookieSession) Holds(rec *computev1alpha1.PreviewAccess, subject string, now time.Time) (bool, *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.redeemed[redeemKey{rec.UID, subject}]
	if !ok || !now.Before(until) {
		return false, nil
	}
	return true, &until
}

func (s *CookieSession) WasAdmitted(rec *computev1alpha1.PreviewAccess, subject string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.redeemed[redeemKey{rec.UID, subject}]
	return ok
}

func (s *CookieSession) noteRedeemed(rec types.UID, subject string, exp time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redeemed == nil {
		s.redeemed = map[redeemKey]time.Time{}
	}
	k := redeemKey{rec, subject}
	if exp.After(s.redeemed[k]) {
		s.redeemed[k] = exp
	}
}
