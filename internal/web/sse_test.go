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

func TestPRFindingsStream_InitialSnapshot(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	pr := &state.PR{
		Number: 11, Anvil: "anvil-a", BeadID: "Forge-cccc",
		Branch: "x", Status: state.PROpen, CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert PR: %v", err)
	}
	if err := srv.db.InsertFinding(state.Finding{
		Anvil: "anvil-a", PRNumber: 11, FindingHash: "fh1",
		Severity: "Important", Title: "Important finding",
	}); err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	resp, cancel := authedSSEClient(t, srv, fmt.Sprintf("/api/prs/%d/findings/stream", pr.ID), "")
	defer cancel()
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type: got %q want text/event-stream", got)
	}

	got := readSSEData(t, resp.Body, 1, 5*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 snapshot event, got %d (%v)", len(got), got)
	}
	var snap prFindingsResponse
	if err := json.Unmarshal([]byte(got[0]), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.PR != 11 || snap.Anvil != "anvil-a" {
		t.Errorf("unexpected snapshot pr/anvil: %+v", snap)
	}
	if len(snap.Findings) != 1 || snap.Findings[0].Message != "Important finding" {
		t.Errorf("unexpected snapshot findings: %+v", snap.Findings)
	}
}

func TestPRFindingsStream_NotFound(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/prs/12345/findings/stream", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown PR, got %d", rec.Code)
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
	// segment but the validWorkerID regex rejects. ".starts-with-dot" guards
	// the leading-character rule: dots are now allowed in trailing positions
	// (bead prefixes like Fhi.Metadata produce IDs that contain them) but a
	// leading dot would let `..` slip through and must stay rejected.
	for _, badID := range []string{"_starts-with-underscore", "-starts-with-dash", ".starts-with-dot"} {
		req := httptest.NewRequest("GET", "/api/worker/"+badID+"/log", nil)
		req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q: expected 400, got %d body=%s", badID, rec.Code, rec.Body.String())
		}
	}
}

