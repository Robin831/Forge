package questgiver

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStore_BeginAndComplete(t *testing.T) {
	store := NewRunStore(0)

	run := store.Begin(BeginOptions{
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		PreviewID: "Forge-abc1",
		HeadSHA:   "abcdef1234",
		BaseURL:   "http://box:42001/",
	})
	require.NotEmpty(t, run.RunID)
	assert.Equal(t, RunRunning, run.Status)
	assert.False(t, run.Done())
	assert.True(t, store.Running("Forge-abc1"))

	store.Complete(run.RunID, &QuestRunResult{
		Passed: true,
		Quests: []QuestOutcome{{Name: "login", Passed: true, FailedStep: -1}},
	}, nil)

	got, ok := store.Get(run.RunID)
	require.True(t, ok)
	assert.Equal(t, RunPassed, got.Status)
	assert.True(t, got.Done())
	assert.False(t, got.FinishedAt.IsZero())
	assert.Len(t, got.Quests, 1)
	assert.False(t, store.Running("Forge-abc1"))
}

// TestRunStore_Classification pins the four terminal statuses apart. A gated
// run and a run that fell over are not failures — only a browser verdict is.
func TestRunStore_Classification(t *testing.T) {
	tests := []struct {
		name string
		res  *QuestRunResult
		err  error
		want string
	}{
		{"passed", &QuestRunResult{Passed: true}, nil, RunPassed},
		{"failed", &QuestRunResult{Passed: false,
			Quests: []QuestOutcome{{Name: "checkout"}}}, nil, RunFailed},
		{"skipped", &QuestRunResult{Skipped: true, SkipReason: SkipReasonNoQuests}, nil, RunSkipped},
		{"errored", nil, errors.New("unreadable quest file"), RunError},
		{"no result and no error is still an error", nil, nil, RunError},
		// An error wins over whatever partial result came back with it.
		{"error beats a result", &QuestRunResult{Passed: true}, errors.New("cancelled"), RunError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewRunStore(0)
			run := store.Begin(BeginOptions{BeadID: "Forge-abc1"})
			store.Complete(run.RunID, tc.res, tc.err)

			got, ok := store.Get(run.RunID)
			require.True(t, ok)
			assert.Equal(t, tc.want, got.Status)
			if tc.want == RunSkipped {
				assert.Equal(t, SkipReasonNoQuests, got.SkipReason)
			}
			if tc.want == RunError {
				assert.NotEmpty(t, got.Error)
			}
		})
	}
}

// TestRunStore_LatestPerBead is what a panel polls with: the bead, not a run id
// it would have to remember across a page reload.
func TestRunStore_LatestPerBead(t *testing.T) {
	store := NewRunStore(0)
	first := store.Begin(BeginOptions{BeadID: "Forge-abc1"})
	store.Complete(first.RunID, &QuestRunResult{Passed: false}, nil)
	other := store.Begin(BeginOptions{BeadID: "Forge-zzzz"})
	second := store.Begin(BeginOptions{BeadID: "Forge-abc1"})

	latest, ok := store.Latest("Forge-abc1")
	require.True(t, ok)
	assert.Equal(t, second.RunID, latest.RunID)

	latestOther, ok := store.Latest("Forge-zzzz")
	require.True(t, ok)
	assert.Equal(t, other.RunID, latestOther.RunID)

	_, ok = store.Latest("Forge-never")
	assert.False(t, ok)
}

// TestRunStore_EvictsOldest keeps the store bounded, and keeps the bead index
// from pointing at a run that is gone.
func TestRunStore_EvictsOldest(t *testing.T) {
	store := NewRunStore(2)
	first := store.Begin(BeginOptions{BeadID: "a"})
	store.Begin(BeginOptions{BeadID: "b"})
	store.Begin(BeginOptions{BeadID: "c"})

	_, ok := store.Get(first.RunID)
	assert.False(t, ok, "the oldest run should have been evicted")
	_, ok = store.Latest("a")
	assert.False(t, ok, "the evicted run's bead index should be gone too")
	_, ok = store.Latest("c")
	assert.True(t, ok)

	// Completing an evicted run is a no-op rather than a panic: the goroutine
	// that owns it has no way to know it aged out.
	store.Complete(first.RunID, &QuestRunResult{Passed: true}, nil)
}

// TestRunStore_RunIDsAreUnique guards the id the client polls with.
func TestRunStore_RunIDsAreUnique(t *testing.T) {
	store := NewRunStore(0)
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		run := store.Begin(BeginOptions{BeadID: "Forge-abc1"})
		assert.False(t, seen[run.RunID], "duplicate run id %s", run.RunID)
		seen[run.RunID] = true
	}
}

// TestRunStore_ReturnsCopies stops a caller mutating a run the store still owns
// (and racing the goroutine writing its outcome).
func TestRunStore_ReturnsCopies(t *testing.T) {
	store := NewRunStore(0)
	run := store.Begin(BeginOptions{BeadID: "Forge-abc1"})
	store.Complete(run.RunID, &QuestRunResult{
		Passed: false,
		Quests: []QuestOutcome{{Name: "checkout", Screenshots: []string{"/tmp/a.png"}}},
	}, nil)

	got, ok := store.Get(run.RunID)
	require.True(t, ok)
	got.Status = "tampered"
	got.Quests[0].Name = "tampered"
	got.Quests[0].Screenshots[0] = "/etc/passwd"

	again, ok := store.Get(run.RunID)
	require.True(t, ok)
	assert.Equal(t, RunFailed, again.Status)
	assert.Equal(t, "checkout", again.Quests[0].Name)
	assert.Equal(t, "/tmp/a.png", again.Quests[0].Screenshots[0])
}
