package wicket

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Monitor periodically polls GitHub repositories for new issues, triages them
// using an AI provider, and dispatches the appropriate action: create a bead,
// ask for clarification, or flag for human review.
type Monitor struct {
	ghClient GitHubClient
	db       *state.DB
	cfg      *config.Config
	mu       sync.RWMutex
	rl       *rateLimiter
	resolver *RepoResolver
	// repoAnvilMap caches "owner/repo" → anvil name so downstream code
	// (dispatch, clarification) can look up which anvil owns a given
	// repository without always re-resolving the git remote.
	repoAnvilMap map[string]string
	// anvilPrimaryRepos caches anvil name → primary "owner/repo" derived
	// from the anvil's own git remote (ignoring WicketRepos overrides).
	// Used to detect when an issue originates from an external (monitored)
	// repo so the triage prompt can include anvil-domain context.
	anvilPrimaryRepos map[string]string
	// triageFunc overrides RunTriageWithComments. Nil means use the default.
	// Tests set this to avoid spawning real AI subprocesses.
	triageFunc func(ctx context.Context, issue Issue, comments []Comment, cfg TriageConfig) TriageDecision
	// triggerCh is used to request an immediate scan outside the normal interval.
	triggerCh chan struct{}
}

// New creates a Wicket issue triage monitor with the default GitHub CLI client.
func New(cfg *config.Config, db *state.DB) *Monitor {
	return &Monitor{
		ghClient:          NewGitHubClient(),
		db:                db,
		cfg:               cfg,
		rl:                newRateLimiter(),
		resolver:          NewRepoResolver(),
		repoAnvilMap:      make(map[string]string),
		anvilPrimaryRepos: make(map[string]string),
		triggerCh:         make(chan struct{}, 1),
	}
}

// resolveRepos returns the list of "owner/repo" strings for the given anvil,
// delegating to m.resolver (lazily creating a default one when nil so that
// test-constructed Monitor values without a resolver still work correctly).
func (m *Monitor) resolveRepos(ctx context.Context, anvil config.AnvilConfig) ([]string, error) {
	r := m.resolver
	if r == nil {
		r = NewRepoResolver()
	}
	return r.ResolveRepos(ctx, anvil)
}

// UpdateConfig replaces the monitor configuration. Safe to call while Run is
// active; the new configuration takes effect on the next scan cycle.
func (m *Monitor) UpdateConfig(cfg *config.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// Stop is a convenience no-op. Cancelling the context passed to Run is the
// primary shutdown mechanism; this method exists for API symmetry with other
// monitors.
func (m *Monitor) Stop() {}

// TriggerScan requests an immediate scan outside the normal interval. If a
// trigger is already pending (not yet consumed by the Run loop) this is a
// no-op, preventing duplicate concurrent scans.
func (m *Monitor) TriggerScan() {
	select {
	case m.triggerCh <- struct{}{}:
	default:
		// A trigger is already queued; discard the duplicate.
	}
}

// Run starts the periodic issue triage loop. Blocks until ctx is canceled.
// The poll interval is dynamically adjusted: when quota is low (<100 remaining)
// it is doubled; when a rate-limit backoff is active the loop waits until the
// backoff expires before scanning again.
func (m *Monitor) Run(ctx context.Context) error {
	m.mu.RLock()
	baseInterval := m.cfg.Settings.WicketInterval
	m.mu.RUnlock()

	if baseInterval <= 0 {
		baseInterval = 15 * time.Minute
	}

	log.Printf("[wicket] Starting issue triage monitor (interval: %s)", baseInterval)
	_ = m.db.LogEvent(state.EventWicketScanDone, "Wicket monitor started", "", "")

	// Perform an initial scan immediately so the first triage cycle does not
	// wait a full interval.
	m.scanAll(ctx)

	for {
		interval := m.effectiveInterval(baseInterval)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Println("[wicket] Shutting down issue triage monitor")
			return ctx.Err()
		case <-m.triggerCh:
			timer.Stop()
			log.Println("[wicket] Manual scan triggered")
			m.scanAll(ctx)
		case <-timer.C:
			m.scanAll(ctx)
		}
	}
}

