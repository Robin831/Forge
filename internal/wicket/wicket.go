package wicket

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
)

// Monitor periodically polls GitHub repositories for new issues, triages them
// using an AI provider, and dispatches the appropriate action: create a bead,
// ask for clarification, or flag for human review.
type Monitor struct {
	ghClient GitHubClient
	db       *state.DB
	cfg      *config.Config
	mu       sync.RWMutex
}

// New creates a Wicket issue triage monitor with the default GitHub CLI client.
func New(cfg *config.Config, db *state.DB) *Monitor {
	return &Monitor{
		ghClient: NewGitHubClient(),
		db:       db,
		cfg:      cfg,
	}
}

// UpdateConfig replaces the monitor configuration. Safe to call while Run is
// active; the new configuration takes effect on the next scan cycle. Note that
// a changed WicketInterval takes effect after the current ticker fires (i.e.
// on the cycle after the update), not immediately. Interval changes require no
// restart.
func (m *Monitor) UpdateConfig(cfg *config.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// Stop is a convenience no-op. Cancelling the context passed to Run is the
// primary shutdown mechanism; this method exists for API symmetry with other
// monitors.
func (m *Monitor) Stop() {}

// Run starts the periodic issue triage loop. Blocks until ctx is canceled.
// It follows the same goroutine+ticker pattern as depcheck.Scanner.Run.
func (m *Monitor) Run(ctx context.Context) error {
	m.mu.RLock()
	interval := m.cfg.Settings.WicketInterval
	m.mu.RUnlock()

	if interval <= 0 {
		interval = 15 * time.Minute
	}

	log.Printf("[wicket] Starting issue triage monitor (interval: %s)", interval)
	_ = m.db.LogEvent(state.EventWicketStarted, "Wicket monitor started", "", "")

	// Perform an initial scan immediately so the first triage cycle does not
	// wait a full interval.
	m.scanAll(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[wicket] Shutting down issue triage monitor")
			return ctx.Err()
		case <-ticker.C:
			m.scanAll(ctx)

			// Re-read interval after each scan; reset the ticker if it changed
			// so that runtime config updates take effect without a restart.
			m.mu.RLock()
			newInterval := m.cfg.Settings.WicketInterval
			m.mu.RUnlock()
			if newInterval <= 0 {
				newInterval = 15 * time.Minute
			}
			if newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
				log.Printf("[wicket] Interval updated to %s", interval)
			}
		}
	}
}

// scanAll iterates all enabled anvils and scans their repositories for new
// issues that need triage.
func (m *Monitor) scanAll(ctx context.Context) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	log.Printf("[wicket] Scanning %d anvils for new issues", len(cfg.Anvils))

	for name, anvil := range cfg.Anvils {
		if ctx.Err() != nil {
			return
		}
		if !isWicketEnabled(anvil, cfg.Settings.WicketEnabled) {
			continue
		}
		m.scanAnvil(ctx, name, anvil, cfg.Settings)
	}

	_ = m.db.LogEvent(state.EventWicketScanDone, "Wicket scan cycle complete", "", "")
}

// isWicketEnabled returns true if Wicket scanning is enabled for the given
// anvil. Per-anvil WicketEnabled overrides the global setting when non-nil.
func isWicketEnabled(anvil config.AnvilConfig, globalEnabled bool) bool {
	if anvil.WicketEnabled != nil {
		return *anvil.WicketEnabled
	}
	return globalEnabled
}

// scanAnvil resolves the repository list for the anvil and scans each one.
func (m *Monitor) scanAnvil(ctx context.Context, name string, anvil config.AnvilConfig, settings config.SettingsConfig) {
	repos := anvil.WicketRepos
	if len(repos) == 0 {
		repo, err := deriveRepo(ctx, anvil.Path)
		if err != nil {
			log.Printf("[wicket] %s: cannot determine repository: %v", name, err)
			_ = m.db.LogEvent(state.EventWicketError,
				fmt.Sprintf("Cannot determine repository for anvil %s: %v", name, err), "", name)
			return
		}
		repos = []string{repo}
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		m.scanRepo(ctx, name, repo, anvil, settings)
	}
}

