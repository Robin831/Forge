package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// insertOpenPR creates a PR row in state.db and returns its DB ID so the
// action tests have a valid {id} path parameter to address.
func insertOpenPR(t *testing.T, srv *Server, beadID, branch string) int {
	t.Helper()
	pr := &state.PR{
		Number:    101,
		Anvil:     "anvil-a",
		BeadID:    beadID,
		Branch:    branch,
		Status:    state.PROpen,
		Title:     "Test PR",
		CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert pr: %v", err)
	}
	return pr.ID
}

func TestPRActions_RequireAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	for _, action := range []string{
		"merge", "close", "approve", "bellows",
		"bellows/detach", "bellows/resume",
		"fix-ci", "fix-comments", "fix-conflicts", "reset-counters",
	} {
		path := "/api/prs/1/" + action
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", path, rec.Code)
		}
	}
}

func TestPRActions_InvalidID(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/prs/abc/merge", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPRActions_NotFound(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/prs/9999/merge", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPRActions_Merge_DispatchesIPC(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "Forge-aaaa", "feature/x")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/merge", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "pr_action" {
		t.Fatalf("expected pr_action, got %s", cmd.Type)
	}
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "merge" || p.PRID != prID || p.PRNumber != 101 || p.Anvil != "anvil-a" || p.BeadID != "Forge-aaaa" || p.Branch != "feature/x" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestPRActions_Approve_DispatchesIPC(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "ext-101", "patch")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/approve", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "pr_action" {
		t.Fatalf("expected pr_action, got %s", cmd.Type)
	}
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "approve" || p.PRNumber != 101 || p.Anvil != "anvil-a" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestPRActions_Close_DispatchesIPC(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "ext-101", "patch")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/close", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "close" {
		t.Errorf("expected action=close, got %q", p.Action)
	}
}

func TestPRActions_Bellows_DispatchesIPC(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "Forge-aaaa", "feature/x")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/bellows", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "assign_bellows" {
		t.Errorf("expected action=assign_bellows, got %q", p.Action)
	}
}

func TestPRActions_FixCI_DispatchesQuench(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "Forge-aaaa", "feature/x")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/fix-ci", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "quench" || p.Branch != "feature/x" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestPRActions_FixComments_DispatchesBurnish(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "Forge-aaaa", "feature/x")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/fix-comments", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "burnish" || p.Branch != "feature/x" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestPRActions_FixConflicts_DispatchesRebase(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "Forge-aaaa", "feature/x")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/fix-conflicts", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "rebase" || p.Branch != "feature/x" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestPRActions_ResetCounters_DispatchesRetry(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "Forge-aaaa", "feature/x")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/reset-counters", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "retry_bead" {
		t.Fatalf("expected retry_bead, got %s", cmd.Type)
	}
	var p ipc.RetryBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.PRID != prID || p.BeadID != "Forge-aaaa" || p.Anvil != "anvil-a" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestPRActions_BranchRequiredActions_RejectsMissingBranch(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	// Insert a PR with no branch — should reject before dispatching.
	prID := insertOpenPR(t, srv, "Forge-aaaa", "")

	for _, action := range []string{"fix-ci", "fix-comments", "fix-conflicts"} {
		rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/%s", prID, action), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400 with empty branch, got %d body=%s", action, rec.Code, rec.Body.String())
		}
	}
}