// effectiveInterval returns the interval to wait before the next scan.
// It doubles the base interval when quota is low and waits for the backoff
// period when a rate-limit has been hit.
func (m *Monitor) effectiveInterval(base time.Duration) time.Duration {
	if m.rl.IsLimited() {
		until := m.rl.BackoffUntil()
		if wait := time.Until(until); wait > 0 {
			return wait
		}
	}
	if m.rl.IsLowQuota() {
		log.Printf("[wicket] API quota low (%d remaining) — doubling poll interval to %s",
			m.rl.Remaining(), base*2)
		return base * 2
	}
	return base
}

// scanAll iterates all enabled anvils and scans their repositories for new
// issues that need triage. It also runs follow-up steps: dispatch confirmation,
// clarification re-triage, and stale detection.
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
		if rateLimited := m.scanAnvil(ctx, name, anvil, cfg.Settings); rateLimited {
			// API quota exhausted — skip follow-up calls that would also hit
			// the GitHub API and keep hammering while backoff is active.
			continue
		}

		// Dispatch confirmation: check bead_created issues for rocket reactions
		// or "dispatch" comments from the issue author.
		m.checkDispatch(ctx, name, anvil, cfg.Settings)

		// Clarification re-triage: check ask_clarify issues for author replies.
		m.checkClarificationReTriage(ctx, name, anvil, cfg.Settings)
	}

	// Stale detection runs globally (not per-anvil) since wicket_issues rows
	// don't carry an anvil field — they are indexed by repo+number.
	m.checkStaleIssues(ctx, cfg.Settings)
	m.checkStaleClosed(ctx)

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
// Returns true when a rate-limit error was encountered so that the caller can
// skip follow-up API calls for the same cycle.
func (m *Monitor) scanAnvil(ctx context.Context, name string, anvil config.AnvilConfig, settings config.SettingsConfig) (rateLimited bool) {
	repos, err := m.resolveRepos(ctx, anvil)
	if err != nil {
		log.Printf("[wicket] %s: cannot determine repository: %v", name, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("Cannot determine repository for anvil %s: %v", name, err), "", name)
		return false
	}

	// Cache the primary repo (derived from the anvil's own git remote) so
	// triageIssue can detect issues from external monitored repos and enrich
	// the triage prompt with anvil-domain context. We only cache on success;
	// if resolveRepos fails (e.g. path is not a git repo yet), we retry on
	// the next scan cycle (lazy, outside the mutex to avoid blocking on IO).
	m.mu.RLock()
	_, primaryCached := m.anvilPrimaryRepos[name]
	m.mu.RUnlock()
	if !primaryCached && anvil.Path != "" {
		// Use a resolver with empty WicketRepos so git is always called,
		// giving us the true primary repo regardless of WicketRepos overrides.
		primaryCfg := config.AnvilConfig{Path: anvil.Path}
		if pr, err2 := m.resolveRepos(ctx, primaryCfg); err2 == nil && len(pr) > 0 {
			m.mu.Lock()
			if m.anvilPrimaryRepos == nil {
				m.anvilPrimaryRepos = make(map[string]string)
			}
			m.anvilPrimaryRepos[name] = pr[0]
			m.mu.Unlock()
		}
	}

	// Update the repo→anvil mapping so downstream code can look up anvil names.
	// Build a set of the current repos for efficient lookup.
	repoSet := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		repoSet[repo] = struct{}{}
	}

	m.mu.Lock()
	if m.repoAnvilMap == nil {
		m.repoAnvilMap = make(map[string]string)
	}

	// Drop any repos previously owned by this anvil that are no longer
	// present in its resolved repo list (e.g. wicket_repos config changed).
	for repo, anvilName := range m.repoAnvilMap {
		if anvilName == name {
			if _, ok := repoSet[repo]; !ok {
				delete(m.repoAnvilMap, repo)
			}
		}
	}

	// Add/update mappings for the current repos, but do not silently override
	// another anvil's ownership.
	for repo := range repoSet {
		if existing, ok := m.repoAnvilMap[repo]; ok && existing != name {
			log.Printf("[wicket] repo %s is already owned by anvil %s; ignoring duplicate ownership by %s", repo, existing, name)
			continue
		}
		m.repoAnvilMap[repo] = name
	}
	m.mu.Unlock()

	for _, repo := range repos {
		if ctx.Err() != nil {
			return false
		}
		if m.rl.IsLimited() {
			log.Printf("[wicket] %s: rate-limit backoff active, skipping %s until %s",
				name, repo, m.rl.BackoffUntil().Format(time.RFC3339))
			return true
		}
		if rateLimited := m.scanRepo(ctx, name, repo, anvil, settings); rateLimited {
			return true
		}
	}
	return false
}