// scanRepo fetches open issues for a single repository and triages each new one.
// If WicketTriggerLabel is configured, issues carrying that label are also
// fetched (independently of WicketIssueLabels) and merged in so they are never
// silently dropped.
func (m *Monitor) scanRepo(ctx context.Context, anvil, repo string, anvilCfg config.AnvilConfig, settings config.SettingsConfig) {
	issues, err := m.ghClient.ListIssues(ctx, repo, anvilCfg.WicketIssueLabels)
	if err != nil {
		log.Printf("[wicket] %s: list issues for %s: %v", anvil, repo, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("Failed to list issues for %s: %v", repo, err), "", anvil)
		return
	}

	// Union in issues that carry the trigger label so they bypass the
	// WicketIssueLabels filter.
	if settings.WicketTriggerLabel != "" {
		triggerIssues, terr := m.ghClient.ListIssues(ctx, repo, []string{settings.WicketTriggerLabel})
		if terr != nil {
			log.Printf("[wicket] %s: list trigger-label issues for %s: %v", anvil, repo, terr)
			_ = m.db.LogEvent(state.EventWicketError,
				fmt.Sprintf("Failed to list trigger-label issues for %s: %v", repo, terr), "", anvil)
		} else {
			issues = mergeIssues(issues, triggerIssues)
		}
	}

	processed := 0
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		if settings.WicketBatchSize > 0 && processed >= settings.WicketBatchSize {
			log.Printf("[wicket] %s: reached batch size %d for %s", anvil, settings.WicketBatchSize, repo)
			break
		}
		if m.shouldSkip(issue, settings) {
			continue
		}
		m.triageIssue(ctx, anvil, issue, anvilCfg, settings)
		processed++
	}
}

// shouldSkip returns true when the issue should not be triaged. An issue is
// skipped when it already carries a Wicket processing label (processed,
// bead-created, or needs-human), or has already reached a terminal state in
// state.db. Issues in non-terminal states (e.g. "pending" from a prior failed
// run) are not skipped so they can be retried.
func (m *Monitor) shouldSkip(issue Issue, settings config.SettingsConfig) bool {
	// Skip issues that were already handled in a previous Wicket cycle.
	for _, label := range issue.Labels {
		switch label {
		case settings.WicketProcessedLabel,
			settings.WicketBeadCreatedLabel,
			settings.WicketNeedsHumanLabel:
			return true
		}
	}

	// Skip issues that have already been processed to a terminal state in
	// state.db. Issues still in "pending" (i.e. a prior attempt failed and was
	// rolled back) are not skipped so they are retried on the next scan.
	wi, err := m.db.GetWicketIssue(issue.Repo, issue.Number)
	if err != nil {
		log.Printf("[wicket] DB check %s#%d: %v — skipping to avoid double-processing", issue.Repo, issue.Number, err)
		return true
	}
	if wi == nil {
		return false
	}
	switch wi.State {
	case "bead_created", "ask_clarify", "needs_human":
		return true
	}
	return false
}

// isTrustedUser returns true if the given GitHub login appears in the trusted
// users list for the anvil. Comparison is case-insensitive.
func isTrustedUser(author string, trustedUsers []string) bool {
	for _, u := range trustedUsers {
		if strings.EqualFold(u, author) {
			return true
		}
	}
	return false
}

// triageIssue runs the full triage → dispatch pipeline for a single issue.
func (m *Monitor) triageIssue(ctx context.Context, anvil string, issue Issue, anvilCfg config.AnvilConfig, settings config.SettingsConfig) {
	log.Printf("[wicket] %s: triaging %s#%d %q", anvil, issue.Repo, issue.Number, issue.Title)

	// Insert a pending row before triaging so that concurrent scan cycles or
	// daemon restarts cannot double-process the same issue.
	pending := state.WicketIssue{
		Repo:        issue.Repo,
		IssueNumber: issue.Number,
		Title:       issue.Title,
		Body:        issue.Body,
		Author:      issue.Author,
		State:       "pending",
	}
	if err := m.db.InsertWicketIssue(pending); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			// Another scan cycle already claimed this issue; skip silently.
			log.Printf("[wicket] %s: skip %s#%d — already claimed by another cycle", anvil, issue.Repo, issue.Number)
		} else {
			// Unexpected DB error — log and emit an event so operators can see it.
			log.Printf("[wicket] %s: insert wicket issue %s#%d: %v", anvil, issue.Repo, issue.Number, err)
			_ = m.db.LogEvent(state.EventWicketError,
				fmt.Sprintf("[%s] Failed to claim %s#%d for triage: %v", anvil, issue.Repo, issue.Number, err), "", anvil)
		}
		return
	}

	var decision TriageDecision

	if isTrustedUser(issue.Author, anvilCfg.WicketTrustedUsers) {
		// Trusted authors bypass AI triage; their issues are always queued for
		// automated implementation using the issue title and body as-is.
		log.Printf("[wicket] %s: %s#%d author %q is trusted — skipping AI triage", anvil, issue.Repo, issue.Number, issue.Author)
		decision = TriageDecision{
			Action:          ActionCreateBead,
			Reason:          "issue author is a trusted user",
			BeadTitle:       issue.Title,
			BeadDescription: issue.Body,
		}
	} else {
		pvs := buildProviders(settings)
		decision = RunTriage(ctx, issue, TriageConfig{
			Providers:   pvs,
			ExtraPrompt: anvilCfg.WicketTriagePrompt,
		})
	}

	_ = m.db.LogEvent(state.EventWicketIssueTriage,
		fmt.Sprintf("[%s] %s#%d triage=%s reason=%s", anvil, issue.Repo, issue.Number, decision.Action, decision.Reason),
		"", anvil)

	m.dispatchDecision(ctx, anvil, issue, decision, settings)
}

