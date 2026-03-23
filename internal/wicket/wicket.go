// Package wicket polls GitHub issues, triages them with AI for trusted users,
// and creates beads or posts comments based on the AI triage decision.
//
// It runs as a background goroutine in the daemon, following the same
// goroutine+ticker pattern as depcheck.Scanner.
package wicket

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
)

// MonitorConfig holds the configuration for a single poll cycle.
type MonitorConfig struct {
	// Anvils maps anvil name → AnvilConfig for anvils with Wicket enabled.
	Anvils map[string]config.AnvilConfig
	// Settings holds the global Forge settings (for labels, batch size, etc.).
	Settings config.SettingsConfig
	// Provider is the AI provider to use for triage.
	Provider provider.Provider
}

// TriageFn is the signature for the AI triage function.
// Injected into Monitor so tests can mock it without spawning real AI sessions.
type TriageFn func(ctx context.Context, repo string, issue *Issue, contextText string) (*TriageDecision, error)

// Monitor polls GitHub issues across registered anvils and triages them with AI.
type Monitor struct {
	db       *state.DB
	ghc      GitHubClient
	triageFn TriageFn // nil = build from cfg.Provider at poll time
	mu       sync.RWMutex
	cfg      MonitorConfig

	cancel context.CancelFunc
}

// New creates a new Wicket Monitor.
// ghc may be nil; a production ghClient is created in that case.
// triageFn may be nil; a real AI triager is constructed from cfg.Provider.
func New(db *state.DB, cfg MonitorConfig, ghc GitHubClient) *Monitor {
	if ghc == nil {
		ghc = newGHClient(defaultGHTimeout)
	}
	return &Monitor{
		db:  db,
		ghc: ghc,
		cfg: cfg,
	}
}

// newWithTriageFn creates a Monitor with an injected triage function (for testing).
func newWithTriageFn(db *state.DB, cfg MonitorConfig, ghc GitHubClient, triageFn TriageFn) *Monitor {
	m := New(db, cfg, ghc)
	m.triageFn = triageFn
	return m
}

// UpdateConfig replaces the configuration used on the next poll cycle.
// Safe to call while Run is active.
func (m *Monitor) UpdateConfig(cfg MonitorConfig) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// Stop cancels the monitor's context, causing Run to return.
func (m *Monitor) Stop() {
	m.mu.RLock()
	cancel := m.cancel
	m.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

// Run starts the Wicket poll loop. It blocks until ctx is cancelled.
// Run performs an initial scan immediately, then ticks on the configured interval.
func (m *Monitor) Run(ctx context.Context) error {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	interval := cfg.Settings.WicketInterval
	if interval < 30*time.Second {
		interval = 5 * time.Minute
	}

	log.Printf("[wicket] starting monitor (interval: %s)", interval)
	_ = m.db.LogEvent(state.EventWicketScanDone, "Wicket monitor started", "", "")

	// Create a child context so Stop() can cancel without affecting the parent.
	runCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	defer cancel()

	// Initial scan.
	m.pollAll(runCtx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-runCtx.Done():
			log.Println("[wicket] shutting down monitor")
			return runCtx.Err()
		case <-ticker.C:
			m.pollAll(runCtx)
		}
	}
}

// pollAll polls all wicket-enabled anvils in the current configuration.
func (m *Monitor) pollAll(ctx context.Context) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	if len(cfg.Anvils) == 0 {
		return
	}

	log.Printf("[wicket] polling %d anvil(s)", len(cfg.Anvils))

	for name, anvil := range cfg.Anvils {
		if ctx.Err() != nil {
			return
		}
		m.pollAnvil(ctx, name, anvil, cfg)
	}

	_ = m.db.LogEvent(state.EventWicketScanDone,
		fmt.Sprintf("Wicket poll complete (%d anvils)", len(cfg.Anvils)), "", "")
}

// pollAnvil polls GitHub issues for a single anvil.
func (m *Monitor) pollAnvil(ctx context.Context, anvilName string, anvil config.AnvilConfig, cfg MonitorConfig) {
	// Determine which repos to poll.
	repos := anvil.WicketRepos
	if len(repos) == 0 {
		// Derive from git remote (best-effort; skip if unavailable).
		repo, err := repoFromGitRemote(anvil.Path)
		if err != nil {
			log.Printf("[wicket] %s: could not determine repo from git remote: %v", anvilName, err)
			_ = m.db.LogEvent(state.EventWicketError,
				fmt.Sprintf("could not determine repo for anvil %s: %v", anvilName, err), "", anvilName)
			return
		}
		repos = []string{repo}
	}

	batchSize := cfg.Settings.WicketBatchSize
	if batchSize <= 0 {
		batchSize = 25
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		m.pollRepo(ctx, anvilName, repo, anvil, cfg, batchSize)
	}
}

