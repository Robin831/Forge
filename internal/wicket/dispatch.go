package wicket

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// bdUpdateRunner is the function used to execute `bd update`. Tests replace this
// to avoid spawning a real subprocess.
var bdUpdateRunner func(ctx context.Context, beadID string, args []string) error = defaultBDUpdateRunner

func defaultBDUpdateRunner(ctx context.Context, beadID string, args []string) error {
	cmdArgs := append([]string{"update", beadID}, args...)
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", cmdArgs...))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		se := strings.TrimSpace(stderr.String())
		if se != "" {
			return fmt.Errorf("bd update %s: %v: %s", beadID, err, se)
		}
		return fmt.Errorf("bd update %s: %w", beadID, err)
	}
	return nil
}

// checkDispatch scans wicket issues in "bead_created" state within the given
// repos for dispatch signals: a rocket emoji reaction or a comment from the
// issue author containing "dispatch" (case-insensitive). When found, the bead
// is tagged for auto-dispatch and the issue is updated accordingly.
//
// This step is skipped for anvils with wicket_auto_dispatch=true since those
// beads are dispatched immediately at creation time.
func (m *Monitor) checkDispatch(ctx context.Context, anvil string, anvilCfg config.AnvilConfig, _ config.SettingsConfig) {
	if anvilCfg.WicketAutoDispatch {
		return // auto-dispatch is on; no confirmation step needed
	}

	repos, err := m.resolveRepos(ctx, anvilCfg)
	if err != nil {
		return
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		m.checkDispatchForRepo(ctx, anvil, repo)
	}
}

// checkDispatchForRepo checks all bead_created issues in the given repo for
// dispatch signals.
func (m *Monitor) checkDispatchForRepo(ctx context.Context, anvil, repo string) {
	issues, err := m.db.ListWicketIssues(state.ListWicketIssuesOpts{
		Repo:  repo,
		State: StateBeadCreated,
	})
	if err != nil {
		log.Printf("[wicket:dispatch] %s: list bead_created issues for %s: %v", anvil, repo, err)
		return
	}

	for _, wi := range issues {
		if ctx.Err() != nil {
			return
		}
		if wi.BeadID == "" {
			continue
		}
		m.checkDispatchForIssue(ctx, anvil, wi)
	}
}

