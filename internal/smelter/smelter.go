// Package smelter batches pending warden rules into PRs. The Smelter
// periodically queries pending_warden_rules from the state DB, groups them
// by anvil, appends them to each anvil's .forge/warden-rules.yaml (deduping
// by rule ID), commits the change, and creates or updates a PR on the
// forge/warden-learn-batch/<anvil> branch.
package smelter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
	"gopkg.in/yaml.v3"
)

// cleanGitEnv returns os.Environ() with GIT_DIR, GIT_WORK_TREE, and
// GIT_CEILING_DIRECTORIES stripped. When the smelter runs inside a Forge
// worktree these variables are set to the parent repo's paths; leaving them
// in place would confuse git subprocesses that operate on a different tree.
func cleanGitEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_DIR=") || strings.HasPrefix(e, "GIT_WORK_TREE=") || strings.HasPrefix(e, "GIT_CEILING_DIRECTORIES=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

const (
	// prTitle is the title used for smelter PRs.
	prTitle = "forge: batch warden rule update [no-changelog]"

	// cleanupTimeout is how long cleanup operations (worktree removal) are
	// allowed to run after the parent context has been canceled.
	cleanupTimeout = 30 * time.Second
)

// branchForAnvil returns the per-anvil batch branch name.
func branchForAnvil(anvilName string) string {
	return "forge/warden-learn-batch/" + anvilName
}

// Smelter batches pending warden rules into PRs on a schedule.
type Smelter struct {
	db         *state.DB
	wtMgr      *worktree.Manager
	interval   time.Duration
	anvilPaths map[string]string // anvil name -> path
	mu         sync.RWMutex

	// intervalCh signals Run to reset the ticker with a new duration.
	intervalCh chan time.Duration

	// consolidator is the AI invocation hook used by the consolidation pass.
	// When nil, consolidation is skipped (legacy behavior). Set via
	// WithConsolidator.
	consolidator warden.ConsolidationRunner
	// dedupThreshold returns the active similarity threshold. When nil or it
	// returns <= 0, consolidation is skipped.
	dedupThreshold func() float64
	// archiveAfterDays returns the staleness threshold in days for Pass 2.
	// When nil or it returns <= 0, the staleness pass is skipped.
	archiveAfterDays func() int
}

// Option configures a Smelter at construction time.
type Option func(*Smelter)

// WithConsolidator wires the AI invocation hook used by Pass 1 consolidation.
// When the runner returns a JSON object with the merged rule fields, the
// Smelter replaces the cluster members in the active rules file and moves
// the originals to the archive store. Pass nil to disable consolidation.
func WithConsolidator(runner warden.ConsolidationRunner) Option {
	return func(s *Smelter) { s.consolidator = runner }
}

// WithDedupThreshold supplies a function the Smelter calls at flush time to
// resolve the current dedup similarity threshold. A function (rather than a
// value) is used so config hot-reload can take effect without restarting
// the smelter. A nil function or one returning <= 0 disables consolidation.
func WithDedupThreshold(fn func() float64) Option {
	return func(s *Smelter) { s.dedupThreshold = fn }
}

// WithArchiveAfterDays supplies a function the Smelter calls at flush time
// to resolve the staleness threshold (in days) used by Pass 2. A function
// is used so config hot-reload can take effect without restarting. A nil
// function or one returning <= 0 disables the staleness pass.
func WithArchiveAfterDays(fn func() int) Option {
	return func(s *Smelter) { s.archiveAfterDays = fn }
}

