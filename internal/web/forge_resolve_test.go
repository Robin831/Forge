package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// gitRunnerMu serialises swaps of the package-level gitRunner so parallel
// tests cannot race on it.
var gitRunnerMu sync.Mutex

// stubGitRunner installs a temporary gitRunner implementation. The fn
// callback receives the args from the test invocation; tests return canned
// stdout/err so no git subprocess is spawned.
func stubGitRunner(t *testing.T, fn func(args []string) ([]byte, error)) {
	t.Helper()
	gitRunnerMu.Lock()
	prev := gitRunner
	gitRunner = func(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
		return fn(args)
	}
	t.Cleanup(func() {
		gitRunner = prev
		gitRunnerMu.Unlock()
	})
}

func TestForgeResolve_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	req := httptest.NewRequest("POST", "/api/forge/resolve", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestForgeResolve_RejectsInvalidAction(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/forge/resolve", map[string]any{
		"bead_id":    "Forge-abc1",
		"action":     "explode",
		"anvil_name": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown action, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "clear|retry|clarify|unclarify|stop") {
		t.Errorf("expected error to mention valid actions, got %s", rec.Body.String())
	}
}

func TestForgeResolve_RejectsInvalidBeadID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/forge/resolve", map[string]any{
		"bead_id":    "../etc/passwd",
		"action":     "clear",
		"anvil_name": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestForgeResolve_RejectsMissingAnvil(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/forge/resolve", map[string]any{
		"bead_id": "Forge-abc1",
		"action":  "clear",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 missing anvil, got %d", rec.Code)
	}
}

func TestForgeResolve_RejectsClarifyMissingNote(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/forge/resolve", map[string]any{
		"bead_id":    "Forge-abc1",
		"action":     "clarify",
		"anvil_name": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "note is required") {
		t.Errorf("expected note-required error, got %s", rec.Body.String())
	}
}

// verbCase pairs the action string the SPA sends with the IPC command type
// the daemon should receive.
type verbCase struct {
	action     string
	expectType string
}

func TestForgeResolve_DispatchesAllVerbs(t *testing.T) {
	cases := []verbCase{
		{resolveActionClear, "queue_clear"},
		{resolveActionRetry, "queue_retry"},
		{resolveActionClarify, "queue_clarify"},
		{resolveActionUnclarify, "queue_unclarify"},
		{resolveActionStop, "queue_stop"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.action, func(t *testing.T) {
			rh := &recordingHandler{}
			srv := newServerWithDefaults(t, rh.handle)
			cookie := loginAndGetCookie(t, srv)

			body := map[string]any{
				"bead_id":    "Forge-abc1",
				"action":     tc.action,
				"anvil_name": "forge",
				"note":       "operator context",
				"forge_id":   "forge-a",
			}
			rec := postAction(t, srv, cookie, "/api/forge/resolve", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			cmd, ok := rh.lastCommand()
			if !ok {
				t.Fatalf("no command dispatched")
			}
			if cmd.Type != tc.expectType {
				t.Errorf("expected IPC type %q, got %q", tc.expectType, cmd.Type)
			}
			var p ipc.QueueActionPayload
			if err := json.Unmarshal(cmd.Payload, &p); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			if p.BeadID != "Forge-abc1" || p.AnvilName != "forge" || p.Note != "operator context" || p.ForgeID != "forge-a" {
				t.Errorf("payload mismatch: %+v", p)
			}
		})
	}
}

func TestForgeResolve_DaemonErrorBubblesUp(t *testing.T) {
	rh := &recordingHandler{
		resp: ipc.Response{
			Type:    "error",
			Payload: []byte(`{"message":"forge_id does not match owning forge: caller=\"forge-b\" local=\"forge-a\""}`),
		},
	}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/forge/resolve", map[string]any{
		"bead_id":    "Forge-abc1",
		"action":     "clear",
		"anvil_name": "forge",
		"forge_id":   "forge-b",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "forge_id does not match") {
		t.Errorf("expected forge-mismatch error in body, got %s", rec.Body.String())
	}
}

// ---------- /api/forge/escalation/{bead_id} ----------

func TestForgeEscalation_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	req := httptest.NewRequest("GET", "/api/forge/escalation/Forge-abc1", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestForgeEscalation_InvalidBeadID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/forge/escalation/..%2Fevil", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	// chi may 404 the malformed segment outright; either rejection is
	// acceptable as long as the handler does not return 200.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("expected 400 or 404, got %d", rec.Code)
	}
}

func TestForgeEscalation_NoRetryRow_ReturnsEmptyMessage(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/forge/escalation/Forge-abc1?anvil=forge", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got escalationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BeadID != "Forge-abc1" || got.Anvil != "forge" {
		t.Errorf("expected bead/anvil echoed back, got %+v", got)
	}
	if got.EscalationMessage != "" {
		t.Errorf("expected empty escalation message, got %q", got.EscalationMessage)
	}
}

