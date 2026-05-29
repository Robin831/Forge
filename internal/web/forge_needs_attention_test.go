package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// getJSON issues an authenticated GET and decodes the JSON body into out.
func getJSON(t *testing.T, srv *Server, cookie, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, rec.Body.String())
		}
	}
	return rec
}

func TestForgeNeedsAttention_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	req := httptest.NewRequest("GET", "/api/forge/needs-attention", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestForgeNeedsAttention_EmptyReturnsArray(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := getJSON(t, srv, cookie, "/api/forge/needs-attention", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Items must serialise as [] not null so the SPA can iterate without a
	// null check.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["items"]) != "[]" {
		t.Errorf("expected items to be [], got %s", string(raw["items"]))
	}
}

// TestForgeNeedsAttention_ListsNeedsHumanAndClarification verifies the list is
// driven by the retries table (NeedsHumanBeads + ClarificationNeededBeads),
// independent of the workers table: a needs_human bead with NO worker row
// still appears, and worker_row_exists reflects whether a row was found.
func TestForgeNeedsAttention_ListsNeedsHumanAndClarification(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	// Graceful escalation: needs_human, but the worker exited status=done.
	if err := srv.db.UpsertRetry(&state.RetryRecord{
		BeadID:           "Forge-aaaa",
		Anvil:            "forge",
		NeedsHuman:       true,
		RecoveryFailures: 1,
		LastError:        "smith returned NEEDS_HUMAN: rework premise\nfull detail line",
	}); err != nil {
		t.Fatalf("upsert retry aaaa: %v", err)
	}
	if err := srv.db.InsertWorker(&state.Worker{
		ID:        "forge-aaaa-1",
		BeadID:    "Forge-aaaa",
		Anvil:     "forge",
		Status:    state.WorkerDone,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert worker aaaa: %v", err)
	}

	// Aged-out failed worker with NO worker row at all (simulating a row that
	// has been pruned). Stranded-branch escalation via its latest event.
	if err := srv.db.UpsertRetry(&state.RetryRecord{
		BeadID:           "Forge-bbbb",
		Anvil:            "forge",
		NeedsHuman:       true,
		DispatchFailures: 2,
		LastError:        "origin branch has commits but no PR",
	}); err != nil {
		t.Fatalf("upsert retry bbbb: %v", err)
	}
	if err := srv.db.LogEvent(state.EventDispatchBlockedStrandedBranch, "stranded", "Forge-bbbb", "forge"); err != nil {
		t.Fatalf("log event bbbb: %v", err)
	}

	// Clarification-class bead (needs_human=0, clarification_needed=1).
	if err := srv.db.SetClarificationNeeded("Forge-cccc", "forge", true, "what does X mean?"); err != nil {
		t.Fatalf("set clarification cccc: %v", err)
	}

	// Seed a title for one bead via the queue cache.
	if err := srv.db.ReplaceQueueCacheForAnvils([]string{"forge"}, []state.QueueItem{
		{BeadID: "Forge-aaaa", Anvil: "forge", Title: "Graceful escalation bead", Section: state.QueueSectionReady},
	}); err != nil {
		t.Fatalf("replace queue cache: %v", err)
	}

	var got needsAttentionResponse
	rec := getJSON(t, srv, cookie, "/api/forge/needs-attention", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	byID := map[string]needsAttentionItem{}
	for _, it := range got.Items {
		byID[it.BeadID] = it
	}
	if len(byID) != 3 {
		t.Fatalf("expected 3 items, got %d: %+v", len(byID), got.Items)
	}

	aaaa := byID["Forge-aaaa"]
	if !aaaa.NeedsHuman {
		t.Errorf("aaaa: expected needs_human=true")
	}
	if !aaaa.WorkerRowExists {
		t.Errorf("aaaa: expected worker_row_exists=true (status=done row present)")
	}
	if aaaa.Title != "Graceful escalation bead" {
		t.Errorf("aaaa: expected title from queue cache, got %q", aaaa.Title)
	}
	if aaaa.LastError == "" || aaaa.EscalationType == "" {
		t.Errorf("aaaa: expected last_error + escalation_type, got %+v", aaaa)
	}

	bbbb := byID["Forge-bbbb"]
	if bbbb.WorkerRowExists {
		t.Errorf("bbbb: expected worker_row_exists=false (no worker row)")
	}
	if bbbb.EscalationType != escTypeStrandedBranch {
		t.Errorf("bbbb: expected escalation_type %q, got %q", escTypeStrandedBranch, bbbb.EscalationType)
	}

	cccc := byID["Forge-cccc"]
	if !cccc.ClarificationNeeded {
		t.Errorf("cccc: expected clarification_needed=true")
	}
	if cccc.EscalationType != escTypeClarification {
		t.Errorf("cccc: expected escalation_type %q, got %q", escTypeClarification, cccc.EscalationType)
	}
}

// TestDeriveEscalationType_PrefersLatestEvent verifies the most recent
// classifying event wins over an earlier one, and that the retry-flag
// fallback applies when no classifying event exists.
func TestDeriveEscalationType_PrefersLatestEvent(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	// First a dispatch-blocked event, then a smith_failed — the later event
	// must win.
	if err := srv.db.LogEvent(state.EventDispatchBlockedStrandedBranch, "x", "Forge-dddd", "forge"); err != nil {
		t.Fatalf("log event: %v", err)
	}
	if err := srv.db.LogEvent(state.EventSmithFailed, "y", "Forge-dddd", "forge"); err != nil {
		t.Fatalf("log event: %v", err)
	}
	got := srv.deriveEscalationType(state.RetryRecord{BeadID: "Forge-dddd", Anvil: "forge", NeedsHuman: true})
	if got != escTypeSmithFailed {
		t.Errorf("expected latest event smith_failed to win, got %q", got)
	}

	// No classifying event: clarification flag → clarification.
	got = srv.deriveEscalationType(state.RetryRecord{BeadID: "Forge-eeee", Anvil: "forge", ClarificationNeeded: true})
	if got != escTypeClarification {
		t.Errorf("expected clarification fallback, got %q", got)
	}

	// No event, dispatch failures only → dispatch_failed.
	got = srv.deriveEscalationType(state.RetryRecord{BeadID: "Forge-ffff", Anvil: "forge", NeedsHuman: true, DispatchFailures: 2})
	if got != escTypeDispatchFailed {
		t.Errorf("expected dispatch_failed fallback, got %q", got)
	}
}