// New creates a Smelter. interval controls how often Flush is called;
// pass 0 to disable scheduled runs (Flush can still be called directly).
func New(db *state.DB, interval time.Duration, anvilPaths map[string]string, opts ...Option) *Smelter {
	s := &Smelter{
		db:         db,
		wtMgr:      worktree.NewManager(),
		interval:   interval,
		anvilPaths: anvilPaths,
		intervalCh: make(chan time.Duration, 1),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// UpdateAnvilPaths replaces the set of anvils. Safe to call while Run is active.
func (s *Smelter) UpdateAnvilPaths(paths map[string]string) {
	copied := make(map[string]string, len(paths))
	maps.Copy(copied, paths)
	s.mu.Lock()
	s.anvilPaths = copied
	s.mu.Unlock()
}

// UpdateInterval signals the Run loop to reset its ticker with d. If d <= 0
// the ticker is paused until a positive interval is sent. Safe to call
// concurrently while Run is active.
func (s *Smelter) UpdateInterval(d time.Duration) {
	// Loop until we successfully send d into the buffered channel (capacity 1).
	// If the channel is full we drain the stale value first, then retry.
	// This guarantees the latest value always wins without blocking callers.
	for {
		select {
		case s.intervalCh <- d:
			return
		default:
		}
		// Channel is full — drain the stale value and retry.
		select {
		case <-s.intervalCh:
		default:
		}
	}
}

// timeUntilNextFlush returns how long to wait before the first flush after
// startup. It bases this on the timestamp of the most recent
// EventSmelterCycleDone event: if that cycle completed at T, the next flush
// is due at T+interval, so the delay is max(0, T+interval-now).
//
// Returning a positive duration means the first flush should be deferred (the
// ticker's initial period is set to this value); returning 0 means a flush
// should run immediately on startup.
//
// When interval <= 0, or when no prior cycle is found, or on any DB error, 0
// is returned so we err on the side of flushing promptly.
func (s *Smelter) timeUntilNextFlush() time.Duration {
	if s.interval <= 0 {
		return 0
	}
	lastCycle, ok, err := s.db.LastEventTime(state.EventSmelterCycleDone)
	if err != nil {
		log.Printf("[smelter] Could not read last flush cycle time, will run startup flush: %v", err)
		return 0
	}
	if !ok {
		return 0
	}
	remaining := time.Until(lastCycle.Add(s.interval))
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// Run starts the periodic flush loop. Blocks until ctx is canceled.
// If interval <= 0 at startup, scheduled flushes are paused until
// UpdateInterval is called with a positive value.
func (s *Smelter) Run(ctx context.Context) error {
	log.Printf("[smelter] Starting smelter (interval: %s)", s.interval)
	_ = s.db.LogEvent(state.EventSmelterStarted,
		fmt.Sprintf("Smelter started (interval: %s)", s.interval), "", "")

	// Determine when the first flush should run. If a recent successful cycle
	// completed less than interval ago, defer the first flush to T+interval
	// rather than running immediately — this prevents a near-2× cadence when
	// the daemon restarts shortly before the next scheduled flush. When no
	// recent cycle is found (or interval <= 0), firstDelay is 0 and we flush
	// immediately on startup as before.
	firstDelay := s.timeUntilNextFlush()
	if firstDelay <= 0 {
		// Initial flush: run once on startup to ensure pending rules are processed
		// promptly. Scheduled flushes are handled by the ticker below.
		if err := s.Flush(ctx); err != nil {
			log.Printf("[smelter] Initial flush error: %v", err)
		}
	} else {
		log.Printf("[smelter] Deferring first flush by %s (resuming prior schedule)", firstDelay.Round(time.Second))
	}

	// Create a ticker. For the first interval use firstDelay when positive so
	// the initial tick aligns with lastCycle+interval rather than now+interval.
	// After the first tick the ticker is reset to the configured interval.
	// If the initial interval is <= 0, use a placeholder duration and stop the
	// ticker immediately so scheduled flushes are paused until a positive
	// interval arrives via UpdateInterval.
	startInterval := firstDelay
	if startInterval <= 0 {
		startInterval = s.interval
	}
	if startInterval <= 0 {
		startInterval = time.Hour // placeholder; stopped immediately below
	}
	ticker := time.NewTicker(startInterval)
	// needsReset is true when the ticker was started with firstDelay and the
	// first tick should reset it to the configured interval.
	needsReset := firstDelay > 0 && s.interval > 0
	if s.interval <= 0 {
		ticker.Stop()
		log.Println("[smelter] Scheduled flushes paused (interval <= 0); waiting for positive interval")
	}
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[smelter] Shutting down smelter")
			return ctx.Err()
		case newInterval := <-s.intervalCh:
			s.mu.Lock()
			s.interval = newInterval
			s.mu.Unlock()
			needsReset = false // interval changed externally; no longer need auto-reset
			if newInterval > 0 {
				log.Printf("[smelter] Interval changed to %s; resetting ticker", newInterval)
				ticker.Reset(newInterval)
			} else {
				log.Printf("[smelter] Interval set to <= 0; pausing ticker")
				ticker.Stop()
			}
		case <-ticker.C:
			if needsReset {
				needsReset = false
				s.mu.RLock()
				regularInterval := s.interval
				s.mu.RUnlock()
				if regularInterval > 0 {
					ticker.Reset(regularInterval)
				}
			}
			if err := s.Flush(ctx); err != nil {
				log.Printf("[smelter] Flush error: %v", err)
			}
		}
	}
}

// Flush queries all pending warden rules, groups them by anvil, and for each
// anvil: creates a worktree, appends rules to warden-rules.yaml (deduping by
// rule ID), commits, force-pushes to the batch branch, and creates/updates a
// PR. Flushed rules are deleted from the DB on success.
func (s *Smelter) Flush(ctx context.Context) error {
	byAnvil, err := s.db.QueryPendingRulesByAnvil()
	if err != nil {
		return fmt.Errorf("querying pending rules: %w", err)
	}
	if len(byAnvil) == 0 {
		// Record a cycle-done event even when there's nothing pending so the
		// startup-skip check knows a flush cycle ran recently.
		_ = s.db.LogEvent(state.EventSmelterCycleDone, "smelter flush cycle complete (nothing pending)", "", "")
		return nil // no-op: nothing pending
	}

	s.mu.RLock()
	anvilPaths := make(map[string]string, len(s.anvilPaths))
	maps.Copy(anvilPaths, s.anvilPaths)
	s.mu.RUnlock()

	var anyFailed bool
	for anvilName, rules := range byAnvil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		anvilPath, ok := anvilPaths[anvilName]
		if !ok {
			log.Printf("[smelter] Anvil %q not found in config — skipping %d pending rule(s)", anvilName, len(rules))
			continue
		}

		if err := s.flushAnvil(ctx, anvilName, anvilPath, rules); err != nil {
			anyFailed = true
			log.Printf("[smelter] Error flushing anvil %s: %v", anvilName, err)
			_ = s.db.LogEvent(state.EventSmelterFailed,
				fmt.Sprintf("Smelter flush failed for %s: %v", anvilName, err), "", anvilName)
			continue
		}
	}
	// Only record cycle-done when all pending rules were successfully drained.
	// If any anvil failed, those rules remain in the DB and the startup-skip
	// check must not treat this as a completed cycle — otherwise a restart
	// shortly after a partial failure would postpone retries for still-pending
	// rules until the next ticker interval.
	if !anyFailed {
		_ = s.db.LogEvent(state.EventSmelterCycleDone, "smelter flush cycle complete", "", "")
	}
	return nil
}

