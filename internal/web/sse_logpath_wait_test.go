package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// insertWorkerRow adds a worker row with the given status/phase and log path.
func insertWorkerRow(t *testing.T, srv *Server, id string, status state.WorkerStatus, phase, logPath string) {
	t.Helper()
	if err := srv.db.InsertWorker(&state.Worker{
		ID:        id,
		BeadID:    "Forge-abc1",
		Anvil:     "anvil-a",
		Status:    status,
		Phase:     phase,
		LogPath:   logPath,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
}

// Regression: the pipeline inserts the worker row (status running) before Smith
// spawns and records a log path. A live panel opened inside that window used to
// get a 404, which permanently closes the browser's EventSource — so the panel
// sat on "reconnecting" until the dashboard was remounted. An active worker
// must instead get a 200 stream that waits for the path.
func TestWorkerLogStream_ActiveWorkerWithoutLogPath_Waits(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	insertWorkerRow(t, srv, "w-pending", state.WorkerRunning, "smith", "")

	req := httptest.NewRequest("GET", "/api/worker/w-pending/stream", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	// Bound the wait: the handler holds the stream open awaiting the path.
	ctx, cancel := context.WithTimeout(req.Context(), 900*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req.WithContext(ctx))

	if rec.Code == http.StatusNotFound {
		t.Fatalf("active worker without a log path must not 404 (that permanently closes EventSource)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 streaming response, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected SSE content-type, got %q", ct)
	}
}

// A worker that can never produce a log should still fail fast, so the client
// isn't left holding a pointless connection open.
func TestWorkerLogStream_NonLoggingWorkersStill404(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	cases := []struct {
		name   string
		id     string
		status state.WorkerStatus
		phase  string
	}{
		{"bellows monitor", "w-bellows", state.WorkerMonitoring, "bellows"},
		{"ready to merge", "w-rtm", state.WorkerMonitoring, "ready_to_merge"},
		{"terminal worker", "w-done", state.WorkerDone, "smith"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			insertWorkerRow(t, srv, tc.id, tc.status, tc.phase, "")
			req := httptest.NewRequest("GET", "/api/worker/"+tc.id+"/stream", nil)
			req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("expected 404 for %s, got %d", tc.name, rec.Code)
			}
		})
	}
}

func TestWorkerMayStillLog(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	insertWorkerRow(t, srv, "a-running", state.WorkerRunning, "smith", "")
	insertWorkerRow(t, srv, "a-pending", state.WorkerPending, "smith", "")
	insertWorkerRow(t, srv, "a-paused", state.WorkerPaused, "smith", "")
	insertWorkerRow(t, srv, "a-assay", state.WorkerRunning, "assay", "")
	insertWorkerRow(t, srv, "a-bellows", state.WorkerMonitoring, "bellows", "")
	insertWorkerRow(t, srv, "a-failed", state.WorkerFailed, "smith", "")

	for id, want := range map[string]bool{
		"a-running": true,
		"a-pending": true,
		"a-paused":  true,
		"a-assay":   true,
		"a-bellows": false,
		"a-failed":  false,
		"a-missing": false,
	} {
		if got := workerMayStillLog(srv.db, id); got != want {
			t.Errorf("workerMayStillLog(%s) = %v, want %v", id, got, want)
		}
	}
}