// checkDispatchForIssue checks a single issue for dispatch signals and
// handles "label <tag>" comments.
func (m *Monitor) checkDispatchForIssue(ctx context.Context, anvil string, wi state.WicketIssue) {
	// Check reactions first (cheaper than fetching all comments).
	reactions, err := m.ghClient.ListReactions(ctx, wi.Repo, wi.IssueNumber)
	if err != nil {
		log.Printf("[wicket:dispatch] %s: list reactions %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, err)
	} else {
		for _, r := range reactions {
			if r.Content == "rocket" {
				log.Printf("[wicket:dispatch] %s: rocket reaction detected on %s#%d — dispatching %s", anvil, wi.Repo, wi.IssueNumber, wi.BeadID)
				m.dispatchBead(ctx, anvil, wi)
				return
			}
		}
	}

	// Check comments for "dispatch" keyword or "label <tag>" commands.
	// Only comments from the issue author are honored to prevent bystanders or
	// bots from accidentally triggering dispatch. We also track the comment count
	// so each comment is processed only once, preventing repeated re-tagging.
	comments, err := m.ghClient.ListComments(ctx, wi.Repo, wi.IssueNumber)
	if err != nil {
		log.Printf("[wicket:dispatch] %s: list comments %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, err)
		return
	}

	currentCount := len(comments)
	previousCount := wi.CommentCount

	// Persist the updated comment count so new comments are not re-processed
	// on the next scan, regardless of whether they trigger an action.
	if currentCount != previousCount {
		wi.CommentCount = currentCount
		if uerr := m.db.UpdateWicketIssue(wi); uerr != nil {
			log.Printf("[wicket:dispatch] %s: update comment_count for %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, uerr)
		}
	}

	// Only act on new comments from the issue author.
	for i := previousCount; i < currentCount; i++ {
		c := comments[i]
		if !strings.EqualFold(c.Author, wi.Author) {
			continue // only the issue author can trigger dispatch or label commands
		}
		body := strings.TrimSpace(c.Body)
		lower := strings.ToLower(body)

		if strings.Contains(lower, "dispatch") {
			log.Printf("[wicket:dispatch] %s: dispatch comment from %s on %s#%d — dispatching %s", anvil, c.Author, wi.Repo, wi.IssueNumber, wi.BeadID)
			m.dispatchBead(ctx, anvil, wi)
			return
		}

		// Handle "label <tag>" comments to tag the bead.
		if tag, ok := parseLabelComment(body); ok {
			log.Printf("[wicket:dispatch] %s: label comment %q on %s#%d — tagging %s", anvil, tag, wi.Repo, wi.IssueNumber, wi.BeadID)
			m.handleLabelComment(ctx, anvil, wi, tag)
			return
		}
	}
}

// parseLabelComment returns (tag, true) when body starts with "label " followed
// by a non-empty tag name. Matching is case-insensitive.
func parseLabelComment(body string) (string, bool) {
	lower := strings.ToLower(body)
	const prefix = "label "
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}
	tag := strings.TrimSpace(body[len(prefix):])
	if tag == "" {
		return "", false
	}
	return tag, true
}

// dispatchBead tags the bead for auto-dispatch, posts a confirmation comment,
// and updates the wicket_issues state to "dispatched".
func (m *Monitor) dispatchBead(ctx context.Context, anvil string, wi state.WicketIssue) {
	if err := bdUpdateRunner(ctx, wi.BeadID, []string{"--tag", "auto-dispatch"}); err != nil {
		log.Printf("[wicket:dispatch] %s: bd update tag for %s: %v", anvil, wi.BeadID, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("[%s] Failed to tag bead %s for dispatch: %v", anvil, wi.BeadID, err), wi.BeadID, anvil)
		return
	}

	comment, err := RenderDispatchConfirmed(DispatchConfirmedData{BeadID: wi.BeadID})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, wi.Repo, wi.IssueNumber, comment); cerr != nil {
			log.Printf("[wicket:dispatch] %s: comment on %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, cerr)
		}
	}

	wi.State = StateDispatched
	if uerr := m.db.UpdateWicketIssue(wi); uerr != nil {
		log.Printf("[wicket:dispatch] %s: update state for %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, uerr)
	}

	_ = m.db.LogEvent(state.EventWicketDispatchConfirm,
		fmt.Sprintf("[%s] Bead %s dispatched for %s#%d", anvil, wi.BeadID, wi.Repo, wi.IssueNumber),
		wi.BeadID, anvil)

	log.Printf("[wicket:dispatch] %s: bead %s dispatched for %s#%d", anvil, wi.BeadID, wi.Repo, wi.IssueNumber)
}

// handleLabelComment applies a tag to the bead and posts a follow-up comment
// re-asking about dispatch.
func (m *Monitor) handleLabelComment(ctx context.Context, anvil string, wi state.WicketIssue, tag string) {
	if err := bdUpdateRunner(ctx, wi.BeadID, []string{"--tag", tag}); err != nil {
		log.Printf("[wicket:dispatch] %s: bd update tag %q for %s: %v", anvil, tag, wi.BeadID, err)
		return
	}

	comment, err := RenderLabelApplied(LabelAppliedData{Tag: tag, BeadID: wi.BeadID})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, wi.Repo, wi.IssueNumber, comment); cerr != nil {
			log.Printf("[wicket:dispatch] %s: comment on %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, cerr)
		}
	}

	log.Printf("[wicket:dispatch] %s: tag %q applied to %s for %s#%d", anvil, tag, wi.BeadID, wi.Repo, wi.IssueNumber)
}

// checkClarificationReTriage checks issues in "ask_clarify" state for new
// author replies and re-triages them when found.
func (m *Monitor) checkClarificationReTriage(ctx context.Context, anvil string, anvilCfg config.AnvilConfig, settings config.SettingsConfig) {
	repos, err := m.resolveRepos(ctx, anvilCfg)
	if err != nil {
		return
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		m.checkClarificationForRepo(ctx, anvil, repo, anvilCfg, settings)
	}
}

// checkClarificationForRepo checks all ask_clarify and stale issues in the
// given repo for new author replies. Stale issues are included so that an
// author reply before the auto-close window can return the issue to the queue.
func (m *Monitor) checkClarificationForRepo(ctx context.Context, anvil, repo string, anvilCfg config.AnvilConfig, settings config.SettingsConfig) {
	for _, st := range []string{StateAskClarify, StateStale} {
		issues, err := m.db.ListWicketIssues(state.ListWicketIssuesOpts{
			Repo:  repo,
			State: st,
		})
		if err != nil {
			log.Printf("[wicket:retriage] %s: list %s issues for %s: %v", anvil, st, repo, err)
			continue
		}

		for _, wi := range issues {
			if ctx.Err() != nil {
				return
			}
			m.checkClarificationForIssue(ctx, anvil, wi, anvilCfg, settings)
		}
	}
}

// checkClarificationForIssue checks if the issue author has replied since the
// last check, and if so re-triages the issue with the full conversation context.
func (m *Monitor) checkClarificationForIssue(ctx context.Context, anvil string, wi state.WicketIssue, anvilCfg config.AnvilConfig, settings config.SettingsConfig) {
	comments, err := m.ghClient.ListComments(ctx, wi.Repo, wi.IssueNumber)
	if err != nil {
		log.Printf("[wicket:retriage] %s: list comments %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, err)
		return
	}

	currentCount := len(comments)
	if currentCount <= wi.CommentCount {
		// No new comments since last check.
		return
	}

	// Check if the author is among the new commenters.
	authorReplied := false
	for i := wi.CommentCount; i < currentCount; i++ {
		if strings.EqualFold(comments[i].Author, wi.Author) {
			authorReplied = true
			break
		}
	}

	// Update the stored comment count and, when the author replied, record the
	// timestamp so stale detection uses last author activity rather than
	// UpdatedAt (which is bumped by any UpdateWicketIssue call).
	wi.CommentCount = currentCount
	if authorReplied {
		now := time.Now().UTC()
		wi.AuthorRepliedAt = &now
	}
	if err := m.db.UpdateWicketIssue(wi); err != nil {
		log.Printf("[wicket:retriage] %s: update comment_count for %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, err)
	}

	if !authorReplied {
		return
	}

	log.Printf("[wicket:retriage] %s: author replied on %s#%d — re-triaging", anvil, wi.Repo, wi.IssueNumber)

	// Reconstruct the Issue struct for re-triage.
	issue := Issue{
		Number:    wi.IssueNumber,
		Repo:      wi.Repo,
		Title:     wi.Title,
		Body:      wi.Body,
		Author:    wi.Author,
		CreatedAt: wi.CreatedAt,
	}

	triageFn := m.triageFunc
	if triageFn == nil {
		triageFn = RunTriageWithComments
	}
	decision := triageFn(ctx, issue, comments, TriageConfig{
		Providers:     buildProviders(settings),
		ExtraPrompt:   anvilCfg.WicketTriagePrompt,
		AnvilPath:     anvilCfg.Path,
		AllAnvilPaths: m.allAnvilPaths(),
	})

	_ = m.db.LogEvent(state.EventWicketIssueTriage,
		fmt.Sprintf("[%s] %s#%d re-triage=%s reason=%s", anvil, wi.Repo, wi.IssueNumber, decision.Action, decision.Reason),
		wi.BeadID, anvil)

	switch decision.Action {
	case ActionCreateBead:
		// Update to pending so handleCreateBead can persist correctly.
		wi.State = "pending"
		wi.TriageAction = string(decision.Action)
		wi.TriageReason = decision.Reason
		_ = m.db.UpdateWicketIssue(wi)
		m.handleCreateBead(ctx, anvil, issue, decision, settings)

	case ActionAskClarify:
		// Post updated clarification request.
		comment, err := RenderClarificationNeeded(ClarificationNeededData{Reason: decision.Reason})
		if err == nil {
			if cerr := m.ghClient.CommentOnIssue(ctx, wi.Repo, wi.IssueNumber, comment); cerr != nil {
				log.Printf("[wicket:retriage] %s: comment on %s#%d: %v", anvil, wi.Repo, wi.IssueNumber, cerr)
			}
		}
		wi.TriageReason = decision.Reason
		_ = m.db.UpdateWicketIssue(wi)

	case ActionDuplicate:
		m.handleDuplicate(ctx, anvil, issue, decision, settings)

	case ActionAlreadyFixed:
		m.handleAlreadyFixed(ctx, anvil, issue, decision, settings)

	case ActionOutOfScope:
		m.handleOutOfScope(ctx, anvil, issue, decision, settings)

	default:
		// Map triage actions to the corresponding lifecycle states to avoid
		// storing raw action strings that other code won't recognize (e.g.
		// "flag_human" must be stored as "needs_human").
		switch decision.Action {
		case ActionFlagHuman:
			wi.State = StateNeedsHuman
		case ActionReject:
			wi.State = StateRejected
		default:
			wi.State = string(decision.Action)
		}
		wi.TriageReason = decision.Reason
		_ = m.db.UpdateWicketIssue(wi)
	}
}
