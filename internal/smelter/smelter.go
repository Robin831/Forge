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
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
	"gopkg.in/yaml.v3"
)

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
}

// New creates a Smelter. interval controls how often Flush is called;
// pass 0 to disable scheduled runs (Flush can still be called directly).
func New(db *state.DB, interval time.Duration, anvilPaths map[string]string) *Smelter {
	return &Smelter{
		db:         db,
		wtMgr:      worktree.NewManager(),
		interval:   interval,
		anvilPaths: anvilPaths,
		intervalCh: make(chan time.Duration, 1),
	}
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

// Run starts the periodic flush loop. Blocks until ctx is canceled.
// If interval <= 0 at startup, scheduled flushes are paused until
// UpdateInterval is called with a positive value.
func (s *Smelter) Run(ctx context.Context) error {
	log.Printf("[smelter] Starting smelter (interval: %s)", s.interval)
	_ = s.db.LogEvent(state.EventSmelterStarted,
		fmt.Sprintf("Smelter started (interval: %s)", s.interval), "", "")

	// Initial flush.
	if err := s.Flush(ctx); err != nil {
		log.Printf("[smelter] Initial flush error: %v", err)
	}

	// Create a ticker. If the initial interval is <= 0, use a placeholder
	// duration and stop the ticker immediately so scheduled flushes are paused
	// until a positive interval arrives via UpdateInterval.
	startInterval := s.interval
	if startInterval <= 0 {
		startInterval = time.Hour // placeholder; stopped immediately below
	}
	ticker := time.NewTicker(startInterval)
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
			if newInterval > 0 {
				log.Printf("[smelter] Interval changed to %s; resetting ticker", newInterval)
				ticker.Reset(newInterval)
			} else {
				log.Printf("[smelter] Interval set to <= 0; pausing ticker")
				ticker.Stop()
			}
		case <-ticker.C:
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
		return nil // no-op: nothing pending
	}

	s.mu.RLock()
	anvilPaths := make(map[string]string, len(s.anvilPaths))
	maps.Copy(anvilPaths, s.anvilPaths)
	s.mu.RUnlock()

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
			log.Printf("[smelter] Error flushing anvil %s: %v", anvilName, err)
			_ = s.db.LogEvent(state.EventSmelterFailed,
				fmt.Sprintf("Smelter flush failed for %s: %v", anvilName, err), "", anvilName)
			continue
		}
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

	var added int
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
			added++
		}
		flushedIDs = append(flushedIDs, pr.ID)
	}

	if added == 0 {
		// All rules were duplicates (already in the file) or malformed. Delete from DB and emit a flush event.
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

	// 3. Save the updated rules file in the worktree.
	if err := warden.SaveRules(wt.Path, rf); err != nil {
		return fmt.Errorf("saving warden rules for %s: %w", anvilName, err)
	}

	// 4. Commit and force-push from the worktree.
	if err := s.commitAndPush(ctx, wt.Path, branch, added); err != nil {
		return fmt.Errorf("commit/push for %s: %w", anvilName, err)
	}

	// 5. Create or update the PR.
	if err := s.ensurePR(ctx, wt.Path, anvilName, branch, added); err != nil {
		return fmt.Errorf("PR creation for %s: %w", anvilName, err)
	}

	// 6. Delete flushed rules from the DB.
	if err := s.db.DeletePendingRules(flushedIDs); err != nil {
		return fmt.Errorf("deleting flushed rules for %s: %w", anvilName, err)
	}

	msg := fmt.Sprintf("Flushed %d new warden rule(s) for %s (%d total pending processed)", added, anvilName, len(rules))
	log.Printf("[smelter] %s", msg)
	_ = s.db.LogEvent(state.EventSmelterFlushed, msg, "", anvilName)

	return nil
}

// commitAndPush stages the rules file, commits, and force-pushes from the
// worktree. The worktree is already on the correct branch.
func (s *Smelter) commitAndPush(ctx context.Context, wtPath, branch string, ruleCount int) error {
	git := func(args ...string) error {
		cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
		cmd.Dir = wtPath
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
		}
		return nil
	}

	// Stage the rules file.
	if err := git("add", warden.RulesFileName); err != nil {
		return err
	}

	// Commit with the specified message format.
	commitMsg := fmt.Sprintf("forge: learn %d warden rule(s) from pending queue [no-changelog]", ruleCount)
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
	// "couldn't find remote ref" — that is fine and the push proceeds normally.
	if err := git("fetch", "origin", branch); err != nil {
		log.Printf("[smelter] pre-push fetch of %s returned error (first push or branch absent on origin): %v", branch, err)
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
		log.Printf("[smelter] Existing open PR #%d for %s updated via force-push", prNumber, anvilName)
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