func (s *Smelter) flushAnvil(ctx context.Context, anvilName, anvilPath string, rules []state.PendingRule) error {
	branch := branchForAnvil(anvilName)
	wtBeadID := "smelter-batch-" + anvilName

	// 1. Create an isolated worktree for the batch branch.
	wt, err := s.wtMgr.CreateWithOptions(ctx, anvilPath, wtBeadID, worktree.CreateOptions{
		Branch:      branch,
		ResetBranch: true, // always reset to main for a clean diff
	})
	if err != nil {
		return fmt.Errorf("creating worktree for %s: %w", anvilName, err)
	}

	// Clean up the worktree when done, using a background context so cleanup
	// succeeds even if the parent ctx has been canceled.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = s.wtMgr.Remove(cleanupCtx, anvilPath, wt)
	}()

	// 2. Parse each pending rule's YAML and load the current rules file
	//    from the worktree (reflects main branch state after reset).
	rf, err := warden.LoadRules(wt.Path)
	if err != nil {
		return fmt.Errorf("loading warden rules for %s: %w", anvilName, err)
	}

	var addedIDs []string
	var flushedIDs []int
	for _, pr := range rules {
		var rule warden.Rule
		if err := yaml.Unmarshal([]byte(pr.RuleYAML), &rule); err != nil {
			log.Printf("[smelter] Skipping malformed rule (id=%d, anvil=%s): %v", pr.ID, anvilName, err)
			// Still mark for deletion so it doesn't jam the queue.
			flushedIDs = append(flushedIDs, pr.ID)
			continue
		}
		if rf.AddRule(rule) {
			addedIDs = append(addedIDs, rule.ID)
		}
		flushedIDs = append(flushedIDs, pr.ID)
	}

	// 2b. Pass 1 consolidation: cluster near-duplicate rules and merge each
	//     cluster into a single canonical rule via the warden-stage AI. Runs
	//     before save/commit so the merge lands in the same Smelter PR.
	consolidationSummary, archived, err := s.runConsolidation(ctx, wt.Path, anvilName, rf)
	if err != nil {
		// Non-fatal: log and continue with whatever rules we have. The original
		// (pre-consolidation) rules are still in rf because Consolidate only
		// mutates rf when at least one cluster merged successfully.
		log.Printf("[smelter] consolidation error for %s: %v", anvilName, err)
	}

	// 2c. Pass 2 staleness archive: move rules that have aged past the
	//     configured threshold and have had no recent source activity into
	//     the archive store with reason="stale". Runs after Pass 1 so the
	//     newly-merged rules in rf.Rules are still candidates for staleness
	//     in future flushes. Pass 3 only operates on whatever remains in
	//     rf.Rules after this step.
	staleArchived := s.runStaleness(anvilName, rf)

	// 2d. Pass 3 paths backfill: for each active rule whose Paths field is
	//     empty and whose Source carries a copilot:PR#N token, fetch the
	//     PR's changed files and derive file-extension globs. Idempotent:
	//     rules with non-empty Paths are skipped. Pass 3 only mutates
	//     existing rules in rf, so no archive write is required.
	backfilledIDs := s.runPathsBackfill(ctx, wt.Path, anvilName, rf)
	if len(backfilledIDs) > 0 {
		log.Printf("[smelter] paths backfilled on %d rule(s) for %s", len(backfilledIDs), anvilName)
		_ = s.db.LogEvent(state.EventSmelterFlushed,
			fmt.Sprintf("Backfilled paths on %d rule(s) for %s", len(backfilledIDs), anvilName), "", anvilName)
	}

	passes := PassResults{
		Added:        addedIDs,
		Consolidated: consolidationSummary,
		Archived:     staleArchived,
		Backfilled:   backfilledIDs,
	}

	if !passes.HasChanges() {
		// All rules were duplicates (already in the file) or malformed AND
		// nothing was consolidated, aged out, or path-backfilled. Delete
		// from DB and emit a flush event.
		log.Printf("[smelter] All %d pending rule(s) for %s were duplicates or malformed — cleaning up", len(rules), anvilName)
		if err := s.db.DeletePendingRules(flushedIDs); err != nil {
			return err
		}
		// Record a smelter_flushed event even when no new rules were added so that
		// successful queue draining is visible in the event log.
		_ = s.db.LogEvent(state.EventSmelterFlushed,
			fmt.Sprintf("Flushed 0 new warden rule(s) for %s (%d total pending processed — all duplicates/malformed)", anvilName, len(flushedIDs)),
			"", anvilName)
		return nil
	}

	// 3. Persist the archive and active rules file. Archive must land first
	//    so the active rules file can never be committed without a matching
	//    archive entry for the rules it replaced (the bead contract). If
	//    either step fails we abort before staging/commit/push, leaving the
	//    pending queue intact for the next flush.
	if err := s.persistRulesAndArchive(wt.Path, rf, archived, consolidationSummary, staleArchived); err != nil {
		return fmt.Errorf("persisting warden rules for %s: %w", anvilName, err)
	}

	// 4. Commit and force-push from the worktree.
	if err := s.commitAndPush(ctx, wt.Path, branch, passes); err != nil {
		return fmt.Errorf("commit/push for %s: %w", anvilName, err)
	}

	// 5. Create or update the PR.
	if err := s.ensurePR(ctx, wt.Path, anvilName, branch, len(addedIDs)); err != nil {
		return fmt.Errorf("PR creation for %s: %w", anvilName, err)
	}

	// 6. Delete flushed rules from the DB.
	if err := s.db.DeletePendingRules(flushedIDs); err != nil {
		return fmt.Errorf("deleting flushed rules for %s: %w", anvilName, err)
	}

	msg := fmt.Sprintf("Flushed %d new warden rule(s) for %s (%d total pending processed)", len(addedIDs), anvilName, len(rules))
	log.Printf("[smelter] %s", msg)
	_ = s.db.LogEvent(state.EventSmelterFlushed, msg, "", anvilName)

	return nil
}