// scanRepo fetches open issues for a single repository and triages each new
// one. Returns true when a rate-limit error was encountered so the caller can
// abort further repo scanning for this cycle.
func (m *Monitor) scanRepo(ctx context.Context, anvil, repo string, anvilCfg config.AnvilConfig, settings config.SettingsConfig) (rateLimited bool) {
	issues, err := m.ghClient.ListIssues(ctx, repo, anvilCfg.WicketIssueLabels)
	if err != nil {
		var rlErr *RateLimitError
		if isRateLimitErr(err, &rlErr) {
			delay := m.rl.RecordRateLimitHit()
			if rlErr != nil && rlErr.Remaining >= 0 {
				m.rl.UpdateRemaining(rlErr.Remaining, rlErr.ResetAt)
			}
			log.Printf("[wicket] %s: rate limited listing issues for %s — backing off %s", anvil, repo, delay)
			_ = m.db.LogEvent(state.EventWicketError,
				fmt.Sprintf("Rate limited listing issues for %s — backing off %s", repo, delay), "", anvil)
			return true
		}
		if isAuthError(err) {
			// Auth failures are non-fatal: log a clear, actionable message so
			// the operator knows which repo and why, then continue scanning
			// remaining repos. Common causes: missing gh auth for private repo,
			// SAML SSO enforcement, or insufficient token scopes.
			log.Printf("[wicket] %s: authentication failure accessing repo %s — run 'gh auth status' and verify token scopes, SSO (if enforced), and repository permissions: %v", anvil, repo, err)
			_ = m.db.LogEvent(state.EventWicketError,
				fmt.Sprintf("Authentication failure accessing repo %s: %v", repo, err), "", anvil)
			return false
		}
		log.Printf("[wicket] %s: list issues for %s: %v", anvil, repo, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("Failed to list issues for %s: %v", repo, err), "", anvil)
		return false
	}
	m.rl.RecordSuccess()

	processed := 0
	for _, issue := range issues {
		if ctx.Err() != nil {
			return false
		}
		if settings.WicketBatchSize > 0 && processed >= settings.WicketBatchSize {
			log.Printf("[wicket] %s: reached batch size %d for %s", anvil, settings.WicketBatchSize, repo)
			break
		}

		// Bot/ignored user filter: skip issues from known bots and configured
		// ignore-list users entirely, without recording anything in state.db.
		if isIgnoredUser(issue.Author, anvilCfg.WicketIgnoreUsers) {
			log.Printf("[wicket] %s: skipping %s#%d from ignored user %q", anvil, repo, issue.Number, issue.Author)
			continue
		}

		// Trigger label filter: when wicket_trigger_label is non-empty, Wicket
		// operates in pull-model — only issues explicitly tagged with that label
		// are processed. When empty (default), all issues are eligible.
		if settings.WicketTriggerLabel != "" && !hasLabel(issue, settings.WicketTriggerLabel) {
			continue
		}

		// Issue label filter: verify all required labels are present. The
		// ListIssues call already applies this filter at the API level, but
		// we re-check here for safety in case the gh CLI AND semantics differ.
		if !hasAllLabels(issue, anvilCfg.WicketIssueLabels) {
			continue
		}

		if m.shouldSkip(issue, settings) {
			continue
		}
		m.triageIssue(ctx, anvil, issue, anvilCfg, settings)
		processed++
	}
	return false
}