func TestForgeEscalation_ReturnsRetryDetail(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	if err := srv.db.UpsertRetry(&state.RetryRecord{
		BeadID:           "Forge-abc1",
		Anvil:            "forge",
		NeedsHuman:       true,
		DispatchFailures: 3,
		LastError:        "build failed: missing toolchain\nfull stack:\nline 1\nline 2",
	}); err != nil {
		t.Fatalf("upsert retry: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/forge/escalation/Forge-abc1?anvil=forge", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got escalationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.EscalationMessage, "missing toolchain") {
		t.Errorf("expected full escalation message, got %q", got.EscalationMessage)
	}
	// The full multi-line text must come through untruncated — this is the
	// whole reason the endpoint exists, since the queue/event surfaces
	// summarise.
	if !strings.Contains(got.EscalationMessage, "line 2") {
		t.Errorf("expected untruncated last_error, got %q", got.EscalationMessage)
	}
	if got.Retry == nil || !got.Retry.NeedsHuman || got.Retry.DispatchFailures != 3 {
		t.Errorf("retry detail mismatch: %+v", got.Retry)
	}
}

func TestForgeEscalation_GathersGitContextFromWorktree(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	// Seed an anvil + a worker so the handler can resolve a worktree path.
	anvilRoot := t.TempDir()
	worktreePath := filepath.Join(anvilRoot, ".workers", "Forge-abc1")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	srv.SetAnvilLister(func() map[string]string {
		return map[string]string{"forge": anvilRoot}
	})

	if err := srv.db.InsertWorker(&state.Worker{
		ID:        "forge-abc1-123",
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		Branch:    "forge/Forge-abc1",
		Status:    state.WorkerFailed,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert worker: %v", err)
	}
	if err := srv.db.UpsertRetry(&state.RetryRecord{
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		LastError: "smith returned NEEDS_HUMAN: API key missing",
	}); err != nil {
		t.Fatalf("upsert retry: %v", err)
	}

	// Stub gitEnvGetter so the test directory is treated as a valid worktree
	// without requiring a real git repo on disk.
	prevEnvGetter := gitEnvGetter
	gitEnvGetter = func(_ string) []string { return []string{"GIT_TEST=1"} }
	t.Cleanup(func() { gitEnvGetter = prevEnvGetter })

	// Fake git: respond to the verify and log probes the handler issues.
	stubGitRunner(t, func(args []string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "rev-parse" && contains(args, "origin/main"):
			return []byte("deadbeef\n"), nil
		case len(args) >= 2 && args[0] == "rev-parse" && contains(args, "origin/master"):
			return nil, errors.New("not found")
		case len(args) >= 2 && args[0] == "rev-parse" && contains(args, "origin/forge/Forge-abc1"):
			return []byte("cafebabe\n"), nil
		case len(args) >= 1 && args[0] == "log":
			if contains(args, "origin/main..HEAD") {
				return []byte("aaaa fix: A\nbbbb fix: B\n"), nil
			}
			if contains(args, "origin/forge/Forge-abc1") {
				return []byte("aaaa fix: A\n"), nil
			}
			return nil, errors.New("unexpected log args")
		case len(args) >= 1 && args[0] == "diff":
			return []byte(" 2 files changed, 5 insertions(+), 1 deletion(-)\n"), nil
		default:
			return nil, errors.New("unexpected git args")
		}
	})

	req := httptest.NewRequest("GET", "/api/forge/escalation/Forge-abc1?anvil=forge", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got escalationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Branch != "forge/Forge-abc1" {
		t.Errorf("branch: got %q", got.Branch)
	}
	if !got.WorktreeExists {
		t.Errorf("expected worktree_exists=true, body=%s", rec.Body.String())
	}
	if got.Context == nil {
		t.Fatalf("expected git context, body=%s", rec.Body.String())
	}
	if got.Context.ParentBase != "origin/main" || got.Context.DiffRange != "origin/main..HEAD" {
		t.Errorf("parent_base/diff_range mismatch: %+v", got.Context)
	}
	if len(got.Context.LocalCommits) != 2 || !strings.HasPrefix(got.Context.LocalCommits[0], "aaaa ") {
		t.Errorf("local commits: %+v", got.Context.LocalCommits)
	}
	if !got.Context.OriginBranchExists || len(got.Context.OriginCommits) != 1 {
		t.Errorf("origin: %+v", got.Context)
	}
	if !strings.Contains(got.Context.DiffStat, "2 files changed") {
		t.Errorf("diff stat: %q", got.Context.DiffStat)
	}
}

func TestForgeEscalation_WorktreeMissing_NoGitCalls(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	// Anvil points at a directory that exists but has no .workers entry
	// for this bead — the handler should NOT shell to git.
	anvilRoot := t.TempDir()
	srv.SetAnvilLister(func() map[string]string {
		return map[string]string{"forge": anvilRoot}
	})
	if err := srv.db.UpsertRetry(&state.RetryRecord{
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		LastError: "some error",
	}); err != nil {
		t.Fatalf("upsert retry: %v", err)
	}

	called := false
	stubGitRunner(t, func(args []string) ([]byte, error) {
		called = true
		return nil, errors.New("should not be called")
	})

	req := httptest.NewRequest("GET", "/api/forge/escalation/Forge-abc1?anvil=forge", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Errorf("git was called even though worktree is missing")
	}
	var got escalationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WorktreeExists {
		t.Errorf("expected worktree_exists=false")
	}
	if got.Context != nil {
		t.Errorf("expected no git context when worktree missing, got %+v", got.Context)
	}
}

// contains reports whether ss includes s. Used by the stubbed git runner
// to match on a single arg without slicing into the variadic.
func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
