package wicket

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

// anvilForRepo returns the anvil name whose WicketRepos list contains repo, or
// an empty string when no match is found. Used to populate the anvil field of
// LogEvent calls in lifecycle handlers that receive no anvil argument directly.
func (m *Monitor) anvilForRepo(repo string) string {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg == nil {
		return ""
	}
	for anvilName, ac := range cfg.Anvils {
		for _, r := range ac.WicketRepos {
			if r == repo {
				return anvilName
			}
		}
	}
	return ""
}

// HandlePRCreated is called by the daemon when a PR is created for a bead
// that may have been sourced from a Wicket GitHub issue. It looks up the
// bead ID in wicket_issues, posts a follow-up comment on the linked GitHub
// issue, and updates the wicket_issues state to "pr_created".
//
// This is a no-op when the bead ID is not found in wicket_issues.
func (m *Monitor) HandlePRCreated(ctx context.Context, beadID, prURL string, prNumber int) {
	if beadID == "" {
		return
	}
	wi, err := m.db.GetWicketIssueByBeadID(beadID)
	if err != nil {
		log.Printf("[wicket:lifecycle] GetWicketIssueByBeadID(%s): %v", beadID, err)
		return
	}
	if wi == nil {
		return // not a wicket issue
	}

	log.Printf("[wicket:lifecycle] PR #%d created for wicket bead %s (%s#%d)", prNumber, beadID, wi.Repo, wi.IssueNumber)

	comment, err := RenderPRCreated(PRCreatedData{PRURL: prURL, BeadID: beadID})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, wi.Repo, wi.IssueNumber, comment); cerr != nil {
			log.Printf("[wicket:lifecycle] comment on %s#%d: %v", wi.Repo, wi.IssueNumber, cerr)
		}
	}

	wi.PRNumber = prNumber
	wi.PRUrl = prURL
	wi.State = StatePRCreated
	if uerr := m.db.UpdateWicketIssue(*wi); uerr != nil {
		log.Printf("[wicket:lifecycle] update wicket issue for %s#%d: %v", wi.Repo, wi.IssueNumber, uerr)
	}

	_ = m.db.LogEvent(state.EventWicketPRLinked,
		fmt.Sprintf("PR #%d linked to %s#%d (bead %s)", prNumber, wi.Repo, wi.IssueNumber, beadID),
		beadID, m.anvilForRepo(wi.Repo))
}

// HandlePRMerged is called by the daemon when a PR is merged for a bead that
// may have been sourced from a Wicket GitHub issue. It posts a closure comment
// on the linked GitHub issue, closes the issue, and updates wicket_issues state
// to "merged".
//
// This is a no-op when the bead ID is not found in wicket_issues.
func (m *Monitor) HandlePRMerged(ctx context.Context, beadID, prURL, baseBranch string, prNumber int) {
	if beadID == "" {
		return
	}
	wi, err := m.db.GetWicketIssueByBeadID(beadID)
	if err != nil {
		log.Printf("[wicket:lifecycle] GetWicketIssueByBeadID(%s): %v", beadID, err)
		return
	}
	if wi == nil {
		return // not a wicket issue
	}

	log.Printf("[wicket:lifecycle] PR #%d merged for wicket bead %s (%s#%d) — closing issue", prNumber, beadID, wi.Repo, wi.IssueNumber)

	base := baseBranch
	if base == "" {
		base = "main"
	}
	comment, err := RenderPRMerged(PRMergedData{PRURL: prURL, BaseBranch: base})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, wi.Repo, wi.IssueNumber, comment); cerr != nil {
			log.Printf("[wicket:lifecycle] comment on %s#%d: %v", wi.Repo, wi.IssueNumber, cerr)
		}
	}

	if cerr := m.ghClient.CloseIssue(ctx, wi.Repo, wi.IssueNumber, "completed"); cerr != nil {
		log.Printf("[wicket:lifecycle] close issue %s#%d: %v", wi.Repo, wi.IssueNumber, cerr)
	}

	wi.PRNumber = prNumber
	wi.PRUrl = prURL
	wi.State = StateMerged
	if uerr := m.db.UpdateWicketIssue(*wi); uerr != nil {
		log.Printf("[wicket:lifecycle] update wicket issue for %s#%d: %v", wi.Repo, wi.IssueNumber, uerr)
	}

	_ = m.db.LogEvent(state.EventWicketIssueClosed,
		fmt.Sprintf("Issue %s#%d closed after PR #%d merged (bead %s)", wi.Repo, wi.IssueNumber, prNumber, beadID),
		beadID, m.anvilForRepo(wi.Repo))
}

