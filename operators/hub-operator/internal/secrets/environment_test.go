package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestEnsureEnvironment(t *testing.T) {
	var envCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"accessToken": "test-token",
				"expiresIn":   3600,
			})
		case "/api/v1/projects/proj-1/environments":
			atomic.AddInt32(&envCalls, 1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message": "Successfully created new environment",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewInfisicalClient(server.URL, "client", "secret", "proj-1", "org-1")
	client.EnvironmentSlug = "stg"

	if err := client.EnsureEnvironment(context.Background()); err != nil {
		t.Fatalf("EnsureEnvironment failed: %v", err)
	}
	if calls := atomic.LoadInt32(&envCalls); calls != 1 {
		t.Fatalf("expected 1 call to create environment, got %d", calls)
	}
}

func TestEnsureFolderAutoCreatesEnvironment(t *testing.T) {
	var envCreated int32
	var folderAttempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"accessToken": "test-token",
				"expiresIn":   3600,
			})
		case "/api/v1/projects/proj-1/environments":
			atomic.StoreInt32(&envCreated, 1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"message": "created"})
		case "/api/v2/folders":
			attempt := atomic.AddInt32(&folderAttempts, 1)
			if attempt == 1 && atomic.LoadInt32(&envCreated) == 0 {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"message": "Environment with slug 'stg' in project with ID 'proj-1' not found",
				})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"message": "created"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewInfisicalClient(server.URL, "client", "secret", "proj-1", "org-1")
	client.EnvironmentSlug = "stg"

	if err := client.EnsureFolder(context.Background(), "/spoke-pool/test-cluster/shared"); err != nil {
		t.Fatalf("EnsureFolder failed: %v", err)
	}
	if atomic.LoadInt32(&envCreated) != 1 {
		t.Fatal("expected environment to be auto-created after 404")
	}
}