// runConsolidation invokes warden.Consolidate over the in-memory rules file
// when a consolidator and positive threshold are configured. It returns the
// per-cluster summary and the original rule entries that were superseded so
// the caller can write them to the archive store. Errors that occur for
// individual clusters are aggregated and returned but do not abort the pass.
func (s *Smelter) runConsolidation(ctx context.Context, wtPath, anvilName string, rf *warden.RulesFile) ([]warden.MergeResult, []warden.Rule, error) {
	if s.consolidator == nil || s.dedupThreshold == nil {
		return nil, nil, nil
	}
	threshold := s.dedupThreshold()
	if threshold <= 0 {
		return nil, nil, nil
	}
	replaced, summary, errs := warden.Consolidate(ctx, wtPath, rf, threshold, s.consolidator)
	if len(summary) > 0 {
		log.Printf("[smelter] consolidated %d cluster(s) for %s", len(summary), anvilName)
		_ = s.db.LogEvent(state.EventSmelterFlushed,
			fmt.Sprintf("Consolidated %d cluster(s) for %s", len(summary), anvilName), "", anvilName)
	}
	var combined error
	if len(errs) > 0 {
		// Surface the first error verbatim; the rest go to the log.
		for i, e := range errs {
			if i == 0 {
				combined = e
				continue
			}
			log.Printf("[smelter] additional consolidation error for %s: %v", anvilName, e)
		}
	}
	return summary, replaced, combined
}

