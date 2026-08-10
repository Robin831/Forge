package web

import (
	"encoding/json"
	"fmt"
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

func TestPRFindings_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/prs/1/findings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", rec.Code)
	}
}

func TestPRFindings_NotFound(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/prs/999/findings", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown PR, got %d", rec.Code)
	}
}

func TestPRFindings_EmptyShape(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	pr := &state.PR{
		Number: 8, Anvil: "anvil-a", BeadID: "Forge-bbbb",
		Branch: "b", Status: state.PROpen, CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/prs/%d/findings", pr.ID), nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// findings must serialise as [] (not null) so the SPA can index it; run is
	// null until Assay records a pass.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(body["findings"]) == "null" {
		t.Errorf("findings should be [] not null")
	}
	if string(body["run"]) != "null" {
		t.Errorf("run should be null when no run recorded, got %s", body["run"])
	}
}

func TestPRFindings_ReturnsFindingsAndRun(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	pr := &state.PR{
		Number: 7, Anvil: "anvil-a", BeadID: "Forge-aaaa",
		Branch: "feature/x", Status: state.PROpen, Title: "Change",
		CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert PR: %v", err)
	}

	// One Important finding (open) and one Nit finding (posted).
	if err := srv.db.InsertFinding(state.Finding{
		Anvil: "anvil-a", PRNumber: 7, HeadSHA: "deadbeef", FindingHash: "h1",
		File: "a.go", Anchor: "a.go:1", Severity: "Important", Category: "logic",
		Title: "Bug here", Body: "details",
	}); err != nil {
		t.Fatalf("insert finding 1: %v", err)
	}
	if err := srv.db.InsertFinding(state.Finding{
		Anvil: "anvil-a", PRNumber: 7, HeadSHA: "deadbeef", FindingHash: "h2",
		File: "b.go", Anchor: "b.go:2", Severity: "Nit", Category: "style",
		Title: "Style nit", Body: "x", Posted: true,
	}); err != nil {
		t.Fatalf("insert finding 2: %v", err)
	}

	finished := time.Now().UTC()
	if err := srv.db.RecordAssayRun(&state.AssayRun{
		Anvil: "anvil-a", PRNumber: 7, HeadSHA: "deadbeef",
		StartedAt: finished.Add(-time.Minute), FinishedAt: &finished,
		FindingsCount: 2, PostedCount: 1, CostUSD: 0.5,
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/prs/%d/findings", pr.ID), nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp prFindingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.PR != 7 || resp.Anvil != "anvil-a" {
		t.Errorf("unexpected pr/anvil: %+v", resp)
	}
	if len(resp.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d (%+v)", len(resp.Findings), resp.Findings)
	}
	// Important sorts before Nit.
	if resp.Findings[0].Severity != "Important" {
		t.Errorf("expected Important first, got %q", resp.Findings[0].Severity)
	}
	if resp.Findings[0].Status != "open" {
		t.Errorf("expected open status, got %q", resp.Findings[0].Status)
	}
	if resp.Findings[0].Message != "Bug here" {
		t.Errorf("expected message from title, got %q", resp.Findings[0].Message)
	}
	if resp.Findings[1].Status != "posted" {
		t.Errorf("expected posted status, got %q", resp.Findings[1].Status)
	}
	if resp.Run == nil || resp.Run.Status != "complete" {
		t.Errorf("expected complete run, got %+v", resp.Run)
	}
	if resp.Run != nil && resp.Run.HeadSHA != "deadbeef" {
		t.Errorf("expected run head_sha deadbeef, got %q", resp.Run.HeadSHA)
	}
}

// TestPRFindings_PartialRunReportsCoverage covers the run whose passes only
// half reviewed the head: it must surface as `partial` (never `error`, which
// the pass-error string alone would produce) and carry the tally, the named
// passes and the rendered status text the panel shows.
func TestPRFindings_PartialRunReportsCoverage(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	pr := &state.PR{
		Number: 11, Anvil: "anvil-a", BeadID: "Forge-bbbb",
		Branch: "feature/y", Status: state.PROpen, Title: "Change",
		CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert PR: %v", err)
	}

	finished := time.Now().UTC()
	if err := srv.db.RecordAssayRun(&state.AssayRun{
		Anvil: "anvil-a", PRNumber: 11, HeadSHA: "cafebabe",
		StartedAt: finished.Add(-time.Minute), FinishedAt: &finished,
		FindingsCount: 1, PostedCount: 1,
		Error:           "assay pass logic: provider claude failed (exit 1, subtype error_max_turns)",
		Status:          state.AssayStatusPartial,
		CompletedPasses: 4, TotalPasses: 5,
		FailedPasses: []state.AssayPassFailure{{Name: "logic", Reason: "error_max_turns"}},
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}

	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/prs/%d/findings", pr.ID), nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp prFindingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Run == nil {
		t.Fatal("expected a run in the payload")
	}
	if resp.Run.Status != "partial" {
		t.Errorf("expected partial status, got %q", resp.Run.Status)
	}
	if resp.Run.CompletedPasses != 4 || resp.Run.TotalPasses != 5 {
		t.Errorf("expected 4/5 passes, got %d/%d", resp.Run.CompletedPasses, resp.Run.TotalPasses)
	}
	if len(resp.Run.FailedPasses) != 1 || resp.Run.FailedPasses[0].Name != "logic" ||
		resp.Run.FailedPasses[0].Reason != "error_max_turns" {
		t.Errorf("unexpected failed passes: %+v", resp.Run.FailedPasses)
	}
	want := "partial: 4 of 5 passes completed (failed: logic — error_max_turns)"
	if resp.Run.StatusText != want {
		t.Errorf("status_text = %q; want %q", resp.Run.StatusText, want)
	}
}

func TestRunStatus(t *testing.T) {
	tests := []struct {
		name                    string
		finished                bool
		errMsg, skipped, stored string
		want                    string
	}{
		{"unfinished", false, "", "", "", "running"},
		{"partial wins over its pass errors", true, "pass blew up", "", state.AssayStatusPartial, "partial"},
		{"error", true, "boom", "", "", "error"},
		{"skipped", true, "", "diff fetch failed", "", "skipped"},
		{"complete", true, "", "", state.AssayStatusComplete, "complete"},
		{"legacy row with no stored status", true, "", "", "", "complete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runStatus(tt.finished, tt.errMsg, tt.skipped, tt.stored); got != tt.want {
				t.Errorf("runStatus = %q; want %q", got, tt.want)
			}
		})
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
