package bellows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
)

// mqGitHub is a fake gh behind the real GitHub provider: one repo whose main
// branch has a merge queue, one PR whose queue state the test steers.
type mqGitHub struct {
	mu        sync.Mutex
	prState   string
	inQueue   bool
	autoMerge bool
	calls     [][]string
}

func (f *mqGitHub) run(_ context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "git" {
		return []byte("https://github.com/FHIDev/Munin.git\n"), nil, nil
	}
	var q string
	for _, a := range args {
		if strings.HasPrefix(a, "query=") {
			q = a
		}
	}
	auto := "null"
	if f.autoMerge {
		auto = `{"enabledAt":"2026-10-07T10:00:00Z"}`
	}
	switch {
	case args[0] == "pr" && args[1] == "merge":
		f.inQueue = true
		return nil, nil, nil
	case args[0] == "run" && args[1] == "list":
		return []byte(`[{"headBranch":"gh-readonly-queue/main/pr-50-abc","conclusion":"failure","workflowName":"CI","url":"https://github.com/FHIDev/Munin/actions/runs/9"}]`), nil, nil
	case strings.Contains(q, "mergeQueue(branch:"):
		return []byte(`{"data":{"repository":{"mergeQueue":{"id":"MQ"}}}}`), nil, nil
	case strings.Contains(q, "fragment prStatus"):
		node := fmt.Sprintf(`{"number":50,"state":%q,"mergeable":"MERGEABLE","headRefName":"forge/Bead-mq","headRefOid":"sha1","isDraft":false,"url":"https://github.com/FHIDev/Munin/pull/50","title":"t",
			"isInMergeQueue":%v,"autoMergeRequest":%s,"mergeQueueEntry":null,
			"statusCheckRollup":{"nodes":[]},"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[]},
			"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]},"reviewRequests":{"nodes":[]}}`, f.prState, f.inQueue, auto)
		return []byte("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n" + `{"data":{"repository":{"pr50":` + node + `}}}`), nil, nil
	case strings.Contains(q, "isInMergeQueue"):
		return []byte(fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"state":%q,"isInMergeQueue":%v,"autoMergeRequest":%s,"mergeQueueEntry":null}}}}`,
			f.prState, f.inQueue, auto)), nil, nil
	}
	return nil, []byte("unexpected call"), errors.New("exit status 1")
}

func (f *mqGitHub) set(state string, inQueue, auto bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prState, f.inQueue, f.autoMerge = state, inQueue, auto
}

func (f *mqGitHub) count(pred func(c []string) bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if pred(c) {
			n++
		}
	}
	return n
}

type eventLog struct {
	mu     sync.Mutex
	events []PREvent
}

func (l *eventLog) handle(_ context.Context, e PREvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) types() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		out = append(out, e.EventType)
	}
	return out
}

type mqFixture struct {
	db    *state.DB
	gh    *mqGitHub
	prov  *github.Provider
	pr    *state.PR
	clock time.Time
}

// newMQFixture enqueues PR #50 through the real provider and records it the
// way the daemon's recordMergeQueued does.
func newMQFixture(t *testing.T) *mqFixture {
	t.Helper()
	db, cleanup := openTempDB(t)
	t.Cleanup(cleanup)
	f := &mqFixture{db: db, gh: &mqGitHub{prState: "OPEN"}, clock: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)}
	f.prov = github.NewWithRunner(nil, f.gh.run, func() time.Time { return f.clock })
	f.pr = &state.PR{Number: 50, Anvil: "munin", BeadID: "Bead-mq", Branch: "forge/Bead-mq", BaseBranch: "main", Status: state.PROpen, CreatedAt: f.clock}
	require.NoError(t, db.InsertPR(f.pr))

	outcome, err := f.prov.MergePRWithOutcome(context.Background(), vcs.MergeRequest{WorktreePath: "/munin", PRNumber: 50, Base: "main", Strategy: "squash"})
	require.NoError(t, err)
	require.Equal(t, vcs.MergeOutcomeQueued, outcome, "a queue repo is enqueued, never reported merged")
	require.NoError(t, db.SetPRMergeQueued(f.pr.ID, f.clock))
	return f
}

// monitor builds a fresh Monitor, as a daemon (re)start does.
func (f *mqFixture) monitor(log *eventLog) *Monitor {
	m := New(f.db, func(string) vcs.Provider { return f.prov }, time.Minute, map[string]string{"munin": "/munin"}, nil, nil, nil, nil)
	m.now = func() time.Time { return f.clock }
	m.retryBackoff = &github.RetryBackoff{}
	m.OnEvent(log.handle)
	return m
}

func (f *mqFixture) row(t *testing.T) *state.PR {
	t.Helper()
	pr, err := f.db.GetPRByID(f.pr.ID)
	require.NoError(t, err)
	return pr
}

func (f *mqFixture) mergeFailedEvents(t *testing.T) []state.Event {
	t.Helper()
	evs, err := f.db.EventsByBead("Bead-mq", "munin", 50)
	require.NoError(t, err)
	var out []state.Event
	for _, e := range evs {
		if e.Type == state.EventPRMergeFailed {
			out = append(out, e)
		}
	}
	return out
}

func isBatchCall(c []string) bool {
	return len(c) > 1 && c[0] == "gh" && strings.Contains(strings.Join(c, " "), "fragment prStatus")
}

// enqueue → pending → merged: the PR is not merged while queued, and the
// normal merged path (EventPRMerged, which closes the bead) runs once it is.
func TestMergeQueue_EnqueuePendingMerged(t *testing.T) {
	f := newMQFixture(t)
	log := &eventLog{}
	m := f.monitor(log)

	f.clock = f.clock.Add(5 * time.Minute)
	m.checkAll(context.Background())
	assert.NotContains(t, log.types(), EventPRMerged, "a queued PR is not merged")
	assert.NotContains(t, log.types(), EventPRMergeDequeued)
	require.NotNil(t, f.row(t).MergeQueuedAt, "still queued")
	assert.Equal(t, state.PROpen, f.row(t).Status)

	f.gh.set("MERGED", false, false)
	f.clock = f.clock.Add(5 * time.Minute)
	m.checkAll(context.Background())
	assert.Contains(t, log.types(), EventPRMerged)
	assert.NotContains(t, log.types(), EventPRMergeDequeued)
	assert.Equal(t, state.PRMerged, f.row(t).Status)
	assert.Empty(t, f.mergeFailedEvents(t))
	assert.Equal(t, 2, f.gh.count(isBatchCall), "each poll is the one batched query, queue state included")
	assert.Equal(t, 1, f.gh.count(func(c []string) bool {
		return !isBatchCall(c) && strings.Contains(strings.Join(c, " "), "isInMergeQueue")
	}), "only the enqueue's own read-back reads queue state outside the batch")
}

// enqueue → pending → dequeued: a failed merge. No merged event, the bead
// stays open, the reason (the merge_group run) is surfaced as pr_merge_failed.
func TestMergeQueue_EnqueuePendingDequeued(t *testing.T) {
	f := newMQFixture(t)
	log := &eventLog{}
	m := f.monitor(log)

	f.clock = f.clock.Add(time.Minute)
	m.checkAll(context.Background())
	require.NotNil(t, f.row(t).MergeQueuedAt)

	f.gh.set("OPEN", false, false)
	f.clock = f.clock.Add(5 * time.Minute)
	m.checkAll(context.Background())

	assert.Contains(t, log.types(), EventPRMergeDequeued)
	assert.NotContains(t, log.types(), EventPRMerged)
	row := f.row(t)
	assert.Nil(t, row.MergeQueuedAt, "the queue entry is resolved")
	assert.NotEqual(t, state.PRMerged, row.Status)
	failed := f.mergeFailedEvents(t)
	require.Len(t, failed, 1)
	assert.Contains(t, failed[0].Message, "removed from merge queue")
	assert.Contains(t, failed[0].Message, "actions/runs/9", "the merge_group run failure is surfaced")

	// Resolved once: a later poll does not fail it again.
	m.checkAll(context.Background())
	assert.Len(t, f.mergeFailedEvents(t), 1)
}

// Right after the enqueue GitHub may show neither a queue entry nor
// auto-merge; inside the settle grace that is not a dequeue.
func TestMergeQueue_SettleGraceIsNotDequeue(t *testing.T) {
	f := newMQFixture(t)
	f.gh.set("OPEN", false, false)
	log := &eventLog{}
	m := f.monitor(log)
	f.clock = f.clock.Add(vcs.MergeQueueSettleGrace / 2)
	m.checkAll(context.Background())
	assert.NotContains(t, log.types(), EventPRMergeDequeued)
	assert.NotNil(t, f.row(t).MergeQueuedAt)
}

// A daemon restart while queued resumes from the prs row: the new monitor
// knows nothing in memory, yet waits, then resolves the PR.
func TestMergeQueue_RestartWhileQueued(t *testing.T) {
	f := newMQFixture(t)

	first := &eventLog{}
	f.clock = f.clock.Add(3 * time.Minute)
	f.monitor(first).checkAll(context.Background())
	assert.Empty(t, first.types())

	// Restart: a fresh Monitor over the same state.db.
	afterRestart := &eventLog{}
	m := f.monitor(afterRestart)
	f.clock = f.clock.Add(3 * time.Minute)
	m.checkAll(context.Background())
	assert.NotContains(t, afterRestart.types(), EventPRMerged)
	assert.NotContains(t, afterRestart.types(), EventPRMergeDequeued, "still queued after the restart")
	require.NotNil(t, f.row(t).MergeQueuedAt)

	f.gh.set("OPEN", false, false)
	f.clock = f.clock.Add(3 * time.Minute)
	m.checkAll(context.Background())
	assert.Contains(t, afterRestart.types(), EventPRMergeDequeued, "the restarted monitor resolves the persisted queue entry")
	assert.NotContains(t, afterRestart.types(), EventPRMerged)
}

// A status from the per-PR fallback has no queue fields; the queued PR then
// costs one extra read instead of being misread as dequeued.
func TestMergeQueue_UnknownQueueStateIsReadNotGuessed(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	pr := &state.PR{Number: 60, Anvil: "a", BeadID: "Bead-u", Branch: "forge/Bead-u", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.SetPRMergeQueued(pr.ID, time.Now().Add(-time.Hour)))

	fake := &queueReadVCS{fakeVCSProvider: fakeVCSProvider{status: &vcs.PRStatus{State: "OPEN"}}, info: vcs.MergeQueueInfo{InQueue: true}}
	log := &eventLog{}
	m := New(db, func(string) vcs.Provider { return fake }, time.Minute, map[string]string{"a": "/a"}, nil, nil, nil, nil)
	m.OnEvent(log.handle)
	m.checkAll(context.Background())
	assert.Equal(t, 1, fake.reads)
	assert.NotContains(t, log.types(), EventPRMergeDequeued)
}

type queueReadVCS struct {
	fakeVCSProvider
	info  vcs.MergeQueueInfo
	reads int
}

func (q *queueReadVCS) MergePRWithOutcome(context.Context, vcs.MergeRequest) (vcs.MergeOutcome, error) {
	return vcs.MergeOutcomeQueued, nil
}

func (q *queueReadVCS) MergeQueueStatus(context.Context, string, int) (string, vcs.MergeQueueInfo, error) {
	q.reads++
	return "OPEN", q.info, nil
}