// runStaleness invokes warden.ArchiveStale on the active rules slice when an
// archive-after threshold is configured. Stale rules are removed from rf in
// place and returned as ArchivedRule entries so the caller can persist them
// to the archive store and surface the count in the commit message.
//
// When archiveAfterDays is nil or returns <= 0, the pass is a no-op and the
// returned slice is empty — rf.Rules is left untouched.
func (s *Smelter) runStaleness(anvilName string, rf *warden.RulesFile) []warden.ArchivedRule {
	if s.archiveAfterDays == nil {
		return nil
	}
	threshold := s.archiveAfterDays()
	if threshold <= 0 {
		return nil
	}
	active, stale := warden.ArchiveStale(rf.Rules, threshold, time.Now().UTC())
	if len(stale) == 0 {
		return nil
	}
	rf.Rules = active
	log.Printf("[smelter] archived %d stale rule(s) for %s (threshold=%dd)", len(stale), anvilName, threshold)
	_ = s.db.LogEvent(state.EventSmelterFlushed,
		fmt.Sprintf("Archived %d stale rule(s) for %s", len(stale), anvilName), "", anvilName)
	return stale
}

// persistRulesAndArchive writes the archive entries (when any) and then
// saves the active rules file. Ordering is load-bearing: the archive must
// land before the active rules file so a partial failure can never leave
// the rules file on disk without a matching archive record for the rules
// it superseded. Any error from either step is returned so callers can
// abort the flush before staging/commit/push.
func (s *Smelter) persistRulesAndArchive(wtPath string, rf *warden.RulesFile, archived []warden.Rule, summary []warden.MergeResult, stale []warden.ArchivedRule) error {
	if len(archived) > 0 || len(stale) > 0 {
		if err := s.archiveRules(wtPath, archived, summary, stale); err != nil {
			return fmt.Errorf("archiving rules: %w", err)
		}
	}
	if err := warden.SaveRules(wtPath, rf); err != nil {
		return fmt.Errorf("saving warden rules: %w", err)
	}
	return nil
}

