package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// writePreservedLog creates a file in ~/.forge/logs/<beadID>/ with the given
// content and mtime, mirroring what preserveWorktreeLogs produces after a
// pipeline run. Callers must have already pointed HOME at a temp dir.
func writePreservedLog(t *testing.T, beadID, filename, content string, mtime time.Time) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	dir := filepath.Join(home, ".forge", "logs", beadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	return p
}

func authedGet(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestBeadLogs_ListPreservedStages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServerWithDefaults(t, nil)

	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	// Written out of order; the endpoint must sort ascending by mtime.
	writePreservedLog(t, "Forge-log1", "warden-2000-2.log", "w1\nw2\n", base.Add(2*time.Minute))
	writePreservedLog(t, "Forge-log1", "smith-1000-1.log", "s1\ns2\ns3\n", base.Add(1*time.Minute))
	writePreservedLog(t, "Forge-log1", "mystery-3000-3.log", "x\n", base.Add(3*time.Minute))

	rec := authedGet(t, srv, "/api/bead/Forge-log1/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp beadLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Files) != 3 {
		t.Fatalf("expected 3 files, got %d: %+v", len(resp.Files), resp.Files)
	}
	wantOrder := []struct {
		name  string
		stage string
	}{
		{"smith-1000-1.log", "smith"},
		{"warden-2000-2.log", "warden"},
		{"mystery-3000-3.log", "other"},
	}
	for i, want := range wantOrder {
		if resp.Files[i].Filename != want.name {
			t.Errorf("file[%d] name = %q, want %q", i, resp.Files[i].Filename, want.name)
		}
		if resp.Files[i].Stage != want.stage {
			t.Errorf("file[%d] stage = %q, want %q", i, resp.Files[i].Stage, want.stage)
		}
		if resp.Files[i].Live {
			t.Errorf("file[%d] unexpectedly live", i)
		}
	}
}

func TestBeadLogs_LiveWorkerFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := newServerWithDefaults(t, nil)

	// Simulate an active worker writing into its worktree .forge-logs dir.
	wtLogDir := filepath.Join(home, ".workers", "anvil-Forge-live", ".forge-logs")
	if err := os.MkdirAll(wtLogDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	livePath := filepath.Join(wtLogDir, "smith-9000-1.log")
	if err := os.WriteFile(livePath, []byte("running…\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := srv.db.InsertWorker(&state.Worker{
		ID:        "anvil-Forge-live-1",
		BeadID:    "Forge-live",
		Anvil:     "anvil",
		Status:    state.WorkerRunning,
		StartedAt: time.Now().UTC(),
		LogPath:   livePath,
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	rec := authedGet(t, srv, "/api/bead/Forge-live/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp beadLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("expected 1 file, got %d: %+v", len(resp.Files), resp.Files)
	}
	f := resp.Files[0]
	if !f.Live || f.WorkerID != "anvil-Forge-live-1" {
		t.Errorf("expected live file with worker id, got %+v", f)
	}
}

func TestBeadLogFile_TailContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServerWithDefaults(t, nil)
	writePreservedLog(t, "Forge-tail", "smith-1-1.log", "a\nb\nc\nd\n", time.Now())

	rec := authedGet(t, srv, "/api/bead/Forge-tail/logs/smith-1-1.log?tail=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Lines) != 2 || out.Lines[0] != "c" || out.Lines[1] != "d" {
		t.Errorf("tail lines = %v, want [c d]", out.Lines)
	}
}

func TestBeadLogFile_MissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServerWithDefaults(t, nil)
	writePreservedLog(t, "Forge-miss", "smith-1-1.log", "a\n", time.Now())

	rec := authedGet(t, srv, "/api/bead/Forge-miss/logs/warden-9-9.log")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing file, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestBeadLogFile_SymlinkEscapeRejected proves the allowlist is enforced after
// symlink resolution: a log entry that is a symlink to a file outside the
// forge-owned roots must not be readable even though its name is a valid bare
// basename.
func TestBeadLogFile_SymlinkEscapeRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := newServerWithDefaults(t, nil)

	// A secret file living outside ~/.forge and outside any /.workers/ dir.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	preservedDir := filepath.Join(home, ".forge", "logs", "Forge-evil")
	if err := os.MkdirAll(preservedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(preservedDir, "escape.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	rec := authedGet(t, srv, "/api/bead/Forge-evil/logs/escape.log")
	if rec.Code == http.StatusOK {
		t.Fatalf("symlink escape leaked content: body=%s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for symlink escape, got %d", rec.Code)
	}
	if got := rec.Body.String(); strings.Contains(got, "TOP-SECRET") {
		t.Fatalf("response leaked secret content: %s", got)
	}
}

func TestIsSafeLogFilename(t *testing.T) {
	good := []string{"smith-1-1.log", "warden-1700000000000-42.log", "temper-1-1.log"}
	for _, name := range good {
		if !isSafeLogFilename(name) {
			t.Errorf("isSafeLogFilename(%q) = false, want true", name)
		}
	}
	bad := []string{
		"",
		".",
		"..",
		"../../etc/passwd",
		"../secret.log",
		"foo/bar.log",
		`foo\bar.log`,
		"..\x2fetc",
		"a..b", // contains ".." substring
		string([]byte{'a', 0, '.', 'l', 'o', 'g'}),
	}
	for _, name := range bad {
		if isSafeLogFilename(name) {
			t.Errorf("isSafeLogFilename(%q) = true, want false", name)
		}
	}
}
