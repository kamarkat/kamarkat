package previewaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/kamarkat/kamarkat/operators/ephemeral-job-operator/api/v1alpha1"
)

// The states a caller can be in, for one preview (ADR-108 §3a, §6).
const (
	StateActive      = "active"
	StateExpired     = "expired"
	StateDenied      = "denied"
	StateRevoked     = "revoked"
	StateEnded       = "ended"
	StateUnavailable = "unavailable"
)

// Field indexes on the record cache. Every lookup on the request path is one
// of these, against memory fed by the watch -- nothing on that path calls the
// API server (ADR-108 §5).
const (
	IndexHost   = "status.host"
	IndexTarget = "spec.target" // <jobRef>/<port>, for one-record-per-target
	IndexJobRef = "spec.jobRef"
	// IndexEmbeddableBy is <appId>/<name> for each app a record names in
	// spec.embeddableBy: how a sibling app's gateway finds it by name.
	IndexEmbeddableBy = "spec.embeddableBy"
)

// The ADR-088 labels the universal-tenant chart sets on every application
// namespace: which tenant, and which of its applications.
const (
	LabelTenant = "tenant-id"
	LabelApp    = "app-id"
)

// EmbedKey is an IndexEmbeddableBy value.
func EmbedKey(app, name string) string { return app + "/" + name }

// AppOriginAnnotation is set by the universal-tenant chart on each application
// namespace: the origin a refusal page may post to, and the only Origin the
// codes endpoint accepts.
const AppOriginAnnotation = "preview-access.zero-ops.io/app-origin"

// TargetKey is the IndexTarget value of a record.
func TargetKey(jobRef string, port int32) string { return fmt.Sprintf("%s/%d", jobRef, port) }

// HostFor is a record's preview host. Derived from the record's UID, so it is
// fixed for the record's whole life, survives restarts without being stored
// anywhere but status, and is not guessable from the record's name.
func HostFor(uid types.UID, domain string) string {
	sum := sha256.Sum256([]byte(uid))
	return "p" + hex.EncodeToString(sum[:])[:24] + "." + domain
}

// Index is what the request path consults.
type Index struct {
	Client client.Reader // the manager's cache

	mu    sync.Mutex
	ended map[string]endedHost // tombstones: host -> where its embedders are, for refusal pages
}

type endedHost struct {
	namespace    string
	embeddableBy []string
	at           time.Time
}

const tombstoneTTL = 24 * time.Hour

// noteEnded keeps a deleted record's host for a day, so a refusal page for
// it can still address the apps that may have embedded it.
func (x *Index) noteEnded(host, namespace string, embeddableBy []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.ended == nil {
		x.ended = map[string]endedHost{}
	}
	now := time.Now()
	for h, e := range x.ended {
		if now.Sub(e.at) > tombstoneTTL {
			delete(x.ended, h)
		}
	}
	x.ended[host] = endedHost{namespace, embeddableBy, now}
}

func (x *Index) endedRecord(host string) (namespace string, embeddableBy []string, ok bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.ended[host]
	return e.namespace, e.embeddableBy, ok
}

// AppNamespace is the namespace of application app in tenant, or "" when the
// tenant has no such application.
func AppNamespace(ctx context.Context, r client.Reader, tenant, app string) (string, error) {
	var l corev1.NamespaceList
	if err := r.List(ctx, &l, client.MatchingLabels{LabelTenant: tenant, LabelApp: app}); err != nil {
		return "", err
	}
	switch len(l.Items) {
	case 0:
		return "", nil
	case 1:
		return l.Items[0].Name, nil
	}
	return "", fmt.Errorf("tenant %q has %d namespaces labelled app %q; ADR-088 gives an application one", tenant, len(l.Items), app)
}

