package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// authedSSEClient logs in, opens an SSE request with the session cookie, and
// returns the response (whose body must be drained or closed by the caller).
func authedSSEClient(t *testing.T, srv *Server, path string, lastEventID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	cookie := loginAndGetCookie(t, srv)

	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("do request: %v", err)
	}
	return resp, cancel
}

// readSSEData scans up to maxEvents `data:` lines from r and returns the JSON
// payloads. It stops once maxEvents are read or the underlying reader hits
// EOF / context cancellation.
func readSSEData(t *testing.T, r io.Reader, maxEvents int, deadline time.Duration) []string {
	t.Helper()
	type result struct {
		lines []string
	}
	out := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				lines = append(lines, strings.TrimPrefix(line, "data: "))
				if len(lines) >= maxEvents {
					break
				}
			}
		}
		out <- result{lines: lines}
	}()

	select {
	case r := <-out:
		return r.lines
	case <-time.After(deadline):
		return nil
	}
}

func TestActivityStream_InitialEvents(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	// Insert three events ahead of the connection.
	for _, msg := range []string{"first", "second", "third"} {
		if err := srv.db.LogEvent(state.EventBeadClaimed, msg, "bd-1", "anvil-a"); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer cancel()
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type: got %q want text/event-stream", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering: got %q want no", got)
	}

	got := readSSEData(t, resp.Body, 3, 5*time.Second)
	if len(got) != 3 {
		t.Fatalf("expected 3 initial events, got %d (%v)", len(got), got)
	}
	// Oldest first: "first" must precede "third" in delivery order.
	var first, third int
	for i, raw := range got {
		var ev activityEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev.Message == "first" {
			first = i
		}
		if ev.Message == "third" {
			third = i
		}
	}
	if first >= third {
		t.Errorf("expected oldest-first order; got first=%d third=%d (%v)", first, third, got)
	}
}

func TestActivityStream_LastEventIDSkipsReplay(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	for i := 0; i < 3; i++ {
		if err := srv.db.LogEvent(state.EventBeadClaimed, fmt.Sprintf("m%d", i), "", ""); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}
	all, err := srv.db.RecentEvents(10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(all))
	}
	// RecentEvents is newest-first: the latest ID is at index 0.
	latestID := all[0].ID

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", fmt.Sprintf("%d", latestID))
	defer resp.Body.Close()

	// No new events past latestID — wait a beat to confirm we don't see a
	// historical replay.
	doneCh := make(chan []string, 1)
	go func() {
		doneCh <- readSSEData(t, resp.Body, 1, 1500*time.Millisecond)
	}()

	got := <-doneCh
	cancel()
	if len(got) != 0 {
		t.Errorf("expected no historical replay with Last-Event-ID, got %v", got)
	}
}

func TestWorkerLog_Tail(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	logPath := writeWorkerLog(t, srv.db, "w-tail-1", []string{"line1", "line2", "line3", "line4"})

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/worker/w-tail-1/log?tail=2", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"line3", "line4"}
	if len(body.Lines) != len(want) {
		t.Fatalf("expected 2 tail lines, got %d (%v)", len(body.Lines), body.Lines)
	}
	for i, w := range want {
		if body.Lines[i] != w {
			t.Errorf("line[%d]: got %q want %q", i, body.Lines[i], w)
		}
	}
	_ = logPath
}

func TestWorkerLog_TailDefaultLimit(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	writeWorkerLog(t, srv.db, "w-tail-default", []string{"a", "b", "c"})

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/worker/w-tail-default/log", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Lines) != 3 {
		t.Errorf("expected 3 lines without tail param, got %d", len(body.Lines))
	}
}

func TestWorkerLog_InvalidWorkerID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	// Path-traversal-style IDs containing "/" can't reach the handler because
	// chi treats them as multiple path segments — that's still a safe outcome
	// for the user. The cases below are characters chi accepts as a single
	// segment but the validWorkerID regex rejects.
	for _, badID := range []string{"_starts-with-underscore", "-starts-with-dash"} {
		req := httptest.NewRequest("GET", "/api/worker/"+badID+"/log", nil)
		req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q: expected 400, got %d body=%s", badID, rec.Code, rec.Body.String())
		}
	}
}

func TestWorkerLog_MissingWorker(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/worker/never-exists/log", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing worker, got %d", rec.Code)
	}
}

func TestWorkerLog_PathOutsideAllowlist(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	// Insert a worker pointing at /tmp — outside both ~/.forge and ~/.workers.
	tmpFile := filepath.Join(t.TempDir(), "evil.log")
	if err := os.WriteFile(tmpFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := srv.db.InsertWorker(&state.Worker{
		ID:        "w-evil",
		BeadID:    "bd-evil",
		Anvil:     "evil",
		Status:    state.WorkerStatus("running"),
		StartedAt: time.Now().UTC(),
		LogPath:   tmpFile,
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/worker/w-evil/log", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path outside allowlist, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerLogStream_DeliversInitialAndNewLines(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	logPath := writeWorkerLog(t, srv.db, "w-stream", []string{"hello", "world"})

	resp, cancel := authedSSEClient(t, srv, "/api/worker/w-stream/stream", "")
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type: got %q", got)
	}

	// Read the two initial lines — they should arrive before we append.
	lines := make(chan string, 16)
	errs := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			ln := scanner.Text()
			if strings.HasPrefix(ln, "data: ") {
				lines <- strings.TrimPrefix(ln, "data: ")
			}
		}
		errs <- scanner.Err()
	}()

	got := []string{}
	collect := func(want int, deadline time.Duration) {
		t.Helper()
		dl := time.After(deadline)
		for len(got) < want {
			select {
			case ln := <-lines:
				got = append(got, ln)
			case <-dl:
				return
			}
		}
	}

	collect(2, 3*time.Second)
	if len(got) < 2 {
		t.Fatalf("expected 2 initial lines, got %d (%v)", len(got), got)
	}

	// Append a new line and confirm it streams within ~1s.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("appended\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	collect(3, 3*time.Second)
	cancel()
	if len(got) < 3 {
		t.Fatalf("expected 3 lines after append, got %d (%v)", len(got), got)
	}

	var entry struct {
		Line string `json:"line"`
	}
	if err := json.Unmarshal([]byte(got[2]), &entry); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if entry.Line != "appended" {
		t.Errorf("expected appended line, got %q", entry.Line)
	}
}

func TestWorkerLogStream_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	for _, p := range []string{"/api/activity/stream", "/api/worker/anything/log", "/api/worker/anything/stream"} {
		req := httptest.NewRequest("GET", p, nil)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", p, rec.Code)
		}
	}
}

// --- helpers ---

// writeWorkerLog inserts a worker row whose LogPath points at a temp file
// inside a hermetic home directory so the resolveWorkerLogPath allowlist
// accepts it without touching the real ~/.forge on the developer's machine.
func writeWorkerLog(t *testing.T, db *state.DB, workerID string, lines []string) string {
	t.Helper()

	// Point HOME at a per-test temp dir so os.UserHomeDir() returns a path we
	// fully control; t.Setenv restores the original value after the test.
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	dir := filepath.Join(tempHome, ".forge", "test-worker-logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(dir, workerID+".log")

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    "bd-" + workerID,
		Anvil:     "test",
		Status:    state.WorkerStatus("running"),
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	return logPath
}