// dispatchDecision executes the action chosen by the triage step.
func (m *Monitor) dispatchDecision(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	switch decision.Action {
	case ActionCreateBead:
		m.handleCreateBead(ctx, anvil, issue, decision, settings)
	case ActionAskClarify:
		m.handleAskClarify(ctx, anvil, issue, decision, settings)
	case ActionFlagHuman:
		m.handleFlagHuman(ctx, anvil, issue, decision, settings)
	default:
		log.Printf("[wicket] %s: unknown triage action %q for %s#%d", anvil, decision.Action, issue.Repo, issue.Number)
	}
}

// handleCreateBead creates a bead, posts a confirmation comment, and labels
// the issue as processed+bead-created.
func (m *Monitor) handleCreateBead(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	// Guard against an empty description (e.g. trusted-user path with no body).
	if decision.BeadDescription == "" {
		decision.BeadDescription = issue.Title
	}

	beadID, err := CreateBead(ctx, m.db, decision, issue, 2)
	if err != nil {
		log.Printf("[wicket] %s: create bead for %s#%d: %v", anvil, issue.Repo, issue.Number, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("[%s] Failed to create bead for %s#%d: %v", anvil, issue.Repo, issue.Number, err), "", anvil)
		// Roll back the pending claim so the issue is retried on the next scan.
		if derr := m.db.DeleteWicketIssue(issue.Repo, issue.Number); derr != nil {
			log.Printf("[wicket] %s: delete pending claim for %s#%d: %v", anvil, issue.Repo, issue.Number, derr)
		}
		return
	}

	log.Printf("[wicket] %s: created bead %s for %s#%d", anvil, beadID, issue.Repo, issue.Number)
	_ = m.db.LogEvent(state.EventWicketBeadCreated,
		fmt.Sprintf("[%s] Bead %s created for %s#%d: %s", anvil, beadID, issue.Repo, issue.Number, issue.Title),
		"", anvil)

	comment, err := RenderBeadCreated(BeadCreatedData{BeadID: beadID, Reason: decision.Reason})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, issue.Repo, issue.Number, comment); cerr != nil {
			log.Printf("[wicket] %s: comment on %s#%d: %v", anvil, issue.Repo, issue.Number, cerr)
		}
	}

	labels := []string{settings.WicketProcessedLabel, settings.WicketBeadCreatedLabel}
	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, labels); lerr != nil {
		log.Printf("[wicket] %s: add labels on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}
}

// handleAskClarify posts a clarification comment, labels the issue as
// processed, and records the outcome in state.db.
func (m *Monitor) handleAskClarify(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	log.Printf("[wicket] %s: %s#%d needs clarification: %s", anvil, issue.Repo, issue.Number, decision.Reason)
	_ = m.db.LogEvent(state.EventWicketClarification,
		fmt.Sprintf("[%s] %s#%d needs clarification: %s", anvil, issue.Repo, issue.Number, decision.Reason),
		"", anvil)

	comment, err := RenderClarificationNeeded(ClarificationNeededData{Reason: decision.Reason})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, issue.Repo, issue.Number, comment); cerr != nil {
			log.Printf("[wicket] %s: comment on %s#%d: %v", anvil, issue.Repo, issue.Number, cerr)
		}
	}

	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, []string{settings.WicketProcessedLabel}); lerr != nil {
		log.Printf("[wicket] %s: add label on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}

	m.persistOutcome(issue, "ask_clarify", decision)
}

// handleFlagHuman applies the needs-human label, posts a comment, and records
// the outcome in state.db.
func (m *Monitor) handleFlagHuman(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	log.Printf("[wicket] %s: %s#%d flagged for human review: %s", anvil, issue.Repo, issue.Number, decision.Reason)
	_ = m.db.LogEvent(state.EventWicketFlaggedHuman,
		fmt.Sprintf("[%s] %s#%d flagged for human review: %s", anvil, issue.Repo, issue.Number, decision.Reason),
		"", anvil)

	comment, err := RenderFlaggedForHuman(FlaggedForHumanData{Reason: decision.Reason})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, issue.Repo, issue.Number, comment); cerr != nil {
			log.Printf("[wicket] %s: comment on %s#%d: %v", anvil, issue.Repo, issue.Number, cerr)
		}
	}

	labels := []string{settings.WicketProcessedLabel, settings.WicketNeedsHumanLabel}
	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, labels); lerr != nil {
		log.Printf("[wicket] %s: add labels on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}

	m.persistOutcome(issue, "needs_human", decision)
}

// persistOutcome updates the wicket_issues row with the final triage state.
// CreateBead handles its own persistence; this is used only for ask_clarify
// and flag_human outcomes.
func (m *Monitor) persistOutcome(issue Issue, issueState string, decision TriageDecision) {
	now := time.Now().UTC()
	wi := state.WicketIssue{
		Repo:         issue.Repo,
		IssueNumber:  issue.Number,
		Title:        issue.Title,
		Body:         issue.Body,
		Author:       issue.Author,
		State:        issueState,
		TriageAction: string(decision.Action),
		TriageReason: decision.Reason,
		ProcessedAt:  &now,
	}
	if err := m.db.UpdateWicketIssue(wi); err != nil {
		log.Printf("[wicket] persist outcome for %s#%d: %v", issue.Repo, issue.Number, err)
	}
}

// mergeIssues returns the union of two issue slices deduplicated by issue
// number. Elements from b that are not already in a are appended in order.
func mergeIssues(a, b []Issue) []Issue {
	seen := make(map[int]bool, len(a))
	for _, i := range a {
		seen[i.Number] = true
	}
	result := append([]Issue{}, a...)
	for _, i := range b {
		if !seen[i.Number] {
			result = append(result, i)
			seen[i.Number] = true
		}
	}
	return result
}

// buildProviders returns the AI provider chain for triage based on settings.
// If WicketProvider is set it is used exclusively; otherwise the global
// Providers chain is used; if both are empty, provider.Defaults() is returned.
func buildProviders(settings config.SettingsConfig) []provider.Provider {
	if settings.WicketProvider != "" {
		return provider.FromConfig([]string{settings.WicketProvider})
	}
	if len(settings.Providers) > 0 {
		return provider.FromConfig(settings.Providers)
	}
	return provider.Defaults()
}

// reGitHubSSH matches git@github.com:owner/repo.git URLs.
var reGitHubSSH = regexp.MustCompile(`(?i)git@github\.com[:/]([^/]+/[^/]+?)(?:\.git)?$`)

// reGitHubHTTPS matches https://github.com/owner/repo URLs.
var reGitHubHTTPS = regexp.MustCompile(`(?i)https?://github\.com/([^/]+/[^/]+?)(?:\.git)?$`)

// deriveRepo returns the "owner/repo" string for the given local git directory
// by inspecting its origin remote URL. Returns an error when the directory is
// not a git repository or the remote URL cannot be parsed as a GitHub URL.
// A 10-second timeout is applied so a slow or hung git process cannot block
// the scan loop indefinitely.
func deriveRepo(ctx context.Context, dir string) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(tctx, "git", "remote", "get-url", "origin"))
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	raw := strings.TrimSpace(stdout.String())
	if m := reGitHubSSH.FindStringSubmatch(raw); m != nil {
		return m[1], nil
	}
	if m := reGitHubHTTPS.FindStringSubmatch(raw); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse GitHub owner/repo from remote URL %q", raw)
}
