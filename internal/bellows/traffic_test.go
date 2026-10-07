package bellows

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trafficVCS records every status call bellows makes, per PR and per batch.
type trafficVCS struct {
	fakeVCSProvider
	mu           sync.Mutex
	checked      []int
	batches      [][]int
	batchErr     error
	batchOmit    map[int]bool
	checkErr     error
	supportBatch bool
}

func (f *trafficVCS) CheckStatus(_ context.Context, _ string, n int) (*vcs.PRStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, n)
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	return &vcs.PRStatus{State: "OPEN"}, nil
}

// batchingVCS is trafficVCS plus vcs.BatchStatusChecker.
type batchingVCS struct{ *trafficVCS }

func (f batchingVCS) CheckStatusBatch(_ context.Context, _ string, nums []int) (map[int]*vcs.PRStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, append([]int(nil), nums...))
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[int]*vcs.PRStatus)
	for _, n := range nums {
		if !f.batchOmit[n] {
			out[n] = &vcs.PRStatus{State: "OPEN"}
		}
	}
	return out, nil
}

// trafficScenario seeds the measurement scenario: 3 repos, 7 open PRs — 2 this
// forge's own (forge/<bead>, managed), 2 a sibling forge's (forge/<bead> too,
// tracked as ext-* by reconcile because they carry the sibling's marker), and
// 3 humans'. Returns the numbers of the two owned PRs.
func trafficScenario(t *testing.T, db *state.DB) []int {
	t.Helper()
	rows := []state.PR{
		{Number: 101, Anvil: "repo-a", BeadID: "Forge-own1", Branch: "forge/Forge-own1"},
		{Number: 201, Anvil: "repo-b", BeadID: "Forge-own2", Branch: "forge/Forge-own2"},
		{Number: 102, Anvil: "repo-a", BeadID: "ext-102", Branch: "forge/Sib-1"},
		{Number: 301, Anvil: "repo-c", BeadID: "ext-301", Branch: "forge/Sib-2"},
		{Number: 103, Anvil: "repo-a", BeadID: "ext-103", Branch: "alice/fix"},
		{Number: 202, Anvil: "repo-b", BeadID: "ext-202", Branch: "bob/feature"},
		{Number: 302, Anvil: "repo-c", BeadID: "ext-302", Branch: "carol/docs"},
	}
	for i := range rows {
		rows[i].Status = state.PROpen
		rows[i].CreatedAt = time.Now()
		require.NoError(t, db.InsertPR(&rows[i]))
	}
	return []int{101, 201}
}

func newTrafficMonitor(db *state.DB, p vcs.Provider) *Monitor {
	m := New(db, func(string) vcs.Provider { return p }, time.Minute,
		map[string]string{"repo-a": "/a", "repo-b": "/b", "repo-c": "/c"}, nil, nil, nil, nil)
	zero := github.RetryBackoff{}
	m.retryBackoff = &zero
	return m
}

func sorted(xs []int) []int {
	out := append([]int(nil), xs...)
	sort.Ints(out)
	return out
}

// Only this forge's PRs reach GitHub: humans' and the sibling forge's rows are
// skipped before any call, even though the sibling's share the forge/ prefix.
func TestCheckAll_PollsOnlyOwnedPRs(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	own := trafficScenario(t, db)

	f := &trafficVCS{}
	m := newTrafficMonitor(db, f)
	m.checkAll(context.Background())

	assert.Equal(t, own, sorted(f.checked), "CheckStatus must be called for owned PRs only")
}

// With a batching provider, one request per repo covers every owned PR in it.
func TestCheckAll_BatchesOwnedPRsPerAnvil(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	trafficScenario(t, db)
	extra := &state.PR{Number: 104, Anvil: "repo-a", BeadID: "Forge-own3", Branch: "forge/Forge-own3", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(extra))

	f := &trafficVCS{}
	m := newTrafficMonitor(db, batchingVCS{f})
	m.checkAll(context.Background())

	require.Len(t, f.batches, 2, "one batch per repo with owned PRs; repo-c has none")
	assert.ElementsMatch(t, [][]int{{101, 104}, {201}}, [][]int{sorted(f.batches[0]), sorted(f.batches[1])})
	assert.Empty(t, f.checked, "every owned PR was answered by its batch")
}

