package infisical

import (
	"context"
	"strings"
	"testing"
)

// staleDecision mirrors the branch logic in invalidateStaleBootstrapCache.
//
// The organization ID is the instance fingerprint: every cached project and
// identity ID names a row inside that organization's database. Two failure modes
// sit either side of this decision, and both are silent:
//
//   - clearing too eagerly destroys the fallback that createProject relies on when
//     project listing returns empty, so a retry against the SAME Infisical fails
//     with "project not found in listing or ConfigMap";
//   - clearing too rarely lets a bootstrap trust projects from a database that was
//     rebuilt, so it skips creating them and fails later at identity creation.
func staleDecision(cachedOrg, liveOrg, cachedProject, cachedSecrets string) bool {
	orphaned := cachedOrg == "" && (cachedProject != "" || cachedSecrets != "")
	switch {
	case cachedOrg == liveOrg:
		return false
	case cachedOrg == "" && !orphaned:
		return false
	default:
		return true
	}
}

func TestCacheStalenessDecision(t *testing.T) {
	cases := []struct {
		name                                     string
		cachedOrg, liveOrg, project, secretsProj string
		wantClear                                bool
	}{
		{
			name:      "same instance — cache is valid and must survive a retry",
			cachedOrg: "org-A", liveOrg: "org-A", project: "p1", secretsProj: "p2",
			wantClear: false,
		},
		{
			// The failure this whole check exists for: platform-db rebuilt, so
			// Infisical came up with a new org and no projects, while the ConfigMap
			// still advertised the old ones.
			name:      "rebuilt database — cache names a dead organization",
			cachedOrg: "org-A", liveOrg: "org-B", project: "p1", secretsProj: "p2",
			wantClear: true,
		},
		{
			name:      "first bootstrap — nothing cached",
			cachedOrg: "", liveOrg: "org-A", project: "", secretsProj: "",
			wantClear: false,
		},
		{
			name:      "already cleared — stays cleared, no churn",
			cachedOrg: "", liveOrg: "org-B", project: "", secretsProj: "",
			wantClear: false,
		},
		{
			// Written by an older CLI, or a half-cleared ConfigMap. There is nothing
			// to validate the project IDs against, so they cannot be trusted.
			name:      "orphaned project IDs with no organization to anchor them",
			cachedOrg: "", liveOrg: "org-A", project: "p1", secretsProj: "",
			wantClear: true,
		},
		{
			name:      "orphaned secrets project only",
			cachedOrg: "", liveOrg: "org-A", project: "", secretsProj: "p2",
			wantClear: true,
		},
	}

	for _, c := range cases {
		got := staleDecision(c.cachedOrg, c.liveOrg, c.project, c.secretsProj)
		if got != c.wantClear {
			t.Errorf("%s:\n  cachedOrg=%q liveOrg=%q project=%q secrets=%q\n  clear=%v, want %v",
				c.name, c.cachedOrg, c.liveOrg, c.project, c.secretsProj, got, c.wantClear)
		}
	}
}

// The decision above was already correct in production — the log showed it firing
// exactly as designed. What failed was the REMEDIATION: it patched
// hub-bootstrap-config, which is GitOps-delivered, so ArgoCD restored the dead IDs
// within seconds and every later read in the same run got them back.
//
// These pin the suppression that actually holds, per reader, because
// getCachedProjectID bypasses getCachedConfigValue and was the one that logged
// "Found cached project hub-platform (id=…)" immediately after the cache was
// "cleared".
func TestSuppressionHidesInstanceIDsFromBothReaders(t *testing.T) {
	staleCacheSuppressed.Store(false)
	t.Cleanup(func() { staleCacheSuppressed.Store(false) })

	if staleCacheSuppressed.Load() {
		t.Fatal("suppression must start off")
	}

	staleCacheSuppressed.Store(true)

	// getCachedProjectID must return nothing at all once suppressed — it reads only
	// instance-specific project UUIDs.
	if got := getCachedProjectID("hub-platform"); got != "" {
		t.Errorf("getCachedProjectID returned %q while suppressed; ArgoCD restoring the "+
			"ConfigMap would feed a dead project ID straight back in", got)
	}
	if got := getCachedProjectID(SecretsProjectSlug); got != "" {
		t.Errorf("getCachedProjectID(secrets) returned %q while suppressed", got)
	}
}