// pollRepo polls issues for a single repo and dispatches triage.
func (m *Monitor) pollRepo(ctx context.Context, anvilName, repo string, anvil config.AnvilConfig, cfg MonitorConfig, batchSize int) {
	issues, err := m.ghc.ListIssues(ctx, repo, batchSize)
	if err != nil {
		log.Printf("[wicket] %s: failed to list issues for %s: %v", anvilName, repo, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("failed to list issues for %s/%s: %v", anvilName, repo, err), "", anvilName)
		return
	}

	settings := cfg.Settings
	processedLabel := settings.WicketProcessedLabel
	if processedLabel == "" {
		processedLabel = "forge-triaged"
	}
	triggerLabel := settings.WicketTriggerLabel

	trustedSet := buildTrustedSet(anvil.WicketTrustedUsers)

	// Resolve the triage function: use injected mock or build from provider.
	m.mu.RLock()
	triageFn := m.triageFn
	m.mu.RUnlock()
	if triageFn == nil {
		triager := newTriager(cfg.Provider)
		triageFn = triager.Triage
	}

	processed := 0
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}

		// Skip pull requests.
		if issue.IsPR {
			continue
		}

		// Skip issues that are not open.
		if !strings.EqualFold(issue.State, "open") {
			continue
		}

		// Skip if already processed (has the processed label).
		if issue.HasLabel(processedLabel) {
			continue
		}

		// Skip if trigger label is set and issue doesn't have it.
		if triggerLabel != "" && !issue.HasLabel(triggerLabel) {
			continue
		}

		// Skip if issue label filter is set and issue doesn't match.
		if len(anvil.WicketIssueLabels) > 0 && !hasAnyLabel(&issue, anvil.WicketIssueLabels) {
			continue
		}

		// Skip if already tracked in wicket_issues.
		tracked, err := m.db.IsIssueTracked(repo, issue.Number)
		if err != nil {
			log.Printf("[wicket] %s: DB error checking issue %s#%d: %v", anvilName, repo, issue.Number, err)
			continue
		}
		if tracked {
			// Touch last_polled so we know it's still open.
			_ = m.db.TouchWicketIssue(repo, issue.Number)
			continue
		}

		// Record in wicket_issues before processing to prevent duplicate runs.
		isTrusted := isTrustedUser(issue.Author.Login, trustedSet)
		wi := &state.WicketIssue{
			Repo:        repo,
			IssueNumber: issue.Number,
			AnvilName:   anvilName,
			Author:      issue.Author.Login,
			IsTrusted:   isTrusted,
			Status:      "open",
		}
		if err := m.db.InsertWicketIssue(wi); err != nil {
			log.Printf("[wicket] %s: failed to insert wicket issue %s#%d: %v", anvilName, repo, issue.Number, err)
			continue
		}

		// Only triage issues from trusted users (Phase 1).
		if !isTrusted {
			log.Printf("[wicket] %s: skipping issue %s#%d from untrusted user %q",
				anvilName, repo, issue.Number, issue.Author.Login)
			continue
		}

		m.triageIssue(ctx, anvilName, repo, &issue, anvil, cfg, triageFn)
		processed++
	}

	if processed > 0 {
		log.Printf("[wicket] %s/%s: triaged %d new issue(s)", anvilName, repo, processed)
	}
}

// triageIssue runs AI triage on a single issue and dispatches the result.
func (m *Monitor) triageIssue(ctx context.Context, anvilName, repo string, issue *Issue, anvil config.AnvilConfig, cfg MonitorConfig, triageFn TriageFn) {
	log.Printf("[wicket] %s: triaging issue %s#%d: %q", anvilName, repo, issue.Number, issue.Title)

	_ = m.db.LogEvent(state.EventWicketIssueTriage,
		fmt.Sprintf("triaging %s#%d: %s", repo, issue.Number, issue.Title), "", anvilName)

	decision, err := triageFn(ctx, repo, issue, "")
	if err != nil {
		log.Printf("[wicket] %s: triage error for %s#%d: %v", anvilName, repo, issue.Number, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("triage error for %s#%d: %v", repo, issue.Number, err), "", anvilName)
		_ = m.db.UpdateWicketIssue(repo, issue.Number, string(ActionFlagHuman), "", "error", err.Error())
		return
	}

	settings := cfg.Settings
	processedLabel := settings.WicketProcessedLabel
	if processedLabel == "" {
		processedLabel = "forge-triaged"
	}

	switch decision.Action {
	case ActionCreateBead:
		m.handleCreateBead(ctx, anvilName, repo, issue, decision, anvil, settings)

	case ActionAskClarify:
		comment := renderClarificationNeeded(decision.Question)
		if err := m.ghc.CommentOnIssue(ctx, repo, issue.Number, comment); err != nil {
			log.Printf("[wicket] %s: failed to post clarification comment on %s#%d: %v",
				anvilName, repo, issue.Number, err)
		}
		// Add processed label so we don't re-triage.
		_ = m.ghc.AddLabels(ctx, repo, issue.Number, []string{processedLabel})
		_ = m.db.UpdateWicketIssue(repo, issue.Number, string(ActionAskClarify), "", "awaiting_clarification", decision.Reasoning)
		_ = m.db.LogEvent(state.EventWicketClarification,
			fmt.Sprintf("posted clarification request on %s#%d", repo, issue.Number), "", anvilName)

	case ActionFlagHuman:
		comment := renderFlaggedForHuman(decision.Reasoning)
		needsHumanLabel := settings.WicketNeedsHumanLabel
		if needsHumanLabel == "" {
			needsHumanLabel = "forge-needs-human"
		}
		if err := m.ghc.CommentOnIssue(ctx, repo, issue.Number, comment); err != nil {
			log.Printf("[wicket] %s: failed to post flag-human comment on %s#%d: %v",
				anvilName, repo, issue.Number, err)
		}
		_ = m.ghc.AddLabels(ctx, repo, issue.Number, []string{processedLabel, needsHumanLabel})
		_ = m.db.UpdateWicketIssue(repo, issue.Number, string(ActionFlagHuman), "", "needs_human", decision.Reasoning)
		_ = m.db.LogEvent(state.EventWicketFlaggedHuman,
			fmt.Sprintf("flagged %s#%d for human review", repo, issue.Number), "", anvilName)
	}
}

