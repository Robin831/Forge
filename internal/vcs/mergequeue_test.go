package vcs

import (
	"context"
	"testing"
	"time"
)

func TestResolveQueued(t *testing.T) {
	queuedAt := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	late := queuedAt.Add(MergeQueueSettleGrace)
	early := queuedAt.Add(MergeQueueSettleGrace - time.Second)
	for _, tc := range []struct {
		name  string
		state string
		info  MergeQueueInfo
		now   time.Time
		want  QueueResolution
	}{
		{"merged", "MERGED", MergeQueueInfo{}, late, QueueMerged},
		{"closed", "CLOSED", MergeQueueInfo{}, late, QueueClosed},
		{"in queue", "OPEN", MergeQueueInfo{InQueue: true}, late, QueueWaiting},
		{"auto-merge waiting for checks", "OPEN", MergeQueueInfo{AutoMergeEnabled: true}, late, QueueWaiting},
		{"neither, inside the settle grace", "OPEN", MergeQueueInfo{}, early, QueueWaiting},
		{"neither, after the grace: dequeued", "OPEN", MergeQueueInfo{}, late, QueueDequeued},
	} {
		if got := ResolveQueued(tc.state, tc.info, queuedAt, tc.now); got != tc.want {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, got)
		}
	}
}

// A provider without queue support keeps MergePR's contract: nil = merged.
func TestMerge_PlainProviderIsMerged(t *testing.T) {
	var p plainMerger
	outcome, err := Merge(t.Context(), &p, MergeRequest{PRNumber: 3, Strategy: "squash"})
	if err != nil || outcome != MergeOutcomeMerged || p.calls != 1 {
		t.Fatalf("got %v/%v after %d calls", outcome, err, p.calls)
	}
}

type plainMerger struct {
	Provider
	calls int
}

func (p *plainMerger) MergePR(_ context.Context, _ string, _ int, _ string) error {
	p.calls++
	return nil
}
