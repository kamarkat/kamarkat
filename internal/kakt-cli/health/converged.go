package health

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ConvergedCheckers are the invariants a hub must satisfy before it is serving.
//
// Bootstrap returning success means every phase completed, not that the platform
// is up. What follows is real convergence: ArgoCD pulls charts, ESO waits on a
// secret store whose credentials another controller is still writing, operators
// reconcile in an order nobody sequences. Sampled once at the end of bootstrap,
// all of this reads as broken -- on the run that produced this code the Infisical
// store reported InvalidProviderConfig and repaired itself, and ExternalSecrets
// went 0 -> 6 -> 19 with nothing intervening.
//
// Ordered by dependency, because HealthWaiter blocks on the first failure and a
// later check failing for want of an earlier one is noise: nodes carry the pods,
// the store issues the secrets, the secrets start the workloads, and ArgoCD
// reports the whole.
func ConvergedCheckers() []HealthChecker {
	return []HealthChecker{
		&jsonCountChecker{
			name: "nodes Ready",
			args: []string{"get", "nodes", "-o", "json"},
			count: func(items []map[string]any) (int, int, string) {
				bad := []string{}
				for _, n := range items {
					if !hasCondition(n, "Ready", "True") {
						bad = append(bad, name(n))
					}
				}
				return len(bad), len(items), strings.Join(bad, ", ")
			},
		},
		&jsonCountChecker{
			name: "secret stores valid",
			args: []string{"get", "clustersecretstore", "-o", "json"},
			count: func(items []map[string]any) (int, int, string) {
				bad := []string{}
				for _, s := range items {
					if reason(s) != "Valid" {
						bad = append(bad, fmt.Sprintf("%s (%s)", name(s), reason(s)))
					}
				}
				return len(bad), len(items), strings.Join(bad, ", ")
			},
		},
		&jsonCountChecker{
			name: "ExternalSecrets synced",
			args: []string{"get", "externalsecrets", "-A", "-o", "json"},
			count: func(items []map[string]any) (int, int, string) {
				bad := []string{}
				for _, e := range items {
					if conditionStatus(e) != "True" {
						bad = append(bad, namespacedName(e))
					}
				}
				return len(bad), len(items), summarise(bad)
			},
		},
		&jsonCountChecker{
			name: "Applications Synced and Healthy",
			args: []string{"get", "applications", "-n", "platform-ops", "-o", "json"},
			count: func(items []map[string]any) (int, int, string) {
				bad := []string{}
				for _, a := range items {
					sync, _ := nested(a, "status", "sync", "status").(string)
					hlth, _ := nested(a, "status", "health", "status").(string)
					if sync != "Synced" || hlth != "Healthy" {
						bad = append(bad, fmt.Sprintf("%s (%s/%s)", name(a), or(sync, "-"), or(hlth, "-")))
					}
				}
				return len(bad), len(items), summarise(bad)
			},
		},
		&jsonCountChecker{
			name: "pods Running or Completed",
			args: []string{"get", "pods", "-A", "-o", "json"},
			count: func(items []map[string]any) (int, int, string) {
				bad := []string{}
				for _, p := range items {
					phase, _ := nested(p, "status", "phase").(string)
					if phase != "Running" && phase != "Succeeded" {
						bad = append(bad, fmt.Sprintf("%s (%s)", namespacedName(p), or(phase, "-")))
					}
				}
				return len(bad), len(items), summarise(bad)
			},
		},
	}
}

// jsonCountChecker reports how many of a resource are not yet in the wanted
// state, and names them. Counting rather than asserting one object exists is
// what makes the message useful while a cluster is still moving: "14/19 synced"
// says progress is happening, where "not ready" does not.
type jsonCountChecker struct {
	name  string
	args  []string
	count func(items []map[string]any) (bad int, total int, detail string)
}

func (c *jsonCountChecker) Name() string { return c.name }

func (c *jsonCountChecker) Check(ctx context.Context, kubeconfig string) error {
	out, err := runKubectl(ctx, append([]string{"--kubeconfig", kubeconfig}, c.args...))
	if err != nil {
		return fmt.Errorf("could not read %s: %w", c.name, err)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return fmt.Errorf("could not parse %s: %w", c.name, err)
	}
	// An empty list is not convergence. A cluster whose ArgoCD has generated no
	// Applications at all looks identical to one where every Application passed,
	// and that shape -- healthy while managing nothing -- is the failure this
	// platform has shipped more than once.
	if len(list.Items) == 0 {
		return fmt.Errorf("no %s exist yet", c.name)
	}
	bad, total, detail := c.count(list.Items)
	if bad == 0 {
		return nil
	}
	return fmt.Errorf("%d/%d ready; waiting on %s", total-bad, total, detail)
}

func nested(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = asMap[p]
	}
	return cur
}

func name(m map[string]any) string {
	s, _ := nested(m, "metadata", "name").(string)
	return s
}

func namespacedName(m map[string]any) string {
	ns, _ := nested(m, "metadata", "namespace").(string)
	return ns + "/" + name(m)
}

func conditions(m map[string]any) []any {
	c, _ := nested(m, "status", "conditions").([]any)
	return c
}

func conditionStatus(m map[string]any) string {
	for _, raw := range conditions(m) {
		if c, ok := raw.(map[string]any); ok {
			s, _ := c["status"].(string)
			return s
		}
	}
	return ""
}

func reason(m map[string]any) string {
	for _, raw := range conditions(m) {
		if c, ok := raw.(map[string]any); ok {
			s, _ := c["reason"].(string)
			return or(s, "no condition")
		}
	}
	return "no condition"
}

func hasCondition(m map[string]any, kind, want string) bool {
	for _, raw := range conditions(m) {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := c["type"].(string); t == kind {
			s, _ := c["status"].(string)
			return s == want
		}
	}
	return false
}

func or(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// summarise keeps the waiting line readable. The full list is printed once, at
// the deadline, where it is the answer rather than noise repeated every poll.
func summarise(bad []string) string {
	const show = 3
	if len(bad) <= show {
		return strings.Join(bad, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(bad[:show], ", "), len(bad)-show)
}
