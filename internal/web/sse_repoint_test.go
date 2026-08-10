package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// Regression (Forge-hyla): a live worker stream resolved the row's log_path
// once at connection time and tailed that file forever. Workers switch log
// files mid-life (Smith iterations, the Warden review, burnish fix → verify),
// so an open panel froze on the finished stage's output until the operator
// refreshed the page. The stream must notice the repoint, emit a divider line,
// and continue with the new file's content.
func TestWorkerLogStream_FollowsLogPathRepoint(t *testing.T) {
	oldInterval := logRepointInterval
	logRepointInterval = 100 * time.Millisecond
	defer func() { logRepointInterval = oldInterval }()

	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	logDir := filepath.Join(tempHome, ".forge", "logs", "Forge-abc1")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	smithLog := filepath.Join(logDir, "smith-1.log")
	wardenLog := filepath.Join(logDir, "warden-1.log")
	if err := os.WriteFile(smithLog, []byte("smith says hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	insertWorkerRow(t, srv, "w-repoint", state.WorkerRunning, "smith", smithLog)

	// Mid-stream, the worker moves on to its Warden session: a new log file
	// appears and the row's log_path is repointed at it.
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := os.WriteFile(wardenLog, []byte("warden says verdict\n"), 0o644); err != nil {
			t.Error(err)
			return
		}
		if err := srv.db.UpdateWorkerLogPath("w-repoint", wardenLog); err != nil {
			t.Error(err)
		}
	}()

	req := httptest.NewRequest("GET", "/api/worker/w-repoint/stream", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	// The 500ms tail ticker drives the repoint check; two ticks are enough to
	// notice the swap and stream the new file.
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req.WithContext(ctx))

	body := rec.Body.String()
	for _, want := range []string{"smith says hello", "warden log started", "warden says verdict"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q\nbody:\n%s", want, body)
		}
	}
	if divider, line := strings.Index(body, "warden log started"), strings.Index(body, "warden says verdict"); divider >= 0 && line >= 0 && divider > line {
		t.Errorf("divider must precede the new file's content\nbody:\n%s", body)
	}
}
