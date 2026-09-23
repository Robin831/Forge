package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
)

// rearmSpy is a bellowsMonitorIface double that records RearmAutoMerge calls.
// Every other method is inherited from the nil embedded interface and panics
// if called, which is the point: auto-merge must reach Bellows only this way.
type rearmSpy struct {
	bellowsMonitorIface
	mu     sync.Mutex
	rearms []int
}

func (s *rearmSpy) RearmAutoMerge(_ string, prNumber int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rearms = append(s.rearms, prNumber)
}

func (s *rearmSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rearms)
}

// The exact text `gh pr merge` printed for Explorer #401's two kinds of failure.
var (
	graphQL500 = errors.New("gh pr merge failed: exit status 1\nstderr: GraphQL: Something went wrong while executing your query on 2026-09-22T23:51:39Z. Please include `3F10:3DE84C:11B410:119655:6AB3148A` when reporting this issue.\n")
	policyErr  = errors.New("gh pr merge failed: exit status 1\nstderr: X Pull request FHIDev/Fhi.Munin.Explorer#401 is not mergeable: the base branch policy prohibits the merge.\n")
)

func newAutoMergeDaemon(t *testing.T, mock *mockVCSProvider) (*Daemon, *rearmSpy) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	spy := &rearmSpy{}
	d := &Daemon{
		db:             db,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcsProviders:   map[string]vcs.Provider{"test-anvil": mock},
		prRetryBackoff: &github.RetryBackoff{}, // no real sleeps
		bellowsMonitor: spy,
	}
	d.cfg.Store(&config.Config{
		Anvils:   map[string]config.AnvilConfig{"test-anvil": {AutoMerge: true}},
		Settings: config.SettingsConfig{MergeStrategy: "squash"},
	})
	return d, spy
}

func autoMergePR(n int) state.PR {
	return state.PR{Number: n, BeadID: "BEAD-am", Anvil: "test-anvil"}
}

// A GitHub 5xx on the merge is retried in place and the merge then succeeds —
// Explorer #401 sat merge-ready for four hours on exactly this error.
func TestDoAutoMerge_RetriesTransientFailure(t *testing.T) {
	mock := &mockVCSProvider{mergeFunc: func(call int) error {
		if call == 1 {
			return graphQL500
		}
		return nil
	}}
	d, spy := newAutoMergeDaemon(t, mock)

	d.doAutoMerge(context.Background(), "test-anvil", t.TempDir(), autoMergePR(401))

	assert.Equal(t, int32(2), mock.mergeCalls.Load(), "one failure, one successful retry")
	assert.Zero(t, spy.count(), "a merge that succeeded must not re-arm Bellows")
}

// A branch-policy refusal is permanent: exactly one attempt, no re-arm.
func TestDoAutoMerge_PolicyRefusalIsNotRetried(t *testing.T) {
	mock := &mockVCSProvider{mergeErr: policyErr}
	d, spy := newAutoMergeDaemon(t, mock)

	d.doAutoMerge(context.Background(), "test-anvil", t.TempDir(), autoMergePR(402))

	assert.Equal(t, int32(1), mock.mergeCalls.Load(), "a policy refusal must not be retried")
	assert.Zero(t, spy.count(), "a permanent refusal must not re-arm the ready edge")
}

// A 5xx does not prove the merge failed. If the PR reads back as merged the
// attempt is a success, and nothing is retried or re-armed.
func TestDoAutoMerge_ErrorButMergedIsSuccess(t *testing.T) {
	mock := &mockVCSProvider{
		mergeErr:    graphQL500,
		lightStatus: &vcs.PRStatus{State: "MERGED"},
	}
	d, spy := newAutoMergeDaemon(t, mock)

	d.doAutoMerge(context.Background(), "test-anvil", t.TempDir(), autoMergePR(403))

	assert.Equal(t, int32(1), mock.mergeCalls.Load(), "a merged PR must not be merged again")
	assert.Zero(t, spy.count())
}

// Once the transient budget is spent the ready edge is re-armed for the next
// poll — but only maxAutoMergeRearms times, so a misclassified failure cannot
// re-fire merges forever.
func TestDoAutoMerge_ExhaustedTransientRearmsBounded(t *testing.T) {
	mock := &mockVCSProvider{mergeErr: graphQL500}
	d, spy := newAutoMergeDaemon(t, mock)

	for i := 0; i < maxAutoMergeRearms+2; i++ {
		d.doAutoMerge(context.Background(), "test-anvil", t.TempDir(), autoMergePR(404))
	}

	assert.Equal(t, int32((maxAutoMergeRearms+2)*(github.MaxTransientAttempts+1)), mock.mergeCalls.Load(),
		"each auto-merge spends the full transient budget")
	assert.Equal(t, maxAutoMergeRearms, spy.count(), "re-arms must stop at the budget")
}

// A successful merge clears the PR's re-arm count.
func TestDoAutoMerge_SuccessClearsRearmCount(t *testing.T) {
	fail := true
	mock := &mockVCSProvider{mergeFunc: func(int) error {
		if fail {
			return graphQL500
		}
		return nil
	}}
	d, spy := newAutoMergeDaemon(t, mock)

	d.doAutoMerge(context.Background(), "test-anvil", t.TempDir(), autoMergePR(405))
	require.Equal(t, 1, spy.count())

	fail = false
	d.doAutoMerge(context.Background(), "test-anvil", t.TempDir(), autoMergePR(405))

	d.autoMergeRearmsMu.Lock()
	_, tracked := d.autoMergeRearms["test-anvil/405"]
	d.autoMergeRearmsMu.Unlock()
	assert.False(t, tracked, "a merged PR must not keep a re-arm count")
}
