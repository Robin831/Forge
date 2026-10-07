package crucible

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/vcs"
)

// queueProvider is a queue-aware provider whose PR is enqueued on merge and
// then reports the states in script, one per poll.
type queueProvider struct {
	vcs.Provider
	mu     sync.Mutex
	merges int
	reads  int
	script []string // "queued", "neither", "merged"
}

func (q *queueProvider) MergePRWithOutcome(context.Context, vcs.MergeRequest) (vcs.MergeOutcome, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.merges++
	return vcs.MergeOutcomeQueued, nil
}

func (q *queueProvider) MergeQueueStatus(context.Context, string, int) (string, vcs.MergeQueueInfo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.script[min(q.reads, len(q.script)-1)]
	q.reads++
	switch s {
	case "merged":
		return "MERGED", vcs.MergeQueueInfo{}, nil
	case "queued":
		return "OPEN", vcs.MergeQueueInfo{InQueue: true}, nil
	}
	return "OPEN", vcs.MergeQueueInfo{}, nil
}

func withFastMergePolls(t *testing.T) {
	t.Helper()
	oldInterval, oldClock := defaultMergePollInterval, mergeClock
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	defaultMergePollInterval = time.Millisecond
	mergeClock = func() time.Time { now = now.Add(time.Minute); return now }
	t.Cleanup(func() { defaultMergePollInterval, mergeClock = oldInterval, oldClock })
}

// A crucible child on a queue branch is waited on until it merges; it is
// enqueued once, not re-merged on every poll.
func TestMergePRWithProvider_QueuedThenMerged(t *testing.T) {
	withFastMergePolls(t)
	q := &queueProvider{script: []string{"queued", "queued", "merged"}}
	if err := MergePRWithProvider(context.Background(), q, 5, "/dir"); err != nil {
		t.Fatalf("want merged, got %v", err)
	}
	if q.merges != 1 || q.reads != 3 {
		t.Fatalf("want 1 enqueue and 3 reads, got %d and %d", q.merges, q.reads)
	}
}

// A child the queue removes is a failed merge, not a success.
func TestMergePRWithProvider_QueuedThenDequeued(t *testing.T) {
	withFastMergePolls(t)
	q := &queueProvider{script: []string{"queued", "neither"}}
	err := MergePRWithProvider(context.Background(), q, 5, "/dir")
	if err == nil || !strings.Contains(err.Error(), "removed from the merge queue") {
		t.Fatalf("want a dequeue error, got %v", err)
	}
}