// archiveRules persists archive entries from Pass 1 (duplicates) and Pass 2
// (stale) to the per-anvil archive store. Pass 1 entries are added with
// reason="duplicate" and superseded_by set to the merged rule's ID. Pass 2
// entries are appended verbatim, preserving the LastSeen and ArchiveReason
// values supplied by ArchiveStale.
func (s *Smelter) archiveRules(wtPath string, archived []warden.Rule, summary []warden.MergeResult, stale []warden.ArchivedRule) error {
	// Build map: originalID -> mergedID for the Pass 1 entries.
	supersededBy := make(map[string]string, len(archived))
	for _, m := range summary {
		for _, id := range m.ReplacedIDs {
			supersededBy[id] = m.Merged.ID
		}
	}

	archivePath := warden.ArchivePath(wtPath)
	archive, err := warden.LoadArchive(archivePath)
	if err != nil {
		return fmt.Errorf("loading archive: %w", err)
	}
	for _, r := range archived {
		archive.Add(r, warden.ArchiveReasonDuplicate, supersededBy[r.ID])
	}
	for _, ar := range stale {
		archive.AddArchived(ar)
	}
	return archive.Save(archivePath)
}

// commitAndPush stages the rules file, commits, and force-pushes from the
// worktree. The worktree is already on the correct branch. The commit
// message is rendered from the aggregate pass outcomes via
// buildCommitMessage so all three passes (added/consolidated from Pass 1,
// archived from Pass 2, backfilled from Pass 3) land in one commit on the
// same PR — one PR per anvil per smelter run.
func (s *Smelter) commitAndPush(ctx context.Context, wtPath, branch string, passes PassResults) error {
	gitEnv := cleanGitEnv()
	git := func(args ...string) error {
		cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
		cmd.Dir = wtPath
		cmd.Env = gitEnv
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
		}
		return nil
	}

	// Stage the rules file and the archive file (when present — the
	// archive may have been written by the consolidation pass).
	if err := git("add", warden.RulesFileName); err != nil {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(wtPath, warden.ArchiveFileName)); statErr == nil {
		if err := git("add", warden.ArchiveFileName); err != nil {
			return err
		}
	}

	commitMsg := buildCommitMessage(passes)
	if err := git("commit", "-m", commitMsg); err != nil {
		return err
	}

	// Fetch the specific batch branch so --force-with-lease has a remote-tracking
	// ref to compare against. On a fresh worktree (first run or after worktree
	// removal) the local branch has no upstream configured, so --force-with-lease
	// would treat the expected remote state as non-existent and reject the push
	// if the branch already exists on origin. Fetching first populates
	// refs/remotes/origin/<branch> so git can verify the lease correctly.
	// If the branch does not exist on origin yet, the fetch fails with
	// "couldn't find remote ref" — that is expected on first push and we proceed.
	// Any other fetch error (auth, network, bad remote) is returned immediately
	// so callers get clear diagnostics rather than a confusing push failure.
	{
		cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		fetchCmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "fetch", "origin", branch))
		fetchCmd.Dir = wtPath
		fetchCmd.Env = gitEnv
		var fetchStderr bytes.Buffer
		fetchCmd.Stderr = &fetchStderr
		if err := fetchCmd.Run(); err != nil {
			stderrStr := fetchStderr.String()
			if strings.Contains(stderrStr, "couldn't find remote ref") {
				// Branch doesn't exist on origin — this happens on first push OR after
				// GitHub auto-deletes the branch when a batch PR is merged. In both cases
				// the local remote-tracking ref may be stale (pointing to the old merged
				// commit). Delete it so --force-with-lease doesn't use it as the expected
				// remote state (which would cause a "(stale info)" rejection).
				pruneCmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "update-ref", "-d", "refs/remotes/origin/"+branch))
				pruneCmd.Dir = wtPath
				pruneCmd.Env = gitEnv
				_ = pruneCmd.Run() // best effort; ignore error if ref doesn't exist
				log.Printf("[smelter] batch branch %s not on origin (new or auto-deleted); cleared stale tracking ref", branch)
			} else {
				return fmt.Errorf("git fetch origin %s: %w\nstderr: %s", branch, err, stderrStr)
			}
		}
	}

	// Force-push to the batch branch.
	if err := git("push", "--force-with-lease", "origin", branch); err != nil {
		return err
	}

	return nil
}

