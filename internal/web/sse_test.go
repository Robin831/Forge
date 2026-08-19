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

// busServer returns a default test server with the in-process event Bus wired
// into its DB, selecting the replay-then-live SSE path.
func busServer(t *testing.T) *Server {
	t.Helper()
	srv := newServerWithDefaults(t, nil)
	srv.db.SetBus(state.NewBus(256))
	return srv
}

// collectMessages unmarshals raw activity-event JSON payloads to their Message
// fields, preserving delivery order.
func collectMessages(t *testing.T, raw []string) []string {
	t.Helper()
	msgs := make([]string, 0, len(raw))
	for _, r := range raw {
		var ev activityEvent
		if err := json.Unmarshal([]byte(r), &ev); err != nil {
			t.Fatalf("unmarshal %q: %v", r, err)
		}
		msgs = append(msgs, ev.Message)
	}
	return msgs
}

func TestActivityStream_BusReplayThenLive(t *testing.T) {
	srv := busServer(t)

	// Backlog replayed on connect (published before any subscriber exists, so
	// these land only in the DB).
	for _, m := range []string{"r0", "r1", "r2"} {
		if err := srv.db.LogEvent(state.EventBeadClaimed, m, "", ""); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer cancel()
	defer resp.Body.Close()

	// Give the handler time to subscribe and drain the replay, then publish
	// live events that must arrive over the Bus channel.
	time.Sleep(300 * time.Millisecond)
	for _, m := range []string{"l3", "l4"} {
		if err := srv.db.LogEvent(state.EventBeadClaimed, m, "", ""); err != nil {
			t.Fatalf("LogEvent live: %v", err)
		}
	}

	got := collectMessages(t, readSSEData(t, resp.Body, 5, 5*time.Second))
	want := []string{"r0", "r1", "r2", "l3", "l4"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery order mismatch: got %v want %v", got, want)
		}
	}
}

