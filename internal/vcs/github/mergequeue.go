package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/vcs"
)

// mergeQueueTTL bounds how long one answer to "does this branch require a
// merge queue" is trusted. A queue switched on or off is seen within it.
const mergeQueueTTL = 10 * time.Minute

// mergeQueueCache remembers, per owner/repo@branch, whether the branch has a
// merge queue. Entries expire after mergeQueueTTL; errors are never cached.
type mergeQueueCache struct {
	mu      sync.Mutex
	entries map[string]mergeQueueEntry
}

type mergeQueueEntry struct {
	queue bool
	at    time.Time
}

func newMergeQueueCache() *mergeQueueCache {
	return &mergeQueueCache{entries: make(map[string]mergeQueueEntry)}
}

// sharedMergeQueueCache is used by every New provider: the daemon builds a
// fresh Provider in several places and they should share one answer.
var sharedMergeQueueCache = newMergeQueueCache()

func (c *mergeQueueCache) get(key string, now time.Time) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.Sub(e.at) >= mergeQueueTTL || now.Before(e.at) {
		delete(c.entries, key)
		return false, false
	}
	return e.queue, true
}

func (c *mergeQueueCache) put(key string, queue bool, now time.Time) {
	c.mu.Lock()
	c.entries[key] = mergeQueueEntry{queue: queue, at: now}
	c.mu.Unlock()
}

func (p *Provider) queueCache() *mergeQueueCache {
	if p.mq != nil {
		return p.mq
	}
	return sharedMergeQueueCache
}

// mergeQueueMention matches gh's and GitHub's wording whenever a merge went to,
// or was refused in favour of, a merge queue ("added to the merge queue",
// "set by the merge queue", "already in merge queue", "must be made through
// the merge queue").
var mergeQueueMention = regexp.MustCompile(`(?i)merge queue`)