// checkStaleIssues finds ask_clarify issues that have not received an author
// reply for staleDays days, posts a stale warning comment, and updates their
// state to "stale".
func (m *Monitor) checkStaleIssues(ctx context.Context, settings config.SettingsConfig) {
	staleDays := settings.WicketStaleDays
	if staleDays <= 0 {
		staleDays = 14
	}

	issues, err := m.db.ListWicketIssues(state.ListWicketIssuesOpts{State: StateAskClarify})
	if err != nil {
		log.Printf("[wicket:stale] list ask_clarify issues: %v", err)
		return
	}

	threshold := time.Now().UTC().AddDate(0, 0, -staleDays)

	for _, wi := range issues {
		if ctx.Err() != nil {
			return
		}
		// Use AuthorRepliedAt as the staleness marker when available — it is
		// only updated when the issue author comments, so non-author activity
		// (bot comments, bystanders) does not reset the staleness timer.
		// Fall back to UpdatedAt for issues that predate this field.
		activityAt := wi.UpdatedAt
		if wi.AuthorRepliedAt != nil {
			activityAt = *wi.AuthorRepliedAt
		}
		if activityAt.After(threshold) {
			continue
		}

		log.Printf("[wicket:stale] %s#%d has been idle for >%d days — marking stale", wi.Repo, wi.IssueNumber, staleDays)

		comment, err := RenderStaleWarning(StaleWarningData{})
		if err == nil {
			if cerr := m.ghClient.CommentOnIssue(ctx, wi.Repo, wi.IssueNumber, comment); cerr != nil {
				log.Printf("[wicket:stale] comment on %s#%d: %v", wi.Repo, wi.IssueNumber, cerr)
			}
		}

		wi.State = StateStale
		if uerr := m.db.UpdateWicketIssue(wi); uerr != nil {
			log.Printf("[wicket:stale] update state for %s#%d: %v", wi.Repo, wi.IssueNumber, uerr)
		}
	}
}

// checkStaleClosed finds stale issues that have received no reply for another
// 7 days and closes them automatically.
func (m *Monitor) checkStaleClosed(ctx context.Context) {
	issues, err := m.db.ListWicketIssues(state.ListWicketIssuesOpts{State: StateStale})
	if err != nil {
		log.Printf("[wicket:stale] list stale issues: %v", err)
		return
	}

	// Close stale issues that have been waiting another 7 days without a reply.
	threshold := time.Now().UTC().AddDate(0, 0, -7)

	for _, wi := range issues {
		if ctx.Err() != nil {
			return
		}
		activityAt := wi.UpdatedAt
		if wi.AuthorRepliedAt != nil {
			activityAt = *wi.AuthorRepliedAt
		}
		if activityAt.After(threshold) {
			continue
		}

		log.Printf("[wicket:stale] %s#%d stale with no reply — auto-closing", wi.Repo, wi.IssueNumber)

		if cerr := m.ghClient.CloseIssue(ctx, wi.Repo, wi.IssueNumber, "not planned"); cerr != nil {
			log.Printf("[wicket:stale] close issue %s#%d: %v", wi.Repo, wi.IssueNumber, cerr)
		}

		wi.State = StateClosed
		if uerr := m.db.UpdateWicketIssue(wi); uerr != nil {
			log.Printf("[wicket:stale] update state for %s#%d: %v", wi.Repo, wi.IssueNumber, uerr)
		}

		_ = m.db.LogEvent(state.EventWicketIssueStaleClose,
			fmt.Sprintf("Issue %s#%d auto-closed due to no reply after stale warning", wi.Repo, wi.IssueNumber),
			wi.BeadID, m.anvilForRepo(wi.Repo))
	}
}