// TestWorkerLog_DottedBeadPrefix_AcceptedByRegex pins the regex fix that
// unblocks worker logs for beads whose prefix contains a `.` (e.g. the
// `Fhi.Metadata` anvil). Previously the validWorkerID regex rejected the
// dot anywhere in the ID and the WorkerLogModal would spin on
// "reconnecting…" forever because both `/log` and `/stream` returned 400
// before resolveWorkerLogPath ran. We assert non-400 for both endpoints;
// the exact downstream status (200 / 404) depends on whether the worker
// row exists, which is covered by the other tests in this file.
func TestWorkerLog_DottedBeadPrefix_AcceptedByRegex(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	// Production-realistic worker ID: <anvil>-<bead-prefix>.<suffix>-<unix-nano>.
	id := "munin-Fhi.Metadata-2rtrj-1778499193"

	for _, suffix := range []string{"/log", "/stream"} {
		req := httptest.NewRequest("GET", "/api/worker/"+id+suffix, nil)
		req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code == http.StatusBadRequest {
			t.Errorf("endpoint=%s: dotted worker ID should bypass the validWorkerID gate, got 400 body=%s", suffix, rec.Body.String())
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

func TestWorkerLog_TailMissingLogFile_Returns200Empty(t *testing.T) {
	// A worker row whose log_path points at a file that hasn't been created
	// yet should return 200 with [] rather than 404. This is the race
	// between the workers row insert and the smith subprocess creating its
	// log file on disk; the modal would otherwise get stuck on
	// "reconnecting…".
	srv := newServerWithDefaults(t, nil)

	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	missingPath := filepath.Join(tempHome, ".forge", "logs", "w-missing.log")
	if err := os.MkdirAll(filepath.Dir(missingPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := srv.db.InsertWorker(&state.Worker{
		ID:        "w-missing",
		BeadID:    "bd-missing",
		Anvil:     "test",
		Status:    state.WorkerStatus("running"),
		Phase:     "smith",
		StartedAt: time.Now().UTC(),
		LogPath:   missingPath,
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/worker/w-missing/log", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for missing log file, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Lines == nil {
		t.Errorf("expected empty array, got nil")
	}
	if len(body.Lines) != 0 {
		t.Errorf("expected 0 lines for missing file, got %d (%v)", len(body.Lines), body.Lines)
	}
}

func TestWorkerLog_TailWorkerWithoutLogPath_Returns200Empty(t *testing.T) {
	// Bellows pseudo-workers carry no LogPath. Hitting the tail endpoint
	// must return 200+[] rather than 404+"worker has no log file" so the
	// modal renders an empty state instead of an error banner if a client
	// somehow bypasses the SPA gating.
	srv := newServerWithDefaults(t, nil)

	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	if err := srv.db.InsertWorker(&state.Worker{
		ID:        "bellows-test-42",
		BeadID:    "bd-bellows",
		Anvil:     "test",
		Status:    state.WorkerMonitoring,
		Phase:     "bellows",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/worker/bellows-test-42/log", nil)
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
	if len(body.Lines) != 0 {
		t.Errorf("expected 0 lines for bellows worker, got %d", len(body.Lines))
	}
}

func TestWorkerLog_TailDropsTrailingPartialLine(t *testing.T) {
	// When a smith is actively writing, the log file's last record may be
	// mid-write (no trailing newline). The tail handler must drop it so the
	// modal doesn't render a half-formed JSON envelope; only the lines
	// terminated with \n are returned.
	srv := newServerWithDefaults(t, nil)

	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	dir := filepath.Join(tempHome, ".forge", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(dir, "w-partial.log")
	// Two complete lines plus an unterminated tail.
	if err := os.WriteFile(logPath, []byte("complete-1\ncomplete-2\npartial-no-newline"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := srv.db.InsertWorker(&state.Worker{
		ID:        "w-partial",
		BeadID:    "bd-partial",
		Anvil:     "test",
		Status:    state.WorkerStatus("running"),
		Phase:     "smith",
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/worker/w-partial/log", nil)
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
	want := []string{"complete-1", "complete-2"}
	if len(body.Lines) != len(want) {
		t.Fatalf("expected %d complete lines, got %d (%v)", len(want), len(body.Lines), body.Lines)
	}
	for i, w := range want {
		if body.Lines[i] != w {
			t.Errorf("line[%d]: got %q want %q", i, body.Lines[i], w)
		}
	}
}

func TestWorkerLogStream_ActiveWriterDeliversAppends(t *testing.T) {
	// Simulates an active smith: open the SSE stream first, then append
	// new lines and confirm they arrive within the polling cadence. This
	// is the end-to-end repro for the "stuck on 'No log content yet.'"
	// bug — the stream must deliver content for an actively-running
	// worker, not just for a worker that already wrote bytes before the
	// modal opened.
	srv := newServerWithDefaults(t, nil)
	logPath := writeWorkerLog(t, srv.db, "w-active-stream", []string{})

	// Wipe the seeded content so the stream starts on an empty file —
	// matches the real race where the workers row is inserted before the
	// smith has written anything.
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	resp, cancel := authedSSEClient(t, srv, "/api/worker/w-active-stream/stream", "")
	defer resp.Body.Close()

	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			ln := scanner.Text()
			if strings.HasPrefix(ln, "data: ") {
				lines <- strings.TrimPrefix(ln, "data: ")
			}
		}
	}()

	// Append several lines in two batches to simulate ongoing claude output.
	appendLog := func(s string) {
		t.Helper()
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open append: %v", err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatalf("write: %v", err)
		}
		f.Close()
	}
	// Small delay so the stream is actively in its ticker loop.
	time.Sleep(200 * time.Millisecond)
	appendLog("first\nsecond\n")
	time.Sleep(700 * time.Millisecond)
	appendLog("third\n")

	got := []string{}
	deadline := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case ln := <-lines:
			got = append(got, ln)
		case <-deadline:
			t.Fatalf("expected 3 lines, got %d (%v)", len(got), got)
		}
	}
	cancel()

	want := []string{"first", "second", "third"}
	for i, w := range want {
		var entry struct {
			Line string `json:"line"`
		}
		if err := json.Unmarshal([]byte(got[i]), &entry); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if entry.Line != w {
			t.Errorf("line[%d]: got %q want %q", i, entry.Line, w)
		}
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