// branchHasMergeQueue reports whether base requires a merge queue, from
// GraphQL repository.mergeQueue(branch:), which is non-null exactly when the
// branch has one, whether set by a ruleset or by classic branch protection.
func (p *Provider) branchHasMergeQueue(ctx context.Context, dir, owner, repo, base string) (bool, error) {
	key := owner + "/" + repo + "@" + base
	if q, ok := p.queueCache().get(key, p.nowFn()); ok {
		return q, nil
	}
	stdout, stderr, err := p.run(ctx, dir, "gh", "api", "graphql",
		"-f", "query=query($owner:String!, $repo:String!, $branch:String!) { repository(owner:$owner, name:$repo) { mergeQueue(branch:$branch) { id } } }",
		"-f", "owner="+owner,
		"-f", "repo="+repo,
		"-f", "branch="+base,
	)
	if err != nil {
		return false, Classify(fmt.Errorf("gh api graphql (merge queue for %s): %w\nstderr: %s", key, err, stderr))
	}
	var resp struct {
		Data struct {
			Repository *struct {
				MergeQueue *struct {
					ID string `json:"id"`
				} `json:"mergeQueue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &resp); err != nil || resp.Data.Repository == nil {
		return false, fmt.Errorf("parsing merge queue response for %s: %v", key, err)
	}
	queue := resp.Data.Repository.MergeQueue != nil
	p.queueCache().put(key, queue, p.nowFn())
	return queue, nil
}

func (p *Provider) prBaseBranch(ctx context.Context, dir string, prNumber int) (string, error) {
	stdout, stderr, err := p.run(ctx, dir, "gh", "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "baseRefName")
	if err != nil {
		return "", Classify(fmt.Errorf("gh pr view (base branch): %w\nstderr: %s", err, stderr))
	}
	var v struct {
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal(stdout, &v); err != nil || v.BaseRefName == "" {
		return "", fmt.Errorf("parsing base branch of PR #%d: %v", prNumber, err)
	}
	return v.BaseRefName, nil
}

// directMergeArgs is the argv every non-queue merge has always used.
func directMergeArgs(prNumber int, strategy string) []string {
	return []string{"pr", "merge", fmt.Sprintf("%d", prNumber), "--" + strategy, "--delete-branch=false"}
}

// queueMergeArgs enqueues: the queue sets the strategy, so none is passed, and
// --auto makes gh enqueue once required checks pass. Never --admin.
func queueMergeArgs(prNumber int) []string {
	return []string{"pr", "merge", fmt.Sprintf("%d", prNumber), "--auto"}
}

// MergePRWithOutcome merges req's PR, deciding from GitHub at merge time
// whether its base branch uses a merge queue. A non-queue branch is merged
// directly and nil means merged, as before. A queue branch is enqueued and the
// outcome is read back from the PR, never from gh's exit code.
func (p *Provider) MergePRWithOutcome(ctx context.Context, req vcs.MergeRequest) (vcs.MergeOutcome, error) {
	strategy := normalizeStrategy(req.Strategy)
	owner, repo, err := p.GetRepoOwnerAndName(ctx, req.WorktreePath)
	if err != nil {
		return vcs.MergeOutcomeMerged, err
	}
	base := req.Base
	if base == "" {
		if base, err = p.prBaseBranch(ctx, req.WorktreePath, req.PRNumber); err != nil {
			return vcs.MergeOutcomeMerged, err
		}
	}
	queue, err := p.branchHasMergeQueue(ctx, req.WorktreePath, owner, repo, base)
	if err != nil {
		return vcs.MergeOutcomeMerged, err
	}
	if queue {
		return p.enqueue(ctx, req.WorktreePath, req.PRNumber)
	}

	log.Printf("[vcs/github] Merging PR #%d with strategy %s", req.PRNumber, strategy)
	stdout, stderr, err := p.run(ctx, req.WorktreePath, "gh", directMergeArgs(req.PRNumber, strategy)...)
	if err != nil {
		if mergeQueueMention.Match(stderr) {
			// The branch gained a queue inside the TTL: GitHub refused the
			// direct merge. Re-learn it and go through the queue.
			log.Printf("[vcs/github] Direct merge of PR #%d refused for a merge queue; enqueueing", req.PRNumber)
			p.queueCache().put(owner+"/"+repo+"@"+base, true, p.nowFn())
			return p.enqueue(ctx, req.WorktreePath, req.PRNumber)
		}
		return vcs.MergeOutcomeMerged, fmt.Errorf("gh pr merge failed: %w\nstderr: %s", err, stderr)
	}
	if mergeQueueMention.Match(stdout) || mergeQueueMention.Match(stderr) {
		// gh itself enqueued (it does so for a queue branch even when given
		// --squash). Same stale-cache case; the PR state decides.
		p.queueCache().put(owner+"/"+repo+"@"+base, true, p.nowFn())
		return p.readBackQueued(ctx, req.WorktreePath, req.PRNumber)
	}
	log.Printf("[vcs/github] Merged PR #%d", req.PRNumber)
	return vcs.MergeOutcomeMerged, nil
}

func (p *Provider) enqueue(ctx context.Context, dir string, prNumber int) (vcs.MergeOutcome, error) {
	log.Printf("[vcs/github] PR #%d targets a merge-queue branch; enqueueing", prNumber)
	_, stderr, err := p.run(ctx, dir, "gh", queueMergeArgs(prNumber)...)
	if err != nil {
		msg := string(stderr)
		if !strings.Contains(strings.ToLower(msg), "already in merge queue") {
			return vcs.MergeOutcomeQueued, fmt.Errorf("gh pr merge --auto failed: %w\nstderr: %s", err, msg)
		}
	}
	return p.readBackQueued(ctx, dir, prNumber)
}

// readBackQueued reads the PR after an enqueue. Only a MERGED state is merged;
// an unreadable state stays queued for Bellows to resolve.
func (p *Provider) readBackQueued(ctx context.Context, dir string, prNumber int) (vcs.MergeOutcome, error) {
	state, info, err := p.MergeQueueStatus(ctx, dir, prNumber)
	switch {
	case err != nil:
		log.Printf("[vcs/github] PR #%d enqueued; could not read its queue state yet: %v", prNumber, err)
		return vcs.MergeOutcomeQueued, nil
	case state == "MERGED":
		log.Printf("[vcs/github] Merged PR #%d (through the merge queue)", prNumber)
		return vcs.MergeOutcomeMerged, nil
	case state == "CLOSED":
		return vcs.MergeOutcomeQueued, fmt.Errorf("PR #%d is closed; it was not merged", prNumber)
	}
	log.Printf("[vcs/github] PR #%d queued (in queue: %v, auto-merge: %v, entry: %q)",
		prNumber, info.InQueue, info.AutoMergeEnabled, info.EntryState)
	return vcs.MergeOutcomeQueued, nil
}

// mergeQueueFields is the PullRequest selection both the single read and the
// batched poll use for queue state.
const mergeQueueFields = `isInMergeQueue autoMergeRequest { enabledAt } mergeQueueEntry { state }`

type mergeQueueNode struct {
	IsInMergeQueue   *bool `json:"isInMergeQueue"`
	AutoMergeRequest *struct {
		EnabledAt string `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	MergeQueueEntry *struct {
		State string `json:"state"`
	} `json:"mergeQueueEntry"`
}

// info is nil when the response did not select the queue fields.
func (n mergeQueueNode) info() *vcs.MergeQueueInfo {
	if n.IsInMergeQueue == nil {
		return nil
	}
	i := &vcs.MergeQueueInfo{InQueue: *n.IsInMergeQueue, AutoMergeEnabled: n.AutoMergeRequest != nil}
	if n.MergeQueueEntry != nil {
		i.EntryState = n.MergeQueueEntry.State
	}
	return i
}

// MergeQueueStatus reads a PR's state and merge-queue fields in one query.
func (p *Provider) MergeQueueStatus(ctx context.Context, dir string, prNumber int) (string, vcs.MergeQueueInfo, error) {
	owner, repo, err := p.GetRepoOwnerAndName(ctx, dir)
	if err != nil {
		return "", vcs.MergeQueueInfo{}, err
	}
	stdout, stderr, err := p.run(ctx, dir, "gh", "api", "graphql",
		"-f", "query=query($owner:String!, $repo:String!, $pr:Int!) { repository(owner:$owner, name:$repo) { pullRequest(number:$pr) { state "+mergeQueueFields+" } } }",
		"-f", "owner="+owner,
		"-f", "repo="+repo,
		"-F", fmt.Sprintf("pr=%d", prNumber),
	)
	if err != nil {
		return "", vcs.MergeQueueInfo{}, Classify(fmt.Errorf("gh api graphql (merge queue state of PR #%d): %w\nstderr: %s", prNumber, err, stderr))
	}
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					State string `json:"state"`
					mergeQueueNode
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return "", vcs.MergeQueueInfo{}, fmt.Errorf("parsing merge queue state of PR #%d: %w", prNumber, err)
	}
	pr := resp.Data.Repository.PullRequest
	if pr == nil {
		return "", vcs.MergeQueueInfo{}, errors.New("merge queue state: PR not found")
	}
	var info vcs.MergeQueueInfo
	if i := pr.info(); i != nil {
		info = *i
	}
	return pr.State, info, nil
}

func normalizeStrategy(strategy string) string {
	switch strategy {
	case "":
		return "squash"
	case "squash", "merge", "rebase":
		return strategy
	}
	log.Printf("[vcs/github] Invalid merge strategy %q, defaulting to squash", strategy)
	return "squash"
}

// MergeGroupFailure returns a one-line description of the latest failed
// merge_group run for the PR, or "" when none is found. One request; called
// only when a queued PR has been dequeued.
func (p *Provider) MergeGroupFailure(ctx context.Context, dir string, prNumber int) (string, error) {
	stdout, stderr, err := p.run(ctx, dir, "gh", "run", "list",
		"--event", "merge_group", "--limit", "30",
		"--json", "headBranch,conclusion,workflowName,url")
	if err != nil {
		return "", fmt.Errorf("gh run list (merge_group): %w\nstderr: %s", err, stderr)
	}
	var runs []struct {
		HeadBranch   string `json:"headBranch"`
		Conclusion   string `json:"conclusion"`
		WorkflowName string `json:"workflowName"`
		URL          string `json:"url"`
	}
	if err := json.Unmarshal(stdout, &runs); err != nil {
		return "", fmt.Errorf("parsing merge_group runs: %w", err)
	}
	// Queue branches are gh-readonly-queue/<base>/pr-<N>-<sha>; runs are newest first.
	marker := fmt.Sprintf("/pr-%d-", prNumber)
	for _, r := range runs {
		if !strings.Contains(r.HeadBranch, marker) {
			continue
		}
		switch r.Conclusion {
		case "failure", "cancelled", "timed_out", "startup_failure", "action_required":
			return fmt.Sprintf("merge_group run %q %s: %s", r.WorkflowName, r.Conclusion, r.URL), nil
		}
	}
	return "", nil
}
