package infisical

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Day-0 must be re-runnable. Resuming a bootstrap died here on
// infisical-db-username with "Secret already exists" -- after three and a half
// minutes of work that had all succeeded -- because the existence check said no
// and the create then said yes.
//
// The server is the one that knows, so a create refused for that reason becomes
// an update.
func TestUploadingAnExistingSecretUpdatesItInstead(t *testing.T) {
	var posted, patched int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/workspace"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workspaces": []map[string]any{{"id": "ws-1", "slug": "proj"}}})
		case r.Method == http.MethodGet:
			// The lookup misses -- which is the condition under test.
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			posted++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"statusCode":400,"message":"Secret already exists","error":"BadRequest"}`))
		case r.Method == http.MethodPatch:
			patched++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, token: "t", httpClient: srv.Client()}
	if err := c.CreateOrUpdateSecret(context.Background(), "proj", "prod", "/", "infisical-db-username", "v"); err != nil {
		t.Fatalf("re-uploading an existing secret failed: %v", err)
	}
	if posted != 1 || patched != 1 {
		t.Errorf("posted=%d patched=%d; want one create attempt then one update", posted, patched)
	}
}

// A 400 that is not "already exists" is a real error and must surface. Matching
// too broadly would turn a malformed request into a silent overwrite.
func TestAnUnrelatedBadRequestIsNotTreatedAsExisting(t *testing.T) {
	var patched int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/workspace"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workspaces": []map[string]any{{"id": "ws-1", "slug": "proj"}}})
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"statusCode":400,"message":"secretValue must be a string"}`))
		case r.Method == http.MethodPatch:
			patched++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, token: "t", httpClient: srv.Client()}
	err := c.CreateOrUpdateSecret(context.Background(), "proj", "prod", "/", "k", "v")
	if err == nil {
		t.Fatal("a malformed request reported success")
	}
	if patched != 0 {
		t.Errorf("an unrelated 400 caused an update; that is a silent overwrite")
	}
}

// A secretPath is a path and may carry characters that are not query-safe.
// Unescaped, the lookup misses and the caller concludes the secret is absent --
// one of the ways the check and the create came to disagree.
func TestTheExistenceLookupEscapesItsQuery(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/secrets/raw/") {
			raw = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspaces": []map[string]any{{"id": "ws-1", "slug": "proj"}}})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, token: "t", httpClient: srv.Client()}
	_, err := c.secretExists(context.Background(), "ws-1", "prod", "/platform/db", "k")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "secretPath=%2Fplatform%2Fdb") {
		t.Errorf("secretPath was not escaped: %s", raw)
	}
}