// shouldSkip returns true when the issue should not be triaged. An issue is
// skipped when it already carries a Wicket processing label (processed,
// bead-created, or needs-human), or is already tracked in state.db. An issue
// tracked as "pending" with no bead_id is NOT skipped — it indicates that
// bead creation failed on a previous cycle and the issue should be retried.
// All other tracked states, including non-terminal workflow states, are
// treated as already being processed and are therefore skipped.
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

	// Check whether the issue is already tracked in state.db.
	wi, err := m.db.GetWicketIssue(issue.Repo, issue.Number)
	if err != nil {
		log.Printf("[wicket] DB check %s#%d: %v — skipping to avoid double-processing", issue.Repo, issue.Number, err)
		return true
	}
	if wi == nil {
		return false
	}
	// A pending row with no bead_id means bead creation failed on a previous
	// cycle. Allow retry so the issue does not get stuck forever.
	if wi.State == "pending" && wi.BeadID == "" {
		log.Printf("[wicket] %s#%d: pending issue with no bead_id — will retry triage", issue.Repo, issue.Number)
		return false
	}
	return true
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
		if isUniqueConstraintErr(err) {
			// Check whether this is a retry of a previously stuck pending issue
			// (bead creation failed on a prior cycle). If so, proceed with triage
			// instead of bailing out — the existing row is already in the right
			// state and will be updated when the retry succeeds.
			existing, getErr := m.db.GetWicketIssue(issue.Repo, issue.Number)
			if getErr != nil {
				log.Printf("[wicket] %s: unique constraint on insert but could not read existing row for %s#%d: %v — skipping", anvil, issue.Repo, issue.Number, getErr)
				return
			}
			if existing != nil && existing.State == "pending" && existing.BeadID == "" {
				log.Printf("[wicket] %s: retrying bead creation for stuck pending issue %s#%d", anvil, issue.Repo, issue.Number)
				// Fall through: the pending row already exists; proceed with triage.
			} else {
				// First-anvil-wins: another anvil (or a concurrent cycle) already
				// claimed this issue via the UNIQUE(repo, issue_number) constraint.
				log.Printf("[wicket] %s: issue already tracked by another anvil, skipping %s#%d", anvil, issue.Repo, issue.Number)
				return
			}
		} else {
			log.Printf("[wicket] %s: failed to insert wicket issue %s#%d: %v", anvil, issue.Repo, issue.Number, err)
			return
		}
	}

	var decision TriageDecision

	if isTrustedUser(issue.Author, anvilCfg.WicketTrustedUsers) {
		// Trusted authors still go through AI triage so that duplicate,
		// already-fixed, and out-of-scope outcomes are detected. If the AI
		// fails to parse (flag_human fallback), we default to create_bead
		// using the raw issue title and body — the author is trusted.
		log.Printf("[wicket] %s: %s#%d author %q is trusted — running AI triage with bead context", anvil, issue.Repo, issue.Number, issue.Author)
		triageFn := m.triageFunc
		if triageFn == nil {
			triageFn = RunTriageWithComments
		}
		// Resolve monitored paths: find all anvil paths that share repos
		// with the current anvil (via the 5a wicket_repos mapping) so the
		// triage prompt includes bead context from all related repositories.
		monitoredPaths := m.anvilPathsForRepos(anvilCfg.WicketRepos)
		if len(monitoredPaths) == 0 && anvilCfg.Path != "" {
			// Fallback: no explicit wicket_repos config; use the single anvil path.
			monitoredPaths = []string{anvilCfg.Path}
		}
		// Look up the cached primary repo for this anvil so RunTriage can
		// detect when the issue originates from an external monitored repo
		// and include anvil-domain README/AGENTS.md context in the prompt.
		m.mu.RLock()
		anvilPrimaryRepo := m.anvilPrimaryRepos[anvil]
		m.mu.RUnlock()
		decision = triageFn(ctx, issue, nil, TriageConfig{
			Providers:           buildProviders(settings),
			ExtraPrompt:         anvilCfg.WicketTriagePrompt,
			AnvilPath:           anvilCfg.Path,
			AnvilRepo:           anvilPrimaryRepo,
			AllAnvilPaths:       m.allAnvilPaths(),
			MonitoredAnvilPaths: monitoredPaths,
		})
		switch decision.Action {
		case ActionDuplicate, ActionAlreadyFixed, ActionOutOfScope:
			// Honor smart triage outcomes even for trusted users.
		default:
			// For all other outcomes (create_bead, ask_clarify, flag_human,
			// reject, or parse-failure fallback), default to create_bead
			// using the issue content directly — trusted user guarantee.
			decision = TriageDecision{
				Action:          ActionCreateBead,
				Reason:          "issue author is a trusted user",
				BeadTitle:       issue.Title,
				BeadDescription: issue.Body,
			}
		}
	} else {
		// Non-trusted user: do NOT run full AI triage. Check for spam first;
		// if not spam, post a generic response and flag for human review.
		if isLikelySpam(issue) {
			log.Printf("[wicket] %s: %s#%d from %q appears to be spam — rejecting silently", anvil, issue.Repo, issue.Number, issue.Author)
			decision = TriageDecision{
				Action: ActionReject,
				Reason: "issue appears to be spam or off-topic",
			}
		} else {
			// Post generic response and flag for human review. This path
			// handles everything itself and returns early.
			log.Printf("[wicket] %s: %s#%d from non-trusted user %q — posting generic response", anvil, issue.Repo, issue.Number, issue.Author)
			_ = m.db.LogEvent(state.EventWicketIssueTriage,
				fmt.Sprintf("[%s] %s#%d triage=flag_human (non-trusted) author=%s", anvil, issue.Repo, issue.Number, issue.Author),
				"", anvil)
			m.handleNonTrustedUser(ctx, anvil, issue, settings)
			return
		}
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
	case ActionReject:
		m.handleReject(ctx, anvil, issue, decision, settings)
	case ActionDuplicate:
		m.handleDuplicate(ctx, anvil, issue, decision, settings)
	case ActionAlreadyFixed:
		m.handleAlreadyFixed(ctx, anvil, issue, decision, settings)
	case ActionOutOfScope:
		m.handleOutOfScope(ctx, anvil, issue, decision, settings)
	default:
		log.Printf("[wicket] %s: unknown triage action %q for %s#%d", anvil, decision.Action, issue.Repo, issue.Number)
	}
}

