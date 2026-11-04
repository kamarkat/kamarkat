package provision

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kamarkat/kamarkat/operators/ephemeral-vm-provisioner/internal/store"
)

// Server exposes the idempotent ingress the SDK's Graphile Worker calls.
type Server struct {
	reconciler *Reconciler
	store      *store.Store
	Log        *slog.Logger
}

func NewServer(r *Reconciler, s *store.Store, log *slog.Logger) *Server {
	return &Server{reconciler: r, store: s, Log: log}
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/provision", s.provision)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return mux
}

type provisionRequest struct {
	JobID string `json:"jobId"`
}

// provision handles POST /internal/provision. It is idempotent by jobId:
//   - PENDING: durably claims the job (PENDING→PROVISIONING + lease) and returns 2xx.
//     Provisioning itself happens in the background reconciler, so a Graphile
//     retry always observes an already-claimed job and returns a fast 2xx.
//   - PROVISIONING / RUNNING: returns the current job state (active claim untouched).
//   - COMPLETED / FAILED: returns the current terminal state — never resurrects.
func (s *Server) provision(w http.ResponseWriter, r *http.Request) {
	var req provisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.JobID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "jobId is required"})
		return
	}

	job, err := s.store.Get(r.Context(), req.JobID)
	if err != nil {
		s.Log.Error("failed to load job", "jobId", req.JobID, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load job"})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}

	switch job.Status {
	case store.StatusCompleted, store.StatusFailed:
		// Terminal — report as-is; the reconciler owns VM cleanup. (Adjustment 1)
		writeJSON(w, http.StatusOK, map[string]interface{}{"jobId": job.ID, "status": job.Status})
	case store.StatusPending:
		claimed, err := s.store.ClaimPending(r.Context(), job.ID, s.reconciler.hostname, leaseSeconds)
		if err != nil {
			s.Log.Error("failed to claim job", "jobId", job.ID, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to claim job"})
			return
		}
		if claimed == nil {
			// Lost a race to another provisioner instance — that instance owns it.
			writeJSON(w, http.StatusOK, map[string]interface{}{"jobId": job.ID, "status": currentStatusNoop})
			return
		}
		// Durably claimed. Trigger provisioning synchronously so the VM is created
		// immediately rather than waiting for the next reconciler tick. Return 2xx
		// so Graphile does not retry an owned job. (Adjustment 2)
		s.Log.Info("claimed job via Graphile", "jobId", job.ID, "attempt", claimed.Attempts, "claimedBy", claimed.ClaimedBy)
		s.reconciler.Trigger(r.Context(), job.ID)
		writeJSON(w, http.StatusOK, map[string]interface{}{"jobId": job.ID, "status": store.StatusProvisioning})
	case store.StatusProvisioning, store.StatusRunning:
		// Already owned (by us or another active lease) — report current state.
		// (Adjustment 1: do not blindly no-op terminal/recoverable states.)
		s.reconciler.Trigger(r.Context(), job.ID)
		writeJSON(w, http.StatusOK, map[string]interface{}{"jobId": job.ID, "status": job.Status, "claimedBy": job.ClaimedBy})
	default:
		writeJSON(w, http.StatusOK, map[string]interface{}{"jobId": job.ID, "status": job.Status})
	}
}

// currentStatusNoop is used when a concurrent claim won the race; the caller is
// told the job is now owned elsewhere.
const currentStatusNoop = "PENDING"

func writeJSON(w http.ResponseWriter, code int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Run blocks serving HTTP requests until ctx is cancelled.
func (s *Server) Run(ctx context.Context, addr string) error {
	s.Log.Info("http server listening", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: s.Router()}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	return srv.ListenAndServe()
}
