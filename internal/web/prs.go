package web

import (
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

// recentlyMergedWindow is how far back the /prs tab's "Recently merged"
// section looks. PRs whose last_checked timestamp falls within this window
// after a merge transition are returned.
const recentlyMergedWindow = 7 * 24 * time.Hour

// prItemJSON is the wire shape for one PR row in /api/prs/all. The fields
// align with the frontend's PRItem type so callers can use the response
// without re-mapping.
type prItemJSON struct {
	ID              int    `json:"id,omitempty"`
	Number          int    `json:"number"`
	Anvil           string `json:"anvil"`
	Branch          string `json:"branch,omitempty"`
	BaseBranch      string `json:"base_branch,omitempty"`
	Title           string `json:"title,omitempty"`
	Status          string `json:"status"`
	IsExternal      bool   `json:"is_external"`
	IsConflicting   bool   `json:"is_conflicting,omitempty"`
	CIPassing       bool   `json:"ci_passing,omitempty"`
	ReviewsApproved bool   `json:"reviews_approved,omitempty"`
	BellowsAssigned bool   `json:"bellows_assigned,omitempty"`
	BellowsDetached bool   `json:"bellows_detached,omitempty"`
	CIFixCount      int    `json:"ci_fix_count,omitempty"`
	ReviewFixCount  int    `json:"review_fix_count,omitempty"`
	RebaseCount     int    `json:"rebase_count,omitempty"`
	BeadID          string `json:"bead_id,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	MergedAt        string `json:"merged_at,omitempty"`
}

// prsResponse is the JSON body returned by GET /api/prs/all. The keys
// match the frontend PRsResponse / PRSectionKind so the SPA can index the
// payload directly.
type prsResponse struct {
	ForgePRs       []prItemJSON `json:"forge_prs"`
	ExternalPRs    []prItemJSON `json:"external_prs"`
	RecentlyMerged []prItemJSON `json:"recently_merged"`
}

// handlePRs returns the three PR sections shown on the /prs tab:
//   - forge_prs:       open PRs created by THIS Forge instance and currently
//     under bellows lifecycle management.
//   - external_prs:    open PRs that Forge did not create or no longer manages
//     (other contributors, sibling Forge instances, untracked PRs reconciled
//     into state.db with synthetic ext-* IDs).
//   - recently_merged: any PR (forge or external) merged within the last
//     recentlyMergedWindow.
//
// All three are read from state.db via two queries (OpenPRs for open PRs,
// recentlyMergedPRs for the merged window). The daemon's reconcileOpenPRs
// goroutine keeps the table in sync with `gh pr list` so this handler never
// shells out per request. The frontend refresh button re-fetches this
// endpoint directly; it does not trigger the reconcile_prs IPC command.
func (s *Server) handlePRs(w http.ResponseWriter, _ *http.Request) {
	openPRs, err := s.db.OpenPRs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load open PRs: "+err.Error())
		return
	}

	resp := prsResponse{
		ForgePRs:       []prItemJSON{},
		ExternalPRs:    []prItemJSON{},
		RecentlyMerged: []prItemJSON{},
	}
	for _, pr := range openPRs {
		item := mapPRToJSON(pr)
		if isForgeManagedPR(pr) {
			resp.ForgePRs = append(resp.ForgePRs, item)
		} else {
			resp.ExternalPRs = append(resp.ExternalPRs, item)
		}
	}

	merged, err := s.recentlyMergedPRs(recentlyMergedWindow)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load merged PRs: "+err.Error())
		return
	}
	resp.RecentlyMerged = merged

	writeJSON(w, http.StatusOK, resp)
}

// isForgeManagedPR reports whether the given PR is one Forge created and is
// still actively orchestrating. Synthetic ext-* IDs and PRs whose
// bellows_managed flag has been cleared (e.g. PRs adopted from a sibling
// Forge instance) belong in the External section.
func isForgeManagedPR(pr state.PR) bool {
	if pr.IsExternal() {
		return false
	}
	return pr.BellowsManaged
}

// mapPRToJSON converts a state.PR to the wire shape consumed by the SPA.
func mapPRToJSON(pr state.PR) prItemJSON {
	item := prItemJSON{
		ID:              pr.ID,
		Number:          pr.Number,
		Anvil:           pr.Anvil,
		Branch:          pr.Branch,
		BaseBranch:      pr.BaseBranch,
		Title:           pr.Title,
		Status:          string(pr.Status),
		IsExternal:      pr.IsExternal(),
		IsConflicting:   pr.IsConflicting,
		CIPassing:       pr.CIPassing,
		ReviewsApproved: pr.HasApproval,
		BellowsAssigned: pr.BellowsManaged,
		BellowsDetached: pr.BellowsDetached,
		CIFixCount:      pr.CIFixCount,
		ReviewFixCount:  pr.ReviewFixCount,
		RebaseCount:     pr.RebaseCount,
		BeadID:          pr.BeadID,
	}
	if !pr.CreatedAt.IsZero() {
		item.CreatedAt = pr.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if pr.LastChecked != nil {
		item.UpdatedAt = pr.LastChecked.UTC().Format(time.RFC3339Nano)
		if pr.Status == state.PRMerged {
			item.MergedAt = pr.LastChecked.UTC().Format(time.RFC3339Nano)
		}
	}
	return item
}

// recentlyMergedPRs returns merged PRs (forge + external) whose last_checked
// timestamp falls within the given window. The state package only exposes a
// helper that excludes ext-* IDs, so we query directly here to give the /prs
// tab a complete view. Results are sorted newest-first.
func (s *Server) recentlyMergedPRs(window time.Duration) ([]prItemJSON, error) {
	conn := s.db.Conn()
	if conn == nil {
		return []prItemJSON{}, nil
	}
	// Match the canonical state.dbTimeLayout (fixed-width nanos with offset)
	// so lexicographic comparison against last_checked stays correct.
	// Do NOT force UTC here — UpdatePRStatus writes time.Now() in the local
	// timezone, so the cutoff must use the same offset to keep comparisons valid.
	cutoff := time.Now().Add(-window).Format("2006-01-02T15:04:05.000000000Z07:00")
	rows, err := conn.Query(`SELECT id, number, anvil, bead_id, branch, COALESCE(base_branch,''),
		status, created_at, last_checked, ci_fix_count, review_fix_count, rebase_count,
		ci_passing, is_conflicting, has_approval, COALESCE(title,''), bellows_managed,
		bellows_detached
		FROM prs
		WHERE status = 'merged' AND last_checked IS NOT NULL AND last_checked >= ?
		ORDER BY last_checked DESC, id DESC
		LIMIT 200`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []prItemJSON{}
	for rows.Next() {
		var (
			id, number                                       int
			anvil, beadID, branch, base, status, title       string
			createdAt                                        string
			lastChecked                                      sql.NullString
			ciFix, reviewFix, rebase                         int
			ciPassing, conflicting, approved, bellowsManaged int
			bellowsDetached                                  int
		)
		if err := rows.Scan(&id, &number, &anvil, &beadID, &branch, &base, &status, &createdAt,
			&lastChecked, &ciFix, &reviewFix, &rebase, &ciPassing, &conflicting,
			&approved, &title, &bellowsManaged, &bellowsDetached); err != nil {
			s.logger.Warn("recently merged PR row scan failed", "error", err)
			continue
		}
		isExt := strings.HasPrefix(beadID, "ext-")
		item := prItemJSON{
			ID:              id,
			Number:          number,
			Anvil:           anvil,
			Branch:          branch,
			BaseBranch:      base,
			Title:           title,
			Status:          status,
			IsExternal:      isExt,
			IsConflicting:   conflicting != 0,
			CIPassing:       ciPassing != 0,
			ReviewsApproved: approved != 0,
			BellowsAssigned: bellowsManaged != 0,
			BellowsDetached: bellowsDetached != 0,
			CIFixCount:      ciFix,
			ReviewFixCount:  reviewFix,
			RebaseCount:     rebase,
			BeadID:          beadID,
		}
		if t := parseAnyTime(createdAt); !t.IsZero() {
			item.CreatedAt = t.UTC().Format(time.RFC3339Nano)
		}
		if lastChecked.Valid {
			if t := parseAnyTime(lastChecked.String); !t.IsZero() {
				item.UpdatedAt = t.UTC().Format(time.RFC3339Nano)
				item.MergedAt = t.UTC().Format(time.RFC3339Nano)
			}
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Defensive: SQL ORDER BY on the stored timestamp string already yields
	// the right order, but stay tolerant of any rows whose last_checked
	// value cannot be compared lexically by re-sorting on the parsed time.
	sort.SliceStable(out, func(i, j int) bool {
		ti := parseAnyTime(out[i].MergedAt)
		tj := parseAnyTime(out[j].MergedAt)
		return ti.After(tj)
	})
	return out, nil
}

// findingJSON is the wire shape for one Assay finding on a PR. The first
// seven fields (id, pr, anvil, status, severity, message, timestamp) form the
// stable contract the typed frontend client mirrors; the remaining fields are
// optional context the PR detail panel renders when present.
type findingJSON struct {
	ID        int    `json:"id"`
	PR        int    `json:"pr"`
	Anvil     string `json:"anvil"`
	Status    string `json:"status"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
	File      string `json:"file,omitempty"`
	Anchor    string `json:"anchor,omitempty"`
	Category  string `json:"category,omitempty"`
	Body      string `json:"body,omitempty"`
}

// assayRunJSON summarises the most recent Assay review pass over a PR so the
// detail panel can show "rerun" progress (running → error → skipped → complete).
type assayRunJSON struct {
	Status        string  `json:"status"`
	HeadSHA       string  `json:"head_sha,omitempty"`
	StartedAt     string  `json:"started_at,omitempty"`
	FinishedAt    string  `json:"finished_at,omitempty"`
	DurationMs    int64   `json:"duration_ms,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
	FindingsCount int     `json:"findings_count"`
	PostedCount   int     `json:"posted_count"`
	ShadowMode    bool    `json:"shadow_mode,omitempty"`
	SkippedReason string  `json:"skipped_reason,omitempty"`
	Error         string  `json:"error,omitempty"`
	// Coverage: how many of the run's review passes actually looked at this
	// head, and which ones did not. Present so the panel can say a `partial`
	// run's findings are not a full review instead of leaving the operator to
	// infer it from the error string.
	CompletedPasses int                 `json:"completed_passes,omitempty"`
	TotalPasses     int                 `json:"total_passes,omitempty"`
	FailedPasses    []assayPassFailJSON `json:"failed_passes,omitempty"`
	StatusText      string              `json:"status_text,omitempty"`
}

// assayPassFailJSON is one Assay pass that did not review the head, and why.
type assayPassFailJSON struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

// prFindingsResponse is the JSON body returned by GET /api/prs/{id}/findings
// and emitted on the findings SSE channel. `run` is null until Assay has
// recorded at least one review pass for the PR; `findings` is always an array.
type prFindingsResponse struct {
	PR       int           `json:"pr"`
	Anvil    string        `json:"anvil"`
	Run      *assayRunJSON `json:"run"`
	Findings []findingJSON `json:"findings"`
}

// handlePRFindings returns the Assay findings (and latest run status) for the
// PR identified by the {id} path parameter. The PR row is resolved from
// state.db so the anvil/number key the pr_findings table is queried with.
func (s *Server) handlePRFindings(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.requirePR(w, r)
	if !ok {
		return
	}
	resp, err := s.collectPRFindings(ctx.pr.Anvil, ctx.pr.Number)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load findings: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// collectPRFindings reads the findings and most-recent Assay run for one PR.
// It queries state.db directly (like recentlyMergedPRs) so the change stays
// confined to the web package. Findings are ordered Important → PreExisting →
// Nit, then by insertion order, so the most actionable items sort first.
func (s *Server) collectPRFindings(anvil string, prNumber int) (prFindingsResponse, error) {
	resp := prFindingsResponse{
		PR:       prNumber,
		Anvil:    anvil,
		Findings: []findingJSON{},
	}
	conn := s.db.Conn()
	if conn == nil {
		return resp, nil
	}

	rows, err := conn.Query(`SELECT id, pr_number, anvil, head_sha, file, anchor,
		severity, category, title, body, posted, resolved_at, created_at
		FROM pr_findings
		WHERE anvil = ? AND pr_number = ?
		ORDER BY
			CASE severity WHEN 'Important' THEN 0 WHEN 'PreExisting' THEN 1 WHEN 'Nit' THEN 2 ELSE 3 END,
			id`, anvil, prNumber)
	if err != nil {
		return resp, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, pr                                        int
			av, headSHA, file, anchor, severity, category string
			title, body, createdAt                        string
			posted                                        int
			resolvedAt                                    sql.NullString
		)
		if err := rows.Scan(&id, &pr, &av, &headSHA, &file, &anchor, &severity,
			&category, &title, &body, &posted, &resolvedAt, &createdAt); err != nil {
			s.logger.Warn("pr finding row scan failed", "error", err)
			continue
		}
		item := findingJSON{
			ID:       id,
			PR:       pr,
			Anvil:    av,
			Status:   findingStatus(posted != 0, resolvedAt.Valid),
			Severity: severity,
			Message:  title,
			HeadSHA:  headSHA,
			File:     file,
			Anchor:   anchor,
			Category: category,
			Body:     body,
		}
		if t := parseAnyTime(createdAt); !t.IsZero() {
			item.Timestamp = t.UTC().Format(time.RFC3339Nano)
		}
		resp.Findings = append(resp.Findings, item)
	}
	if err := rows.Err(); err != nil {
		return resp, err
	}

	run, err := s.latestAssayRun(anvil, prNumber)
	if err != nil {
		return resp, err
	}
	resp.Run = run
	return resp, nil
}

// latestAssayRun returns the most recent assay_runs row for the anvil/PR as a
// wire object, or (nil, nil) when no run has been recorded yet.
func (s *Server) latestAssayRun(anvil string, prNumber int) (*assayRunJSON, error) {
	conn := s.db.Conn()
	if conn == nil {
		return nil, nil
	}
	var (
		headSHA, skipped, errMsg           string
		startedAt                          string
		finishedAt                         sql.NullString
		durationMs                         int64
		costUSD                            float64
		findingsCount, postedCount, shadow int
		runStatusRaw, failedPassesRaw      string
		completedPasses, totalPasses       int
	)
	err := conn.QueryRow(`SELECT head_sha, started_at, finished_at, duration_ms,
		cost_usd, findings_count, posted_count, shadow_mode, skipped_reason, error,
		status, completed_passes, total_passes, failed_passes
		FROM assay_runs
		WHERE anvil = ? AND pr_number = ?
		ORDER BY id DESC LIMIT 1`, anvil, prNumber).Scan(
		&headSHA, &startedAt, &finishedAt, &durationMs, &costUSD,
		&findingsCount, &postedCount, &shadow, &skipped, &errMsg,
		&runStatusRaw, &completedPasses, &totalPasses, &failedPassesRaw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	failed := state.DecodeAssayPassFailures(failedPassesRaw)
	status := runStatus(finishedAt.Valid, errMsg, skipped, runStatusRaw)
	run := &assayRunJSON{
		Status:          status,
		HeadSHA:         headSHA,
		DurationMs:      durationMs,
		CostUSD:         costUSD,
		FindingsCount:   findingsCount,
		PostedCount:     postedCount,
		ShadowMode:      shadow != 0,
		SkippedReason:   skipped,
		Error:           errMsg,
		CompletedPasses: completedPasses,
		TotalPasses:     totalPasses,
		FailedPasses:    assayPassFailuresJSON(failed),
	}
	if status == state.AssayStatusPartial {
		run.StatusText = assay.RenderStatusText(
			assay.RunStatusPartial, completedPasses, totalPasses, enginePassFailures(failed))
	}
	if t := parseAnyTime(startedAt); !t.IsZero() {
		run.StartedAt = t.UTC().Format(time.RFC3339Nano)
	}
	if finishedAt.Valid {
		if t := parseAnyTime(finishedAt.String); !t.IsZero() {
			run.FinishedAt = t.UTC().Format(time.RFC3339Nano)
		}
	}
	return run, nil
}

// findingStatus maps a finding's posted/resolved flags to a status label the
// frontend can badge: resolved (thread closed) → posted (live comment) → open
// (detected, not yet posted).
func findingStatus(posted, resolved bool) string {
	switch {
	case resolved:
		return "resolved"
	case posted:
		return "posted"
	default:
		return "open"
	}
}

// runStatus maps an assay_runs row to a coarse lifecycle label: running (not
// finished) → partial → error → skipped → complete.
//
// A finished run's persisted status wins where it records one: `partial` is a
// run that produced real findings from some passes while others never saw the
// head, and it carries a non-empty error string (the failed passes'), so the
// error branch below would otherwise swallow it. Rows written before coverage
// was recorded have an empty stored status and fall through to the original
// derivation unchanged.
func runStatus(finished bool, errMsg, skipped, stored string) string {
	switch {
	case !finished:
		return "running"
	case stored == state.AssayStatusPartial:
		return state.AssayStatusPartial
	case errMsg != "":
		return "error"
	case skipped != "":
		return "skipped"
	default:
		return "complete"
	}
}

// assayPassFailuresJSON converts persisted failed passes to the wire shape.
func assayPassFailuresJSON(failed []state.AssayPassFailure) []assayPassFailJSON {
	if len(failed) == 0 {
		return nil
	}
	out := make([]assayPassFailJSON, 0, len(failed))
	for _, f := range failed {
		out = append(out, assayPassFailJSON{Name: f.Name, Reason: f.Reason})
	}
	return out
}

// enginePassFailures converts persisted failed passes back into the engine's
// type so the status line is rendered by the same helper the daemon uses.
func enginePassFailures(failed []state.AssayPassFailure) []assay.PassFailure {
	if len(failed) == 0 {
		return nil
	}
	out := make([]assay.PassFailure, 0, len(failed))
	for _, f := range failed {
		out = append(out, assay.PassFailure{Name: f.Name, Reason: f.Reason})
	}
	return out
}

// parseAnyTime parses a timestamp using the layouts the state package
// produces (canonical fixed-width with nanos, then RFC3339Nano / RFC3339).
// Returns the zero time when none of them parse.
func parseAnyTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000000000Z07:00", s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
