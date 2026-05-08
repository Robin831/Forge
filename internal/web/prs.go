package web

import (
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"

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
// All three are read from state.db. The daemon's reconcileOpenPRs goroutine
// keeps the table in sync with `gh pr list` so this read-side handler stays
// fast (single SQL roundtrip) without shelling out per request. Manual
// refresh is handled by the existing reconcile_prs IPC command.
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

	merged, err := recentlyMergedPRs(s.db, recentlyMergedWindow)
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
		CIFixCount:      pr.CIFixCount,
		ReviewFixCount:  pr.ReviewFixCount,
		RebaseCount:     pr.RebaseCount,
		BeadID:          pr.BeadID,
	}
	if !pr.CreatedAt.IsZero() {
		item.CreatedAt = pr.CreatedAt.Format(time.RFC3339)
	}
	if pr.LastChecked != nil {
		item.UpdatedAt = pr.LastChecked.Format(time.RFC3339)
		if pr.Status == state.PRMerged {
			item.MergedAt = pr.LastChecked.Format(time.RFC3339)
		}
	}
	return item
}

// recentlyMergedPRs returns merged PRs (forge + external) whose last_checked
// timestamp falls within the given window. The state package only exposes a
// helper that excludes ext-* IDs, so we query directly here to give the /prs
// tab a complete view. Results are sorted newest-first.
func recentlyMergedPRs(db *state.DB, window time.Duration) ([]prItemJSON, error) {
	conn := db.Conn()
	if conn == nil {
		return []prItemJSON{}, nil
	}
	// Match the canonical state.dbTimeLayout (fixed-width nanos with offset)
	// so lexicographic comparison against last_checked stays correct.
	cutoff := time.Now().Add(-window).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	rows, err := conn.Query(`SELECT id, number, anvil, bead_id, branch, COALESCE(base_branch,''),
		status, created_at, last_checked, ci_fix_count, review_fix_count, rebase_count,
		ci_passing, is_conflicting, has_approval, COALESCE(title,''), bellows_managed
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
		)
		if err := rows.Scan(&id, &number, &anvil, &beadID, &branch, &base, &status, &createdAt,
			&lastChecked, &ciFix, &reviewFix, &rebase, &ciPassing, &conflicting,
			&approved, &title, &bellowsManaged); err != nil {
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
			CIFixCount:      ciFix,
			ReviewFixCount:  reviewFix,
			RebaseCount:     rebase,
			BeadID:          beadID,
			CreatedAt:       createdAt,
		}
		if lastChecked.Valid {
			item.UpdatedAt = lastChecked.String
			item.MergedAt = lastChecked.String
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