// handleDuplicate posts a comment referencing the existing bead that already
// covers this issue and records the outcome in state.db.
func (m *Monitor) handleDuplicate(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	log.Printf("[wicket] %s: %s#%d is duplicate of %s: %s", anvil, issue.Repo, issue.Number, decision.DuplicateID, decision.Reason)
	_ = m.db.LogEvent(state.EventWicketRejected,
		fmt.Sprintf("[%s] %s#%d duplicate of %s: %s", anvil, issue.Repo, issue.Number, decision.DuplicateID, decision.Reason),
		"", anvil)

	comment, err := RenderDuplicate(DuplicateData{DuplicateID: decision.DuplicateID})
	if err != nil {
		log.Printf("[wicket] %s: render duplicate comment: %v", anvil, err)
	} else if cerr := m.ghClient.CommentOnIssue(ctx, issue.Repo, issue.Number, comment); cerr != nil {
		log.Printf("[wicket] %s: comment on %s#%d: %v", anvil, issue.Repo, issue.Number, cerr)
	}

	labels := []string{settings.WicketProcessedLabel}
	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, labels); lerr != nil {
		log.Printf("[wicket] %s: add label on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}

	m.persistOutcome(issue, "rejected", decision)
}

// handleAlreadyFixed posts a comment referencing the PR or bead that resolved
// the issue and records the outcome in state.db.
func (m *Monitor) handleAlreadyFixed(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	log.Printf("[wicket] %s: %s#%d already fixed (ref: %s): %s", anvil, issue.Repo, issue.Number, decision.ReferencePR, decision.Reason)
	_ = m.db.LogEvent(state.EventWicketRejected,
		fmt.Sprintf("[%s] %s#%d already fixed (ref: %s): %s", anvil, issue.Repo, issue.Number, decision.ReferencePR, decision.Reason),
		"", anvil)

	comment, err := RenderAlreadyFixed(AlreadyFixedData{ReferencePR: decision.ReferencePR})
	if err != nil {
		log.Printf("[wicket] %s: render already_fixed comment: %v", anvil, err)
	} else if cerr := m.ghClient.CommentOnIssue(ctx, issue.Repo, issue.Number, comment); cerr != nil {
		log.Printf("[wicket] %s: comment on %s#%d: %v", anvil, issue.Repo, issue.Number, cerr)
	}

	labels := []string{settings.WicketProcessedLabel}
	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, labels); lerr != nil {
		log.Printf("[wicket] %s: add label on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}

	m.persistOutcome(issue, "rejected", decision)
}

// handleOutOfScope posts a rejection comment with the AI reasoning and records
// the outcome in state.db.
func (m *Monitor) handleOutOfScope(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	log.Printf("[wicket] %s: %s#%d out of scope: %s", anvil, issue.Repo, issue.Number, decision.Reason)
	_ = m.db.LogEvent(state.EventWicketRejected,
		fmt.Sprintf("[%s] %s#%d out of scope: %s", anvil, issue.Repo, issue.Number, decision.Reason),
		"", anvil)

	comment, err := RenderOutOfScope(OutOfScopeData{Reason: decision.Reason})
	if err != nil {
		log.Printf("[wicket] %s: render out_of_scope comment: %v", anvil, err)
	} else if cerr := m.ghClient.CommentOnIssue(ctx, issue.Repo, issue.Number, comment); cerr != nil {
		log.Printf("[wicket] %s: comment on %s#%d: %v", anvil, issue.Repo, issue.Number, cerr)
	}

	labels := []string{settings.WicketProcessedLabel}
	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, labels); lerr != nil {
		log.Printf("[wicket] %s: add label on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}

	m.persistOutcome(issue, "rejected", decision)
}

// handleCreateBead creates a bead, posts a confirmation comment, and labels
// the issue as processed+bead-created.
func (m *Monitor) handleCreateBead(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	// Guard against an empty description (e.g. trusted-user path with no body).
	if decision.BeadDescription == "" {
		decision.BeadDescription = issue.Title
	}

	m.mu.RLock()
	anvilPath := m.cfg.Anvils[anvil].Path
	m.mu.RUnlock()
	beadID, err := CreateBead(ctx, m.db, decision, issue, 2, anvil, anvilPath)
	if err != nil {
		log.Printf("[wicket] %s: create bead for %s#%d: %v", anvil, issue.Repo, issue.Number, err)
		_ = m.db.LogEvent(state.EventWicketError,
			fmt.Sprintf("[%s] Failed to create bead for %s#%d: %v", anvil, issue.Repo, issue.Number, err), "", anvil)
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

	// When auto-dispatch is enabled, immediately tag the bead with the
	// anvil's dispatch tag (e.g. "forgeReady") so the poller picks it up.
	m.mu.RLock()
	anvilCfg := m.cfg.Anvils[anvil]
	m.mu.RUnlock()
	if anvilCfg.WicketAutoDispatch && anvilCfg.AutoDispatchTag != "" {
		labelCmd, labelCancel := executil.BdCommand(ctx, "label", "add", beadID, anvilCfg.AutoDispatchTag)
		defer labelCancel()
		if anvilPath != "" {
			labelCmd.Dir = anvilPath
		}
		if labelOut, labelErr := labelCmd.CombinedOutput(); labelErr != nil {
			log.Printf("[wicket] %s: auto-dispatch tag for %s: %v: %s", anvil, beadID, labelErr, labelOut)
		} else {
			wi := state.WicketIssue{
				Repo:        issue.Repo,
				IssueNumber: issue.Number,
				BeadID:      beadID,
			}
			wi.State = StateDispatched
			_ = m.db.UpdateWicketIssue(wi)
			log.Printf("[wicket] %s: auto-dispatched bead %s for %s#%d", anvil, beadID, issue.Repo, issue.Number)
		}
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

// handleNonTrustedUser handles the conservative triage path for issues from
// non-trusted contributors. It posts a generic acknowledgement comment and
// applies the processed and needs-human labels without running AI triage.
func (m *Monitor) handleNonTrustedUser(ctx context.Context, anvil string, issue Issue, settings config.SettingsConfig) {
	_ = m.db.LogEvent(state.EventWicketFlaggedHuman,
		fmt.Sprintf("[%s] %s#%d flagged for human review (non-trusted user: %s)", anvil, issue.Repo, issue.Number, issue.Author),
		"", anvil)

	comment, err := RenderGenericNonTrustedUser(GenericNonTrustedUserData{Author: issue.Author})
	if err == nil {
		if cerr := m.ghClient.CommentOnIssue(ctx, issue.Repo, issue.Number, comment); cerr != nil {
			log.Printf("[wicket] %s: comment on %s#%d: %v", anvil, issue.Repo, issue.Number, cerr)
		}
	}

	labels := []string{settings.WicketProcessedLabel, settings.WicketNeedsHumanLabel}
	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, labels); lerr != nil {
		log.Printf("[wicket] %s: add labels on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}

	m.persistOutcome(issue, "needs_human", TriageDecision{
		Action: ActionFlagHuman,
		Reason: "issue author is not a trusted contributor — a maintainer will review",
	})
}

// handleReject silently discards an issue that appears to be spam or
// off-topic. No public comment is posted; only the processed label is applied.
func (m *Monitor) handleReject(ctx context.Context, anvil string, issue Issue, decision TriageDecision, settings config.SettingsConfig) {
	log.Printf("[wicket] %s: %s#%d rejected as spam/off-topic: %s", anvil, issue.Repo, issue.Number, decision.Reason)
	_ = m.db.LogEvent(state.EventWicketRejected,
		fmt.Sprintf("[%s] %s#%d rejected: %s", anvil, issue.Repo, issue.Number, decision.Reason),
		"", anvil)

	// Apply only the processed label — no public comment for spam.
	if lerr := m.ghClient.AddLabels(ctx, issue.Repo, issue.Number, []string{settings.WicketProcessedLabel}); lerr != nil {
		log.Printf("[wicket] %s: add label on %s#%d: %v", anvil, issue.Repo, issue.Number, lerr)
	}

	m.persistOutcome(issue, "rejected", decision)
}

// persistOutcome updates the wicket_issues row with the final triage state.
// CreateBead handles its own persistence; this is used only for ask_clarify,
// flag_human, rejected, and re-triage outcomes.
//
// The existing row is loaded first so that fields set by other code paths
// (CommentCount, BeadID, PRNumber, PRUrl, AuthorRepliedAt) are not
// accidentally zeroed out when UpdateWicketIssue overwrites all columns.
func (m *Monitor) persistOutcome(issue Issue, issueState string, decision TriageDecision) {
	existing, err := m.db.GetWicketIssue(issue.Repo, issue.Number)
	if err != nil {
		log.Printf("[wicket] load existing wicket issue for %s#%d: %v", issue.Repo, issue.Number, err)
	}

	now := time.Now().UTC()
	var wi state.WicketIssue
	if existing != nil {
		wi = *existing
	} else {
		wi = state.WicketIssue{
			Repo:        issue.Repo,
			IssueNumber: issue.Number,
			Title:       issue.Title,
			Body:        issue.Body,
			Author:      issue.Author,
		}
	}
	wi.State = issueState
	wi.TriageAction = string(decision.Action)
	wi.TriageReason = decision.Reason
	wi.ProcessedAt = &now

	if err := m.db.UpdateWicketIssue(wi); err != nil {
		log.Printf("[wicket] persist outcome for %s#%d: %v", issue.Repo, issue.Number, err)
	}
}

// AnvilForRepo returns the anvil name that owns the given "owner/repo" string,
// and whether a mapping exists. The mapping is populated during scanAnvil and
// is consumed by downstream workers (Wicket phases 5b and 5c) that need to
// route GitHub events back to the correct anvil without re-resolving the git
// remote on every call.
func (m *Monitor) AnvilForRepo(repo string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, ok := m.repoAnvilMap[repo]
	return name, ok
}

// anvilPathsForRepos returns the local filesystem paths of all configured
// anvils whose explicit wicket_repos list contains any of the given GitHub
// repo slugs. The current anvil's own path is always included via the
// caller providing it in repos context. The result is sorted for
// deterministic ordering. This is used to populate MonitoredAnvilPaths in
// TriageConfig so that the triage prompt includes bead context from all
// repos monitored by the triggering anvil (Wicket 5a mapping).
func (m *Monitor) anvilPathsForRepos(repos []string) []string {
	if len(repos) == 0 {
		return nil
	}
	repoSet := make(map[string]struct{}, len(repos))
	for _, r := range repos {
		repoSet[strings.ToLower(r)] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	pathSet := make(map[string]struct{})
	for _, anvil := range m.cfg.Anvils {
		if anvil.Path == "" {
			continue
		}
		for _, r := range anvil.WicketRepos {
			if _, ok := repoSet[strings.ToLower(r)]; ok {
				pathSet[anvil.Path] = struct{}{}
				break
			}
		}
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// allAnvilPaths returns the filesystem paths of all currently configured
// anvils. It is used to populate AllAnvilPaths in TriageConfig so that
// RunTriage can perform a cross-anvil Source URL duplicate check.
func (m *Monitor) allAnvilPaths() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Collect names first so we can sort them for deterministic ordering.
	// Map iteration order is nondeterministic; sorting by name ensures the
	// first cross-anvil duplicate match is stable across runs.
	names := make([]string, 0, len(m.cfg.Anvils))
	for name, anvil := range m.cfg.Anvils {
		if anvil.Path != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, m.cfg.Anvils[name].Path)
	}
	return paths
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

// isUniqueConstraintErr returns true when err is a SQLite UNIQUE constraint
// violation (extended result code SQLITE_CONSTRAINT_UNIQUE). It uses errors.As
// to unwrap the modernc.org/sqlite driver error and checks the numeric code,
// avoiding brittle string-matching that can break across driver versions.
func isUniqueConstraintErr(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// isIgnoredUser returns true if the author should be skipped entirely. It
// checks against the hardcoded defaultBotIgnoreList and any custom ignore
// users configured per-anvil. Comparison is case-insensitive.
func isIgnoredUser(author string, customIgnoreList []string) bool {
	for _, bot := range defaultBotIgnoreList {
		if strings.EqualFold(author, bot) {
			return true
		}
	}
	for _, u := range customIgnoreList {
		if strings.EqualFold(author, u) {
			return true
		}
	}
	return false
}

// hasLabel returns true if the issue carries the specified label (case-insensitive).
func hasLabel(issue Issue, label string) bool {
	for _, l := range issue.Labels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}

// hasAllLabels returns true if the issue carries all of the required labels.
// Returns true when requiredLabels is empty.
func hasAllLabels(issue Issue, requiredLabels []string) bool {
	if len(requiredLabels) == 0 {
		return true
	}
	for _, req := range requiredLabels {
		if !hasLabel(issue, req) {
			return false
		}
	}
	return true
}

// isLikelySpam returns true when the issue matches simple heuristics that
// suggest it is spam or a test submission not worth engaging with publicly.
// This is intentionally conservative — false positives turn real issues into
// silent rejections, so only flag the most obviously low-quality content.
func isLikelySpam(issue Issue) bool {
	title := strings.TrimSpace(issue.Title)
	body := strings.TrimSpace(issue.Body)
	titleLower := strings.ToLower(title)

	// Completely empty submissions are not meaningful issues.
	if len(titleLower) == 0 && len(body) == 0 {
		return true
	}

	// Issues with no body are only treated as spam when the title is a known
	// placeholder or explicit test phrase. This avoids dropping legitimate
	// short-title issues like "Help" or "Docs" that forgot a description.
	if len(body) == 0 {
		shortPlaceholderTitles := []string{
			"test",
			"testing",
			"ignore",
			"please ignore",
			"dummy",
			"sample",
		}
		for _, ph := range shortPlaceholderTitles {
			if titleLower == ph {
				return true
			}
		}
	}

	// Exact-match well-known placeholder/test titles. These are always spam
	// regardless of whether a body is present.
	alwaysSpamTitles := []string{
		"asdfgh", "qwerty",
	}
	for _, s := range alwaysSpamTitles {
		if titleLower == s {
			return true
		}
	}

	return false
}
