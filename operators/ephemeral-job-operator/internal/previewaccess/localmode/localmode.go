// Package localmode is preview-access's local development mode (ADR-108,
// "Local development mode"): everything local about the service, and nothing
// else.
//
// It exists so a developer can verify code builds and the preview logic on a
// Kind cluster: records, hosts, Ready ranking, the codes and state endpoints,
// proxying (WebSocket included) and the `ended`/`unavailable` refusals. It
// deliberately does NOT reproduce what only a real environment can prove —
// origin isolation on a separate registrable domain, login, the handoff and
// the partitioned cookie. Those are verified in dev.
//
// It refuses to run anywhere but Kind (RequireKind), so it cannot be switched
// on in a real environment by mistake. The manifests and scripts that use it
// are under zero-ops/scripts/local/.
package localmode

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
	"github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/internal/previewaccess"
)

// Config is local mode's whole configuration, from the environment.
type Config struct {
	// Namespace is where the local application's records live.
	Namespace string
	// AppOrigins are the local applications' origins, e.g.
	// http://localhost:3002 and http://localhost:3007: the Origins the codes
	// endpoint accepts, and where refusal pages post. Several applications
	// share one local namespace and one development identity.
	AppOrigins []string
	// Subject is the identity every local request acts as: the subject of the
	// application's development identity.
	Subject string
	// PreviewBase is the URL a preview is opened at, with {host} for the
	// record's host, e.g. http://{host}:3010.
	PreviewBase string
}

// ConfigFromEnv reads Config. Every value is required: a missing one is named.
func ConfigFromEnv() (Config, error) {
	c := Config{
		Namespace:   os.Getenv("LOCAL_NAMESPACE"),
		Subject:     os.Getenv("LOCAL_SUBJECT"),
		PreviewBase: os.Getenv("LOCAL_PREVIEW_BASE"),
	}
	origins := os.Getenv("LOCAL_APP_ORIGINS")
	for name, v := range map[string]string{
		"LOCAL_NAMESPACE": c.Namespace, "LOCAL_APP_ORIGINS": origins,
		"LOCAL_SUBJECT": c.Subject, "LOCAL_PREVIEW_BASE": c.PreviewBase,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("local mode requires %s", name)
		}
	}
	// Comma-separated, each an exact origin: scheme, host and port, no path.
	// An origin is compared byte for byte with the browser's Origin header,
	// so anything else would never match and is refused here instead.
	seen := map[string]bool{}
	for _, o := range strings.Split(origins, ",") {
		o = strings.TrimSpace(o)
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return Config{}, fmt.Errorf("LOCAL_APP_ORIGINS: %q is not an origin (scheme://host[:port], nothing after)", o)
		}
		if seen[o] {
			return Config{}, fmt.Errorf("LOCAL_APP_ORIGINS lists %q twice", o)
		}
		seen[o] = true
		c.AppOrigins = append(c.AppOrigins, o)
	}
	if !strings.Contains(c.PreviewBase, "{host}") {
		return Config{}, fmt.Errorf("LOCAL_PREVIEW_BASE %q must contain {host}", c.PreviewBase)
	}
	return c, nil
}

// RequireKind refuses to proceed unless every node of the cluster is a Kind
// node. This is what makes local mode impossible to run in a real environment.
func RequireKind(ctx context.Context, r client.Reader) error {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return fmt.Errorf("listing nodes to confirm this is a Kind cluster: %w", err)
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("local mode runs only on a Kind cluster; this cluster reports no nodes")
	}
	for _, n := range nodes.Items {
		if !strings.HasPrefix(n.Spec.ProviderID, "kind://") {
			return fmt.Errorf("local mode runs only on a Kind cluster; node %s is %q", n.Name, n.Spec.ProviderID)
		}
	}
	return nil
}

// Identity is local mode's previewaccess.Identity: every caller is the
// configured development identity, and the application is whichever
// configured origin the request names. A request naming none, or another, has
// no application: the codes endpoint then refuses it. Every local app shares
// one namespace, so the caller carries no tenant or app: a record's
// embeddableBy has nothing to widen locally.
type Identity struct{ Config Config }

func (i Identity) Caller(r *http.Request) (previewaccess.Caller, error) {
	c := previewaccess.Caller{Namespace: i.Config.Namespace, Subject: i.Config.Subject}
	if o := r.Header.Get("Origin"); slices.Contains(i.Config.AppOrigins, o) {
		c.Origin = o
	}
	return c, nil
}

func (i Identity) AppOrigins(context.Context, string, []string) ([]string, error) {
	return i.Config.AppOrigins, nil
}

// Session is local mode's previewaccess.Session: no handoff and no cookie.
// A preview request is admitted when the record admits the development
// identity; every other check (the record exists and is current, its target
// is running) is the server's and applies unchanged.
type Session struct{ Config Config }

func (s Session) Begin(rec *computev1alpha1.PreviewAccess, _ previewaccess.Caller, now time.Time) (string, time.Duration, error) {
	return strings.ReplaceAll(s.Config.PreviewBase, "{host}", rec.Status.Host) + "/",
		previewaccess.SessionLifetime(rec, now), nil
}

func (Session) ServeOwn(http.ResponseWriter, *http.Request, string, previewaccess.RefuseFunc) bool {
	return false
}

func (s Session) Admit(_ *http.Request, rec *computev1alpha1.PreviewAccess) (string, string, string) {
	// No embedder is known from a preview request (there is no cookie to
	// carry it), so the origin is left to the server, which posts a refusal
	// to every configured application.
	if !previewaccess.Allowed(rec, s.Config.Subject) {
		return s.Config.Subject, "", previewaccess.StateDenied
	}
	return s.Config.Subject, "", ""
}

// Holds: with no cookie, an allowed identity is always admitted.
func (Session) Holds(*computev1alpha1.PreviewAccess, string, time.Time) (bool, *time.Time) {
	return true, nil
}

func (Session) WasAdmitted(*computev1alpha1.PreviewAccess, string) bool { return false }
