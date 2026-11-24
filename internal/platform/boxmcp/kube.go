package boxmcp

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The platform resources the tools read, by group and plural. Versions are
// resolved from discovery: CAPI, CNPG and ESO move their served versions over
// time, and a hardcoded one stops answering on the upgrade that drops it.
var (
	resApplications    = groupResource{"argoproj.io", "applications"}
	resApplicationSets = groupResource{"argoproj.io", "applicationsets"}
	resCAPIClusters    = groupResource{"cluster.x-k8s.io", "clusters"}
	resCAPIMachines    = groupResource{"cluster.x-k8s.io", "machines"}
	resHubEnvironments = groupResource{"ops.nutgraf.in", "hubenvironments"}
	resCertificates    = groupResource{"cert-manager.io", "certificates"}
	resCNPGClusters    = groupResource{"postgresql.cnpg.io", "clusters"}
	resPolicyReports   = groupResource{"wgpolicyk8s.io", "policyreports"}
	resClusterReports  = groupResource{"wgpolicyk8s.io", "clusterpolicyreports"}
	resExternalSecrets = groupResource{"external-secrets.io", "externalsecrets"}
	// SpokePool's group is the platform's XRD group; found by its plural.
	resSpokePools = groupResource{"", "spokepools"}
)

type groupResource struct{ group, resource string }

func (g groupResource) String() string {
	if g.group == "" {
		return g.resource
	}
	return g.resource + "." + g.group
}

// perm is the permission string a tool declares for reading r.
func perm(verb string, r groupResource) string { return verb + " " + r.String() }

var resolved sync.Map // groupResource -> schema.GroupVersionResource

// gvr resolves r against the API server's discovery. An absent resource is an
// error naming it: the capability that installs it is off, or not installed.
func gvr(c *Clients, r groupResource) (schema.GroupVersionResource, error) {
	if v, ok := resolved.Load(r); ok {
		return v.(schema.GroupVersionResource), nil
	}
	groups, lists, err := c.Discovery.ServerGroupsAndResources()
	if err != nil && len(lists) == 0 {
		return schema.GroupVersionResource{}, fmt.Errorf("discover the API: %w", err)
	}
	// A group's preferred version first, so a resource served at several
	// versions resolves to the one the API server recommends.
	preferred := map[string]string{}
	for _, g := range groups {
		preferred[g.Name] = g.PreferredVersion.GroupVersion
	}
	isPreferred := func(gvs string) bool {
		gv, _ := schema.ParseGroupVersion(gvs)
		return preferred[gv.Group] != "" && preferred[gv.Group] == gvs
	}
	sort.SliceStable(lists, func(i, j int) bool {
		return isPreferred(lists[i].GroupVersion) && !isPreferred(lists[j].GroupVersion)
	})
	for _, l := range lists {
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil || (r.group != "" && gv.Group != r.group) {
			continue
		}
		for _, res := range l.APIResources {
			if res.Name == r.resource {
				out := gv.WithResource(res.Name)
				resolved.Store(r, out)
				return out, nil
			}
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("%s is not served by this cluster: the component that installs it is not running here", r)
}

func list(ctx context.Context, c *Clients, r groupResource, namespace string, opts metav1.ListOptions) ([]unstructured.Unstructured, error) {
	g, err := gvr(c, r)
	if err != nil {
		return nil, err
	}
	l, err := c.Dynamic.Resource(g).Namespace(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	items := l.Items
	sort.Slice(items, func(i, j int) bool {
		if items[i].GetNamespace() != items[j].GetNamespace() {
			return items[i].GetNamespace() < items[j].GetNamespace()
		}
		return items[i].GetName() < items[j].GetName()
	})
	return items, nil
}

func get(ctx context.Context, c *Clients, r groupResource, namespace, name string) (*unstructured.Unstructured, error) {
	g, err := gvr(c, r)
	if err != nil {
		return nil, err
	}
	return c.Dynamic.Resource(g).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// Condition is a status condition, as every tool reports one.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Since   string `json:"since,omitempty"`
}

func conditions(u *unstructured.Unstructured, path ...string) []Condition {
	if len(path) == 0 {
		path = []string{"status", "conditions"}
	}
	raw, _, _ := unstructured.NestedSlice(u.Object, path...)
	out := make([]Condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Condition{
			Type:    str(m, "type"),
			Status:  str(m, "status"),
			Reason:  str(m, "reason"),
			Message: truncate(str(m, "message"), 600),
			Since:   first(str(m, "lastTransitionTime"), str(m, "lastUpdateTime")),
		})
	}
	return out
}

// notTrue is the conditions that are not True: what an agent should look at.
func notTrue(cs []Condition) []Condition {
	var out []Condition
	for _, c := range cs {
		if c.Status != "True" {
			out = append(out, c)
		}
	}
	return out
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func nested(u *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedFieldNoCopy(u.Object, path...)
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func first(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// age is how long ago an RFC 3339 timestamp was, for a human reading the answer.
func age(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return time.Since(t).Round(time.Second).String()
}