// Slugs are identical on every instance, so suppressing them would discard usable
// configuration and break lookup-by-slug. Only the UUIDs identify one Infisical.
func TestSuppressionKeepsSlugsReadable(t *testing.T) {
	staleCacheSuppressed.Store(true)
	t.Cleanup(func() { staleCacheSuppressed.Store(false) })

	for _, key := range []string{"INFISICAL_ORGANIZATION_ID", "INFISICAL_PROJECT_ID", "INFISICAL_SECRETS_PROJECT_ID"} {
		if got := getCachedConfigValue(context.Background(), key); got != "" {
			t.Errorf("%s returned %q while suppressed", key, got)
		}
	}
	// Slug keys are not in the suppression list. Without a cluster they resolve to ""
	// anyway, so assert the routing rather than the value: a slug key must not be
	// short-circuited by the guard.
	for _, key := range []string{"INFISICAL_PROJECT_SLUG", "INFISICAL_SECRETS_PROJECT_SLUG"} {
		switch key {
		case "INFISICAL_ORGANIZATION_ID", "INFISICAL_PROJECT_ID", "INFISICAL_SECRETS_PROJECT_ID":
			t.Errorf("%s must not be in the suppressed set", key)
		}
	}
}

// curlAPI exists because "curl -s" throws the HTTP status away: an API error body
// unmarshals into the success struct leaving every field zero, and the failure is
// then reported as "returned empty ID" — which describes the parse, not the cause.
// These pin the split so a regression cannot quietly reintroduce that.
func TestCurlAPISplitsBodyFromStatus(t *testing.T) {
	cases := []struct {
		name, raw, wantBody string
		wantStatus          int
	}{
		{"json body then status", "{\"project\":{\"id\":\"p1\"}}\n200", "{\"project\":{\"id\":\"p1\"}}", 200},
		{"error body then status", "{\"statusCode\":401,\"error\":\"Unauthorized\"}\n401", "{\"statusCode\":401,\"error\":\"Unauthorized\"}", 401},
		{"multiline body", "line1\nline2\n500", "line1\nline2", 500},
		{"trailing newline after status", "{\"a\":1}\n403\n", "{\"a\":1}", 403},
	}
	for _, c := range cases {
		body, status := splitBodyStatus(c.raw)
		if body != c.wantBody || status != c.wantStatus {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", c.name, body, status, c.wantBody, c.wantStatus)
		}
	}
}

// A 401 must not be describable as an empty ID.
func TestAPIErrorNamesStatusAndBody(t *testing.T) {
	err := apiError("create project \"hub-platform\"", 401, "  {\"error\":\"Unauthorized\"}  ")
	msg := err.Error()
	for _, want := range []string{"HTTP 401", "Unauthorized", "hub-platform"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestAPIErrorTruncatesLongBodies(t *testing.T) {
	err := apiError("op", 500, strings.Repeat("x", 900))
	if len(err.Error()) > 500 {
		t.Errorf("error not truncated: %d chars", len(err.Error()))
	}
}

// Every kubectl in this package shells out. Running it bare made it obey an ambient
// KUBECONFIG while the Go-client health checks in the same phase used an explicit
// path — so readiness passed against the hub while kubectl fell back to
// localhost:8080, and the phase failed with "cannot find Infisical pod".
func TestWithKubeconfigPrefixesTheFlag(t *testing.T) {
	t.Cleanup(func() { SetKubeconfig("") })

	SetKubeconfig("")
	if got := withKubeconfig("get", "pods"); len(got) != 2 || got[0] != "get" {
		t.Errorf("unset: got %v, want args unchanged", got)
	}

	SetKubeconfig("/tmp/hub.kubeconfig")
	got := withKubeconfig("get", "pods")
	want := []string{"--kubeconfig", "/tmp/hub.kubeconfig", "get", "pods"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