// handleCreateBead creates a bead from the triage decision, posts a comment, and labels the issue.
func (m *Monitor) handleCreateBead(ctx context.Context, anvilName, repo string, issue *Issue, decision *TriageDecision, anvil config.AnvilConfig, settings config.SettingsConfig) {
	issueURL := issue.URL
	if issueURL == "" {
		issueURL = fmt.Sprintf("https://github.com/%s/issues/%d", repo, issue.Number)
	}

	// Append the source URL to the description so the bead links back.
	description := decision.Description
	if description == "" {
		description = issue.Body
	}
	description = fmt.Sprintf("%s\n\nSource: %s", description, issueURL)

	beadID, err := CreateBead(ctx, anvil.Path, decision.Title, description, decision.IssueType, decision.Priority)
	if err != nil {
		log.Printf("[wicket] %s: failed to create bead for %s#%d: %v", anvilName, repo, issue.Number, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("failed to create bead for %s#%d: %v", repo, issue.Number, err), "", anvilName)
		_ = m.db.UpdateWicketIssue(repo, issue.Number, string(ActionCreateBead), "", "error", err.Error())
		return
	}

	// Post "bead created" comment on the issue.
	comment := renderBeadCreated(beadID, decision.Title)
	if err := m.ghc.CommentOnIssue(ctx, repo, issue.Number, comment); err != nil {
		log.Printf("[wicket] %s: failed to post bead-created comment on %s#%d: %v",
			anvilName, repo, issue.Number, err)
	}

	// Apply processed + bead-created labels.
	processedLabel := settings.WicketProcessedLabel
	if processedLabel == "" {
		processedLabel = "forge-triaged"
	}
	beadCreatedLabel := settings.WicketBeadCreatedLabel
	if beadCreatedLabel == "" {
		beadCreatedLabel = "forge-bead-created"
	}
	_ = m.ghc.AddLabels(ctx, repo, issue.Number, []string{processedLabel, beadCreatedLabel})

	_ = m.db.UpdateWicketIssue(repo, issue.Number, string(ActionCreateBead), beadID, "bead_created", decision.Reasoning)
	_ = m.db.LogEvent(state.EventWicketBeadCreated,
		fmt.Sprintf("created bead %s from %s#%d: %s", beadID, repo, issue.Number, decision.Title), "", anvilName)

	log.Printf("[wicket] %s: created bead %s from %s#%d", anvilName, beadID, repo, issue.Number)
}

// buildTrustedSet returns a set (map) of lowercase usernames.
func buildTrustedSet(users []string) map[string]struct{} {
	s := make(map[string]struct{}, len(users))
	for _, u := range users {
		s[strings.ToLower(u)] = struct{}{}
	}
	return s
}

// isTrustedUser reports whether the given login is in the trusted set.
func isTrustedUser(login string, trusted map[string]struct{}) bool {
	if len(trusted) == 0 {
		return false
	}
	_, ok := trusted[strings.ToLower(login)]
	return ok
}

// hasAnyLabel reports whether the issue has any of the given labels.
func hasAnyLabel(issue *Issue, labels []string) bool {
	for _, want := range labels {
		if issue.HasLabel(want) {
			return true
		}
	}
	return false
}

// repoFromGitRemote extracts "owner/repo" from git remote origin in the given path.
// It shells out to `git remote get-url origin` and parses the result.
func repoFromGitRemote(anvilPath string) (string, error) {
	return parseRepoFromPath(anvilPath)
}
