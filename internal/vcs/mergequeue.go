package vcs

import (
	"context"
	"errors"
	"time"
)

// MergeOutcome is what a merge call achieved. On a branch that requires a
// merge queue, `gh pr merge` exits 0 having only enqueued the PR (or only
// enabled auto-merge), so a nil error alone does not mean merged.
type MergeOutcome int

const (
	// MergeOutcomeMerged: the PR is merged; run the post-merge path.
	MergeOutcomeMerged MergeOutcome = iota
	// MergeOutcomeQueued: the PR is in (or waiting for) the merge queue. It is
	// NOT merged: no bead close, no "merged" event. Bellows resolves it later.
	MergeOutcomeQueued
)

func (o MergeOutcome) String() string {
	if o == MergeOutcomeQueued {
		return "queued"
	}
	return "merged"
}

// ErrMergeQueued is returned by a queue-aware provider's plain MergePR when the
// PR was only enqueued, so a caller that reads nil as "merged" cannot do so.
var ErrMergeQueued = errors.New("pull request was added to the merge queue; it is not merged yet")

// MergeRequest names one merge. Base is the PR's target branch; empty makes
// the provider look it up.
type MergeRequest struct {
	WorktreePath string
	PRNumber     int
	Base         string
	Strategy     string
}

// MergeQueueInfo is a PR's merge-queue state as GitHub reports it.
type MergeQueueInfo struct {
	InQueue          bool   // PullRequest.isInMergeQueue
	AutoMergeEnabled bool   // PullRequest.autoMergeRequest != null
	EntryState       string // PullRequest.mergeQueueEntry.state, "" when not queued
}

// QueueAwareMerger is implemented by providers that detect a merge queue on
// the base branch at merge time and report whether the PR merged or queued.
type QueueAwareMerger interface {
	MergePRWithOutcome(ctx context.Context, req MergeRequest) (MergeOutcome, error)
	// MergeQueueStatus reads the PR's state and merge-queue fields.
	MergeQueueStatus(ctx context.Context, worktreePath string, prNumber int) (state string, info MergeQueueInfo, err error)
}

// Merge merges through the queue-aware path when the provider has one, and
// through plain MergePR (nil = merged, today's contract) otherwise.
func Merge(ctx context.Context, p Provider, req MergeRequest) (MergeOutcome, error) {
	if q, ok := p.(QueueAwareMerger); ok {
		return q.MergePRWithOutcome(ctx, req)
	}
	return MergeOutcomeMerged, p.MergePR(ctx, req.WorktreePath, req.PRNumber, req.Strategy)
}

// QueueResolution is what a queued PR has become since it was enqueued.
type QueueResolution int

const (
	QueueWaiting QueueResolution = iota
	QueueMerged
	QueueDequeued
	QueueClosed
)

// MergeQueueSettleGrace is how long a freshly enqueued PR may show neither a
// queue entry nor auto-merge before it is read as dequeued. GitHub can report
// the enqueue a moment after gh returns.
const MergeQueueSettleGrace = 90 * time.Second

// ResolveQueued classifies a queued PR from its current state. queuedAt is
// when the Forge enqueued it; now is the caller's clock.
func ResolveQueued(state string, info MergeQueueInfo, queuedAt, now time.Time) QueueResolution {
	switch state {
	case "MERGED":
		return QueueMerged
	case "CLOSED":
		return QueueClosed
	}
	if info.InQueue || info.AutoMergeEnabled {
		return QueueWaiting
	}
	if now.Sub(queuedAt) < MergeQueueSettleGrace {
		return QueueWaiting
	}
	return QueueDequeued
}

// MergeGroupFailureReporter describes why a dequeued PR left the queue, from
// its latest failed merge_group CI run. "" means nothing was found.
type MergeGroupFailureReporter interface {
	MergeGroupFailure(ctx context.Context, worktreePath string, prNumber int) (string, error)
}