// Reachable is every record named name that application app of tenant may
// embed: the one in its own namespace, and any a sibling app of the same
// tenant declared embeddable by it. More than one is a collision the caller
// must refuse, never resolve by picking.
func Reachable(ctx context.Context, r client.Reader, tenant, app, name string) ([]*computev1alpha1.PreviewAccess, error) {
	var found []*computev1alpha1.PreviewAccess
	own, err := AppNamespace(ctx, r, tenant, app)
	if err != nil {
		return nil, err
	}
	if own != "" {
		var rec computev1alpha1.PreviewAccess
		if err := r.Get(ctx, client.ObjectKey{Namespace: own, Name: name}, &rec); err == nil {
			found = append(found, &rec)
		} else if client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}
	var l computev1alpha1.PreviewAccessList
	if err := r.List(ctx, &l, client.MatchingFields{IndexEmbeddableBy: EmbedKey(app, name)}); err != nil {
		return nil, err
	}
	for i := range l.Items {
		rec := &l.Items[i]
		if rec.Namespace == own {
			continue
		}
		var n corev1.Namespace
		if err := r.Get(ctx, client.ObjectKey{Name: rec.Namespace}, &n); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return nil, err
		}
		if n.Labels[LabelTenant] == tenant {
			found = append(found, rec)
		}
	}
	return found, nil
}

// ByHost returns the record serving host, or nil.
func (x *Index) ByHost(ctx context.Context, host string) (*computev1alpha1.PreviewAccess, error) {
	var l computev1alpha1.PreviewAccessList
	if err := x.Client.List(ctx, &l, client.MatchingFields{IndexHost: host}); err != nil {
		return nil, err
	}
	if len(l.Items) == 0 {
		return nil, nil
	}
	return &l.Items[0], nil
}

// Allowed reports whether subject may open rec.
func Allowed(rec *computev1alpha1.PreviewAccess, subject string) bool {
	return subject != "" && (rec.Spec.OwnerSubject == subject || slices.Contains(rec.Spec.SharedWith, subject))
}

// RecordEnded reports whether the record is past its own bound.
func RecordEnded(rec *computev1alpha1.PreviewAccess, now time.Time) bool {
	return rec.Spec.ExpiresAt != nil && !now.Before(rec.Spec.ExpiresAt.Time)
}

// Target is where a record's preview is served from, or why it cannot be.
type Target struct {
	Addr string // podIP:port, when serving
}

// Resolve finds the running sandbox behind rec. ok is false when the record is
// not Ready or its job is not Running with a pod address: that is
// `unavailable`, never a proxy to something else.
func (x *Index) Resolve(ctx context.Context, rec *computev1alpha1.PreviewAccess) (Target, bool, error) {
	if !meta.IsStatusConditionTrue(rec.Status.Conditions, "Ready") {
		return Target{}, false, nil
	}
	var ej computev1alpha1.EphemeralJob
	if err := x.Client.Get(ctx, client.ObjectKey{Namespace: rec.Namespace, Name: rec.Spec.JobRef}, &ej); err != nil {
		return Target{}, false, client.IgnoreNotFound(err)
	}
	if ej.Status.Phase != computev1alpha1.PhaseRunning || ej.Status.PodName == "" {
		return Target{}, false, nil
	}
	var pod corev1.Pod
	if err := x.Client.Get(ctx, client.ObjectKey{Namespace: rec.Namespace, Name: ej.Status.PodName}, &pod); err != nil {
		return Target{}, false, client.IgnoreNotFound(err)
	}
	if pod.Status.PodIP == "" || pod.Labels["compute.nutgraf.in/ephemeraljob-uid"] != string(ej.UID) {
		return Target{}, false, nil
	}
	return Target{Addr: fmt.Sprintf("%s:%d", pod.Status.PodIP, rec.Spec.Port)}, true, nil
}

// StateFor is the caller's state for one preview, as the state endpoint
// answers it (ADR-108 §3a). rec is nil when the caller can reach no such
// record. Whether the caller is admitted, and was ever admitted,
// is the Session's to say.
func (x *Index) StateFor(ctx context.Context, rec *computev1alpha1.PreviewAccess, subject string, session Session) (string, *time.Time, error) {
	now := time.Now()
	if rec == nil || RecordEnded(rec, now) {
		return StateEnded, nil, nil
	}
	if !Allowed(rec, subject) {
		if session.WasAdmitted(rec, subject) {
			return StateRevoked, nil, nil
		}
		return StateDenied, nil, nil
	}
	if _, ok, err := x.Resolve(ctx, rec); err != nil {
		return "", nil, err
	} else if !ok {
		return StateUnavailable, nil, nil
	}
	if held, until := session.Holds(rec, subject, now); held {
		return StateActive, until, nil
	}
	return StateExpired, nil, nil
}
