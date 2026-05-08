package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

func TestPRsAll_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/prs/all", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", rec.Code)
	}
}

func TestPRsAll_EmptyResponseShape(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/prs/all", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// All three sections must be present and serialise as [] (not null) so
	// the SPA can rely on indexing without null checks.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, field := range []string{"forge_prs", "external_prs", "recently_merged"} {
		raw, ok := body[field]
		if !ok {
			t.Errorf("field %q missing from response", field)
			continue
		}
		if string(raw) == "null" {
			t.Errorf("field %q is null; want []", field)
		}
	}
}

func TestPRsAll_SectionAssignment(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	// Insert one forge-managed PR, one external PR, and one recently-merged PR.
	now := time.Now().UTC()
	forgePR := &state.PR{
		Number:    1, Anvil: "anvil-a", BeadID: "Forge-aaaa",
		Branch: "feature/x", Status: state.PROpen, Title: "Forge change",
		CreatedAt: now,
	}
	if err := srv.db.InsertPR(forgePR); err != nil {
		t.Fatalf("insert forge PR: %v", err)
	}
	// InsertPR sets bellows_managed=1 by default for forge PRs.

	externalPR := &state.PR{
		Number:    2, Anvil: "anvil-a", BeadID: "ext-2",
		Branch: "patch", Status: state.PROpen, Title: "External patch",
		CreatedAt: now,
	}
	if err := srv.db.InsertPR(externalPR); err != nil {
		t.Fatalf("insert external PR: %v", err)
	}
	if err := srv.db.UpdatePRBellowsManaged(externalPR.ID, false); err != nil {
		t.Fatalf("clear bellows on external: %v", err)
	}

	mergedPR := &state.PR{
		Number:    3, Anvil: "anvil-a", BeadID: "Forge-bbbb",
		Branch: "feature/y", Status: state.PROpen, Title: "Old change",
		CreatedAt: now.Add(-2 * 24 * time.Hour),
	}
	if err := srv.db.InsertPR(mergedPR); err != nil {
		t.Fatalf("insert merged PR: %v", err)
	}
	if err := srv.db.UpdatePRStatus(mergedPR.ID, state.PRMerged); err != nil {
		t.Fatalf("mark merged: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/prs/all", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp prsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Forge PR (open, bellows-managed, non-ext bead) goes to forge_prs.
	if len(resp.ForgePRs) != 1 {
		t.Fatalf("forge_prs: want 1, got %d (%+v)", len(resp.ForgePRs), resp.ForgePRs)
	}
	if got := resp.ForgePRs[0]; got.Number != 1 || got.IsExternal {
		t.Errorf("forge_prs[0] unexpected: %+v", got)
	}

	// External PR (ext-* bead, not bellows-managed) goes to external_prs.
	if len(resp.ExternalPRs) != 1 {
		t.Fatalf("external_prs: want 1, got %d (%+v)", len(resp.ExternalPRs), resp.ExternalPRs)
	}
	if got := resp.ExternalPRs[0]; got.Number != 2 || !got.IsExternal {
		t.Errorf("external_prs[0] unexpected: %+v", got)
	}

	// Merged PR within the 7-day window goes to recently_merged.
	if len(resp.RecentlyMerged) != 1 {
		t.Fatalf("recently_merged: want 1, got %d (%+v)", len(resp.RecentlyMerged), resp.RecentlyMerged)
	}
	if got := resp.RecentlyMerged[0]; got.Number != 3 || got.Status != string(state.PRMerged) {
		t.Errorf("recently_merged[0] unexpected: %+v", got)
	}
}

func TestPRsAll_RecentlyMerged_RespectsWindow(t *testing.T) {
	// A PR last_checked >7 days ago must be excluded from recently_merged.
	srv := newServerWithDefaults(t, nil)

	pr := &state.PR{
		Number:    99, Anvil: "anvil-a", BeadID: "Forge-cccc",
		Branch: "old", Status: state.PROpen, Title: "Stale merged",
		CreatedAt: time.Now().UTC().Add(-30 * 24 * time.Hour),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := srv.db.UpdatePRStatus(pr.ID, state.PRMerged); err != nil {
		t.Fatalf("mark merged: %v", err)
	}
	// Backdate last_checked so the merge falls outside the 7-day window.
	if _, err := srv.db.Conn().Exec(
		`UPDATE prs SET last_checked = ? WHERE id = ?`,
		time.Now().UTC().Add(-30*24*time.Hour).Format("2006-01-02T15:04:05.000000000Z07:00"),
		pr.ID,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/prs/all", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp prsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.RecentlyMerged) != 0 {
		t.Errorf("expected stale PR to be excluded, got %+v", resp.RecentlyMerged)
	}
}
