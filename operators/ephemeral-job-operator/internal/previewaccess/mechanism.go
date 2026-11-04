package previewaccess

import (
	"context"
	"net/http"
	"time"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// The two decisions that differ between environments, as interfaces.
//
// Everything else — records, hosts, Ready ranking, the state machine, the
// refusal shapes, the proxy — is one implementation. What varies is only how a
// caller is identified and how a browser is admitted to a preview host. The
// platform implementations (gateway identity, handoff-and-cookie session) are
// in platform.go; any other implementation lives in its own package, so this
// package never branches on an environment.

// Caller is who is asking the app API, and from which application.
type Caller struct {
	Namespace string // the application's namespace: its own records are here
	// Tenant and App are the application's ADR-088 identity, by which records
	// of sibling apps that declare it in spec.embeddableBy are found. Both
	// empty when the caller has no application identity (one namespace, no
	// siblings): then only Namespace is searched.
	Tenant  string
	App     string
	Origin  string // the application's origin: the only Origin accepted
	Subject string // the issuer subject the request acts as
}

// Refused turns an app-API caller away with Status.
type Refused struct {
	Status int
	Reason string
}

func (r *Refused) Error() string { return r.Reason }

// Identity establishes the caller of the app API.
type Identity interface {
	// Caller identifies r's caller, or returns a *Refused.
	Caller(r *http.Request) (Caller, error)
	// AppOrigins are the origins of the applications that may embed a
	// preview held in namespace and declared embeddable by embeddableBy —
	// where a refusal page posts its state. Empty when none declares one.
	AppOrigins(ctx context.Context, namespace string, embeddableBy []string) ([]string, error)
}

// RefuseFunc writes a refusal for state, addressed to origin.
type RefuseFunc func(state, origin string)

// Session is how a browser is admitted to a preview host.
type Session interface {
	// Begin is where the canvas points the iframe for rec, and how long the
	// browser's admission lasts from now.
	Begin(rec *computev1alpha1.PreviewAccess, c Caller, now time.Time) (url string, lifetime time.Duration, err error)
	// ServeOwn handles a preview-host request that belongs to the admission
	// mechanism itself (the handoff), and reports whether it did.
	ServeOwn(w http.ResponseWriter, r *http.Request, host string, refuse RefuseFunc) bool
	// Admit decides whether r may reach rec. state is "" to admit, or the
	// refusal state; subject is the requester; origin the app to notify.
	Admit(r *http.Request, rec *computev1alpha1.PreviewAccess) (subject, origin, state string)
	// Holds reports whether subject is admitted to rec now, and until when
	// (nil: no bound).
	Holds(rec *computev1alpha1.PreviewAccess, subject string, now time.Time) (bool, *time.Time)
	// WasAdmitted reports whether subject has ever been admitted to rec — what
	// tells `revoked` from `denied`.
	WasAdmitted(rec *computev1alpha1.PreviewAccess, subject string) bool
}

// SessionLifetime is how long an admission to rec may last from now: the
// cookie bound, never past the record's own expiry.
func SessionLifetime(rec *computev1alpha1.PreviewAccess, now time.Time) time.Duration {
	d := CookieMaxAge
	if rec.Spec.ExpiresAt != nil {
		if left := rec.Spec.ExpiresAt.Sub(now).Truncate(time.Second); left < d {
			d = left
		}
	}
	return d
}