// ensurePR creates a PR for the batch branch if one doesn't already exist.
// If a previous PR was merged or closed, creates a new one.
func (s *Smelter) ensurePR(ctx context.Context, wtPath, anvilName, branch string, ruleCount int) error {
	// Check for an existing open PR on the batch branch.
	prNumber, prState, err := s.findBatchPR(ctx, wtPath, branch)
	if err != nil {
		log.Printf("[smelter] Warning: could not check for existing PR on %s: %v", anvilName, err)
		// Fall through to create — gh pr create will error if one already exists.
	}

	if prState == "OPEN" {
		// PR already exists and is open — it was just force-pushed, so it's updated.
		// Also update the body so the rule count reflects the current push.
		body := fmt.Sprintf("Automated batch update of warden rules.\n\n"+
			"This PR adds %d new rule(s) learned from Copilot review comments.\n\n"+
			"Generated by the Forge Smelter.", ruleCount)
		editCtx, editCancel := context.WithTimeout(ctx, 60*time.Second)
		defer editCancel()
		editCmd := executil.HideWindow(exec.CommandContext(editCtx, "gh", "pr", "edit",
			fmt.Sprintf("%d", prNumber),
			"--body", body,
		))
		editCmd.Dir = wtPath
		var editStderr bytes.Buffer
		editCmd.Stderr = &editStderr
		if err := editCmd.Run(); err != nil {
			log.Printf("[smelter] Warning: could not update PR #%d body: %v\nstderr: %s", prNumber, err, editStderr.String())
		}
		log.Printf("[smelter] Existing open PR #%d for %s updated via force-push (%d rule(s))", prNumber, anvilName, ruleCount)
		return nil
	}

	// No open PR (merged/closed/doesn't exist) — create a new one.
	body := fmt.Sprintf("Automated batch update of warden rules.\n\n"+
		"This PR adds %d new rule(s) learned from Copilot review comments.\n\n"+
		"Generated by the Forge Smelter.", ruleCount)

	ghCtx, ghCancel := context.WithTimeout(ctx, 60*time.Second)
	defer ghCancel()
	cmd := executil.HideWindow(exec.CommandContext(ghCtx, "gh", "pr", "create",
		"--title", prTitle,
		"--body", body,
		"--base", "main",
		"--head", branch,
	))
	cmd.Dir = wtPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrStr := stderr.String()
		if strings.Contains(stderrStr, "already exists") {
			log.Printf("[smelter] PR already exists for %s on branch %s", anvilName, branch)
			return nil
		}
		return fmt.Errorf("gh pr create failed: %w\nstderr: %s", err, stderrStr)
	}

	prURL := strings.TrimSpace(stdout.String())
	log.Printf("[smelter] Created PR for %s: %s", anvilName, prURL)
	return nil
}

// findBatchPR looks for an existing PR on the batch branch and returns its
// number and state ("OPEN", "MERGED", "CLOSED"). Returns (0, "", nil) if
// no PR exists for the branch.
func (s *Smelter) findBatchPR(ctx context.Context, wtPath, branch string) (int, string, error) {
	ghCtx, ghCancel := context.WithTimeout(ctx, 60*time.Second)
	defer ghCancel()
	cmd := executil.HideWindow(exec.CommandContext(ghCtx, "gh", "pr", "list",
		"--head", branch,
		"--state", "all",
		"--json", "number,state",
		"--limit", "1",
	))
	cmd.Dir = wtPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return 0, "", fmt.Errorf("gh pr list: %w\nstderr: %s", err, stderr.String())
	}

	var prs []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return 0, "", fmt.Errorf("parsing pr list: %w", err)
	}

	if len(prs) == 0 {
		return 0, "", nil
	}

	return prs[0].Number, strings.ToUpper(prs[0].State), nil
}