// A PR the batch could not answer falls back to its own CheckStatus; a batch
// that fails outright (not on a rate limit) falls back for all its PRs.
func TestCheckAll_BatchFallbacks(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	trafficScenario(t, db)

	f := &trafficVCS{batchOmit: map[int]bool{201: true}}
	newTrafficMonitor(db, batchingVCS{f}).checkAll(context.Background())
	assert.Equal(t, []int{201}, f.checked)

	g := &trafficVCS{batchErr: errors.New("parsing batch graphql response: boom")}
	newTrafficMonitor(db, batchingVCS{g}).checkAll(context.Background())
	assert.Equal(t, []int{101, 201}, sorted(g.checked))
}

// A rate-limit refusal is not retried in-line and pauses every status poll for
// the server's Retry-After; polling resumes once it has passed.
func TestCheckAll_RateLimitPausesPollingForRetryAfter(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	trafficScenario(t, db)

	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := &trafficVCS{batchErr: &github.RateLimitError{Err: errors.New("HTTP 403: secondary rate limit"), Secondary: true, RetryAfter: 90 * time.Second}}
	m := newTrafficMonitor(db, batchingVCS{f})
	m.now = func() time.Time { return now }

	m.checkAll(context.Background())
	assert.Len(t, f.batches, 1, "the refused batch must not be replayed")
	assert.Empty(t, f.checked, "no per-PR fallback while rate limited")

	now = now.Add(60 * time.Second)
	m.checkAll(context.Background())
	assert.Len(t, f.batches, 1, "still inside Retry-After: no request at all")

	now = now.Add(31 * time.Second)
	f.batchErr = nil
	m.checkAll(context.Background())
	assert.Len(t, f.batches, 3, "after Retry-After both repos are polled again")
}

// Without a hint the pause starts at one minute and doubles per consecutive
// refusal; any pause is capped, and a success resets the escalation.
func TestNoteRateLimit_BackoffEscalatesAndCaps(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	m := New(nil, nil, time.Minute, nil, nil, nil, nil, nil)
	m.now = func() time.Time { return now }
	noHint := errors.New("gh: HTTP 403: You have exceeded a secondary rate limit")

	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		m.noteRateLimit("test", noHint)
		left, limited := m.rateLimitCooldown()
		require.True(t, limited)
		assert.Equal(t, want, left)
		now = now.Add(left)
	}

	m.noteRateLimit("test", nil)
	m.noteRateLimit("test", noHint)
	left, _ := m.rateLimitCooldown()
	assert.Equal(t, time.Minute, left, "a call that got through resets the escalation")
	now = now.Add(left)

	m.noteRateLimit("test", &github.RateLimitError{Err: errors.New("HTTP 403"), RetryAfter: 2 * time.Hour})
	left, _ = m.rateLimitCooldown()
	assert.Equal(t, maxRateLimitCooldown, left, "an hour-away reset is capped")
	now = now.Add(left)

	m.noteRateLimit("test", errors.New("HTTP 502: Bad Gateway"))
	_, limited := m.rateLimitCooldown()
	assert.False(t, limited, "other errors never pause polling")
}

// A rate limit hit on a per-PR CheckStatus stops the rest of the cycle instead
// of sending the remaining PRs into the same refusal.
func TestCheckAll_RateLimitMidCycleStopsRemainingPolls(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	trafficScenario(t, db)

	f := &trafficVCS{checkErr: errors.New("gh pr view failed: exit status 1\nstderr: GraphQL: API rate limit exceeded for user ID 1.")}
	m := newTrafficMonitor(db, f)
	m.checkAll(context.Background())

	assert.Len(t, f.checked, 1, "one refused call, not one per PR, and no in-line replays")
}