func TestActivityStream_BusDedupSkipsAlreadyEmitted(t *testing.T) {
	srv := busServer(t)

	for _, m := range []string{"a", "b", "c"} {
		if err := srv.db.LogEvent(state.EventBeadClaimed, m, "", ""); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}
	all, err := srv.db.RecentEvents(10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	maxID := all[0].ID // newest-first

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer cancel()
	defer resp.Body.Close()

	// Let the handler finish replay (lastEmittedSeq == maxID).
	time.Sleep(300 * time.Millisecond)

	// A live event whose Seq was already emitted during replay must be dropped.
	srv.db.Bus().Publish(state.BusEvent{
		Seq:   int64(maxID),
		Event: state.Event{ID: maxID, Type: state.EventBeadClaimed, Message: "dup"},
	})
	// A genuinely new event must be delivered.
	srv.db.Bus().Publish(state.BusEvent{
		Seq:   int64(maxID + 1),
		Event: state.Event{ID: maxID + 1, Type: state.EventBeadClaimed, Message: "fresh"},
	})

	got := collectMessages(t, readSSEData(t, resp.Body, 4, 5*time.Second))
	want := []string{"a", "b", "c", "fresh"}
	if len(got) != len(want) {
		t.Fatalf("expected dedup to drop 'dup'; got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestActivityStream_BusLastEventIDSeedsReplay(t *testing.T) {
	srv := busServer(t)

	for _, m := range []string{"e0", "e1", "e2"} {
		if err := srv.db.LogEvent(state.EventBeadClaimed, m, "", ""); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}
	all, err := srv.db.RecentEvents(10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	// all is newest-first: [e2, e1, e0]. Resume from e1's ID so replay should
	// only contain e2.
	resumeID := all[1].ID
	e2ID := all[0].ID

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", fmt.Sprintf("%d", resumeID))
	defer cancel()
	defer resp.Body.Close()

	got := collectMessages(t, readSSEData(t, resp.Body, 1, 5*time.Second))
	if len(got) != 1 || got[0] != "e2" {
		t.Fatalf("Last-Event-ID replay: got %v want [e2] (e2 id=%d)", got, e2ID)
	}
}

func TestActivityStream_BusGapMarkerResyncs(t *testing.T) {
	srv := busServer(t)

	if err := srv.db.LogEvent(state.EventBeadClaimed, "seed", "", ""); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer cancel()
	defer resp.Body.Close()

	time.Sleep(300 * time.Millisecond)

	// Persist an event the subscriber "missed" (as if it had been dropped on
	// overflow), then deliver a gap marker: the handler must re-sync it from
	// the DB via EventsSince rather than losing it.
	if err := srv.db.LogEvent(state.EventBeadClaimed, "missed", "", ""); err != nil {
		t.Fatalf("LogEvent missed: %v", err)
	}
	srv.db.Bus().Publish(state.BusEvent{GapMarker: true})

	got := collectMessages(t, readSSEData(t, resp.Body, 2, 5*time.Second))
	want := []string{"seed", "missed"}
	if len(got) != len(want) {
		t.Fatalf("gap re-sync: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// TestActivityStream_PollFallbackIgnoresBus verifies the one-release safety
// valve: with the Bus wired but settings.sse_poll_fallback engaged, the handler
// takes the legacy polling path. Polling reads from the DB, so a DB-logged event
// is delivered while a Bus-only publish (never persisted) is never seen.
func TestActivityStream_PollFallbackIgnoresBus(t *testing.T) {
	srv := busServer(t)
	srv.SetSSEPollFallback(func() bool { return true })

	if err := srv.db.LogEvent(state.EventBeadClaimed, "seed", "", ""); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer cancel()
	defer resp.Body.Close()

	// Let the initial replay flush, then emit a Bus-only event (not in the DB)
	// that the polling path must ignore, and a DB-logged event the 2s poll
	// tick must pick up.
	time.Sleep(300 * time.Millisecond)
	srv.db.Bus().Publish(state.BusEvent{
		Seq:   1 << 30,
		Event: state.Event{ID: 1 << 30, Type: state.EventBeadClaimed, Message: "busonly"},
	})
	if err := srv.db.LogEvent(state.EventBeadClaimed, "polled", "", ""); err != nil {
		t.Fatalf("LogEvent polled: %v", err)
	}

	got := collectMessages(t, readSSEData(t, resp.Body, 2, 5*time.Second))
	want := []string{"seed", "polled"}
	if len(got) != len(want) {
		t.Fatalf("poll fallback: got %v want %v (busonly must not appear)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// TestActivityStream_PollFallbackDisabledUsesBus confirms that when the fallback
// callback reports false the handler stays on the Bus path — a Bus-only publish
// (never persisted to the DB) is delivered, which the polling path could not do.
func TestActivityStream_PollFallbackDisabledUsesBus(t *testing.T) {
	srv := busServer(t)
	srv.SetSSEPollFallback(func() bool { return false })

	if err := srv.db.LogEvent(state.EventBeadClaimed, "seed", "", ""); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	all, err := srv.db.RecentEvents(10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	maxID := all[0].ID

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer cancel()
	defer resp.Body.Close()

	time.Sleep(300 * time.Millisecond)
	// A Bus-only event with a fresh Seq must be delivered over the live channel,
	// proving the Bus path is active despite the (false) fallback override.
	srv.db.Bus().Publish(state.BusEvent{
		Seq:   int64(maxID + 1),
		Event: state.Event{ID: maxID + 1, Type: state.EventBeadClaimed, Message: "busonly"},
	})

	got := collectMessages(t, readSSEData(t, resp.Body, 2, 5*time.Second))
	want := []string{"seed", "busonly"}
	if len(got) != len(want) {
		t.Fatalf("bus path: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// TestActivityStream_BusDeliversUnder100ms is the SSE-level integration test
// design point 5 calls for: a live event published after the handler has entered
// the Bus live loop must reach a connected SSE client in well under 100ms — the
// whole point of the Bus over the legacy 2s poll. The DB starts empty so replay
// emits nothing and the measurement isolates the live publish→flush→client path.
//
// The clock must not start until the handler is provably parked in the live
// select loop. Do() returns as soon as the response headers arrive, which
// streamActivityBus flushes BEFORE it subscribes and replays, so a bare sleep
// only guesses at readiness: on a loaded CI runner where startup outruns the
// guess, the "delivery latency" silently absorbs the rest of subscribe+replay
// and the assertion fails on scheduler noise rather than on Bus delivery. So we
// synchronise on a warmup event instead. Receiving it proves the line-222 flush
// has already run — a replay-phase emit is not visible to the client until that
// flush — which puts the handler at the select, and only then do we measure.
func TestActivityStream_BusDeliversUnder100ms(t *testing.T) {
	srv := busServer(t)

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer cancel()
	defer resp.Body.Close()

	type stamped struct {
		msg string
		at  time.Time
	}
	got := make(chan stamped, 8)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev activityEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				got <- stamped{msg: ev.Message, at: time.Now()}
			}
		}
	}()

	// Readiness barrier: publish a warmup event and wait for the client to see
	// it. Its own delivery is deliberately untimed — however long the handler
	// takes to subscribe, replay and flush is startup cost, not Bus latency.
	if err := srv.db.LogEvent(state.EventBeadClaimed, "warmup", "", ""); err != nil {
		t.Fatalf("LogEvent warmup: %v", err)
	}
	warmupDeadline := time.After(10 * time.Second)
	for ready := false; !ready; {
		select {
		case s := <-got:
			if s.msg == "warmup" {
				ready = true
			}
		case <-warmupDeadline:
			t.Fatal("handler did not reach the live loop within 10s (warmup event never delivered)")
		}
	}

	// The handler is now in the live select loop, so this measures exactly the
	// publish→flush→client path the design point is about.
	start := time.Now()
	if err := srv.db.LogEvent(state.EventBeadClaimed, "live", "", ""); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-got:
			if s.msg != "live" {
				// Ignore any residual replay/backlog delivery.
				continue
			}
			if latency := s.at.Sub(start); latency > 100*time.Millisecond {
				t.Fatalf("live event delivered in %s, want < 100ms", latency)
			}
			return
		case <-deadline:
			t.Fatal("live event not delivered within 2s")
		}
	}
}

// TestActivityStream_BusFanOutToNClients confirms the fan-out property holds at
// the SSE layer, not just in the Bus unit tests: N concurrent EventSource
// clients each receive the same live event over their own subscription.
func TestActivityStream_BusFanOutToNClients(t *testing.T) {
	srv := busServer(t)

	const nClients = 4
	bodies := make([]io.ReadCloser, 0, nClients)
	// Each goroutine ships the raw payload back; parsing/assertion stays on the
	// main test goroutine (t.Fatal must not be called from a spawned one).
	results := make(chan string, nClients)

	for i := 0; i < nClients; i++ {
		resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
		defer cancel()
		bodies = append(bodies, resp.Body)
		go func(body io.ReadCloser) {
			raw := readSSEData(t, body, 1, 5*time.Second)
			if len(raw) == 1 {
				results <- raw[0]
			} else {
				results <- ""
			}
		}(resp.Body)
	}
	defer func() {
		for _, b := range bodies {
			b.Close()
		}
	}()

	// Ensure every handler has subscribed before the single live publish.
	time.Sleep(300 * time.Millisecond)
	if err := srv.db.LogEvent(state.EventBeadClaimed, "broadcast", "", ""); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	for i := 0; i < nClients; i++ {
		select {
		case raw := <-results:
			if raw == "" {
				t.Fatalf("client %d received no event", i)
			}
			var ev activityEvent
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatalf("client %d: unmarshal %q: %v", i, raw, err)
			}
			if ev.Message != "broadcast" {
				t.Fatalf("client %d received %q, want \"broadcast\"", i, ev.Message)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for client %d to receive the broadcast", i)
		}
	}
}

func TestActivityStream_BusClientDisconnectEndsStream(t *testing.T) {
	srv := busServer(t)

	if err := srv.db.LogEvent(state.EventBeadClaimed, "only", "", ""); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	resp, cancel := authedSSEClient(t, srv, "/api/activity/stream", "")
	defer resp.Body.Close()

	// Read the replay event to confirm the subscription is active.
	got := collectMessages(t, readSSEData(t, resp.Body, 1, 5*time.Second))
	if len(got) != 1 || got[0] != "only" {
		t.Fatalf("expected replay [only], got %v", got)
	}

	// Cancel the request context; the handler must return (running the
	// deferred Unsubscribe) and the stream must end at EOF.
	cancel()
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after client disconnect")
	}
}

// findingsBusServer returns a default test server with the dedicated findings
// Bus wired into its DB, selecting the bus-driven PR-findings SSE path.
func findingsBusServer(t *testing.T) *Server {
	t.Helper()
	srv := newServerWithDefaults(t, nil)
	srv.db.SetFindingsBus(state.NewBus(256))
	return srv
}

func TestPRFindingsStream_BusReEmitsOnRecordedRun(t *testing.T) {
	srv := findingsBusServer(t)

	pr := &state.PR{
		Number: 11, Anvil: "anvil-a", BeadID: "Forge-cccc",
		Branch: "x", Status: state.PROpen, CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert PR: %v", err)
	}

	resp, cancel := authedSSEClient(t, srv, fmt.Sprintf("/api/prs/%d/findings/stream", pr.ID), "")
	defer cancel()
	defer resp.Body.Close()

	// A completed Assay pass persists a finding then records the run; recording
	// the run publishes findings-changed. Trigger it from a goroutine after the
	// handler has subscribed so a single reader observes both the initial (empty)
	// snapshot and the re-emitted one.
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := srv.db.InsertFinding(state.Finding{
			Anvil: "anvil-a", PRNumber: 11, FindingHash: "fh1",
			Severity: "Important", Title: "Bus-driven finding",
		}); err != nil {
			t.Errorf("insert finding: %v", err)
			return
		}
		finished := time.Now().UTC()
		if err := srv.db.RecordAssayRun(&state.AssayRun{
			Anvil: "anvil-a", PRNumber: 11, HeadSHA: "abc",
			StartedAt: finished, FinishedAt: &finished, FindingsCount: 1,
		}); err != nil {
			t.Errorf("record assay run: %v", err)
		}
	}()

	got := readSSEData(t, resp.Body, 2, 5*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected initial + re-emitted snapshot, got %d (%v)", len(got), got)
	}
	var snap prFindingsResponse
	if err := json.Unmarshal([]byte(got[1]), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if len(snap.Findings) != 1 || snap.Findings[0].Message != "Bus-driven finding" {
		t.Errorf("unexpected re-emitted findings: %+v", snap.Findings)
	}
	if snap.Run == nil {
		t.Errorf("expected run to be present after RecordAssayRun, got nil")
	}
}

func TestPRFindingsStream_BusGapMarkerResyncs(t *testing.T) {
	srv := findingsBusServer(t)

	pr := &state.PR{
		Number: 7, Anvil: "anvil-b", BeadID: "Forge-dddd",
		Branch: "x", Status: state.PROpen, CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert PR: %v", err)
	}

	resp, cancel := authedSSEClient(t, srv, fmt.Sprintf("/api/prs/%d/findings/stream", pr.ID), "")
	defer cancel()
	defer resp.Body.Close()

	// Change the snapshot in the DB WITHOUT a targeted notification, then deliver
	// a gap marker (the Bus dropped events under load). The handler cannot know
	// which PR changed, so it must re-read its own snapshot and pick up the change.
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := srv.db.InsertFinding(state.Finding{
			Anvil: "anvil-b", PRNumber: 7, FindingHash: "fh-gap",
			Severity: "Important", Title: "Recovered after gap",
		}); err != nil {
			t.Errorf("insert finding: %v", err)
			return
		}
		srv.db.FindingsBus().Publish(state.BusEvent{GapMarker: true})
	}()

	got := readSSEData(t, resp.Body, 2, 5*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected initial + re-synced snapshot, got %d (%v)", len(got), got)
	}
	var snap prFindingsResponse
	if err := json.Unmarshal([]byte(got[1]), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if len(snap.Findings) != 1 || snap.Findings[0].Message != "Recovered after gap" {
		t.Errorf("unexpected re-synced findings: %+v", snap.Findings)
	}
}
