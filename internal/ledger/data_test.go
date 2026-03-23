package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockExec builds a bdExecFunc that returns different responses based on which
// --status=<value> argument is present in the call. "bd sql" calls return an
// error to trigger the fallback to "bd list" (which the tests validate).
func mockExec(byStatus map[string][]byte, fallback []byte, execErr error) bdExecFunc {
	return func(ctx context.Context, anvilPath string, args ...string) ([]byte, error) {
		if execErr != nil {
			return nil, execErr
		}
		// Return error for "bd sql" calls so tests exercise the "bd list" fallback.
		if len(args) > 0 && args[0] == "sql" {
			return nil, fmt.Errorf("mock: bd sql not supported")
		}
		for _, a := range args {
			if resp, ok := byStatus[a]; ok {
				return resp, nil
			}
		}
		if fallback != nil {
			return fallback, nil
		}
		return []byte("[]"), nil
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestFetchAllBeadsWithExecHappyPath(t *testing.T) {
	openBeads := []Bead{
		{ID: "forge-1", Title: "Fix the leak", Status: "open"},
		{ID: "forge-2", Title: "WIP task", Status: "in_progress"},
	}
	execFn := mockExec(map[string][]byte{
		"--status=open": mustMarshal(openBeads),
	}, []byte("[]"), nil)

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	require.NotNil(t, cmd)

	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok, "expected UpdateBeadsMsg")
	assert.NoError(t, update.Err)
	require.Len(t, update.Beads, 2)

	// All returned beads must have the anvil name set (not the path).
	for _, b := range update.Beads {
		assert.Equal(t, "myAnvil", b.Anvil)
	}
}

func TestFetchAllBeadsWithExecAnvilNameNotPath(t *testing.T) {
	openBeads := []Bead{{ID: "x-1", Status: "open"}}
	execFn := mockExec(map[string][]byte{
		"--status=open": mustMarshal(openBeads),
	}, []byte("[]"), nil)

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"anvil-name": "/some/path"}, nil)
	msg := cmd()
	update := msg.(UpdateBeadsMsg)

	require.Len(t, update.Beads, 1)
	assert.Equal(t, "anvil-name", update.Beads[0].Anvil, "Anvil must be the registry name, not the filesystem path")
}

func TestFetchAllBeadsWithExecErrorPropagated(t *testing.T) {
	execErr := errors.New("bd: connection refused")
	execFn := mockExec(nil, nil, execErr)

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok)
	assert.Error(t, update.Err, "an exec error must surface in UpdateBeadsMsg.Err")
}

func TestFetchAllBeadsWithExecInvalidJSONError(t *testing.T) {
	execFn := func(ctx context.Context, anvilPath string, args ...string) ([]byte, error) {
		return []byte("not-json"), nil
	}

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok)
	assert.Error(t, update.Err, "invalid JSON from bd must produce an error in UpdateBeadsMsg")
}

func TestFetchAllBeadsWithExecClosedBeadAgeFilter(t *testing.T) {
	recent := time.Now().Add(-24 * time.Hour)
	old := time.Now().Add(-10 * 24 * time.Hour)

	closedBeads := []Bead{
		{ID: "recent-closed", Status: "closed", ClosedAt: &recent},
		{ID: "old-closed", Status: "closed", ClosedAt: &old},
	}
	execFn := mockExec(map[string][]byte{
		"--status=closed": mustMarshal(closedBeads),
	}, []byte("[]"), nil)

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	msg := cmd()
	update := msg.(UpdateBeadsMsg)

	ids := make(map[string]bool, len(update.Beads))
	for _, b := range update.Beads {
		ids[b.ID] = true
	}
	assert.True(t, ids["recent-closed"], "bead closed within 7 days must be included")
	assert.False(t, ids["old-closed"], "bead closed more than 7 days ago must be excluded")
}

func TestFetchAllBeadsWithExecUpdatedAtFallback(t *testing.T) {
	// Bead has no ClosedAt but a recent UpdatedAt — it should still be included.
	recent := time.Now().Add(-48 * time.Hour)
	closedBeads := []Bead{
		{ID: "updated-recent", Status: "closed", UpdatedAt: &recent},
	}
	execFn := mockExec(map[string][]byte{
		"--status=closed": mustMarshal(closedBeads),
	}, []byte("[]"), nil)

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	msg := cmd()
	update := msg.(UpdateBeadsMsg)

	ids := make(map[string]bool, len(update.Beads))
	for _, b := range update.Beads {
		ids[b.ID] = true
	}
	assert.True(t, ids["updated-recent"], "bead with recent UpdatedAt must be included even without ClosedAt")
}

func TestFetchAllBeadsWithExecMultipleAnvils(t *testing.T) {
	execFn := func(ctx context.Context, anvilPath string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "sql" {
			return nil, fmt.Errorf("mock: bd sql not supported")
		}
		for _, a := range args {
			if a == "--status=open" {
				bead := []Bead{{ID: "bead-in-" + anvilPath, Status: "open"}}
				return mustMarshal(bead), nil
			}
		}
		return []byte("[]"), nil
	}

	anvils := map[string]string{
		"anvil-a": "/path/a",
		"anvil-b": "/path/b",
	}
	cmd := fetchAllBeadsWithExec(execFn, anvils, nil)
	msg := cmd()
	update := msg.(UpdateBeadsMsg)

	assert.Len(t, update.Beads, 2, "one bead per anvil must be returned")
	anvilNames := make(map[string]bool)
	for _, b := range update.Beads {
		anvilNames[b.Anvil] = true
	}
	assert.True(t, anvilNames["anvil-a"])
	assert.True(t, anvilNames["anvil-b"])
}

func TestFetchAllBeadsWithExecClosedBeadFetchUsesLimit(t *testing.T) {
	// Capture all arg slices seen for closed-status queries to verify --limit 50 is passed.
	var mu sync.Mutex
	var closedArgs []string
	execFn := func(ctx context.Context, anvilPath string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "sql" {
			return nil, fmt.Errorf("mock: bd sql not supported")
		}
		for _, a := range args {
			if a == "--status=closed" {
				mu.Lock()
				closedArgs = append(closedArgs, args...)
				mu.Unlock()
				break
			}
		}
		return []byte("[]"), nil
	}

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	cmd()

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, closedArgs, "closed-bead fetch must be invoked")

	found := false
	for i, a := range closedArgs {
		if a == "--limit" && i+1 < len(closedArgs) && closedArgs[i+1] == "50" {
			found = true
			break
		}
	}
	assert.True(t, found, "closed-bead bd list must include --limit 50")
}

// ---- Tests for fetchAnvilBeadsWithExec ----

func TestFetchAnvilBeadsWithExecHappyPath(t *testing.T) {
	openBeads := []Bead{
		{ID: "a-1", Title: "Open task", Status: "open"},
		{ID: "a-2", Title: "In progress", Status: "in_progress"},
	}
	execFn := mockExec(map[string][]byte{
		"--status=open": mustMarshal(openBeads),
	}, []byte("[]"), nil)

	cmd := fetchAnvilBeadsWithExec(execFn, "myAnvil", "/tmp/anvil", nil)
	require.NotNil(t, cmd)

	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok, "expected UpdateBeadsMsg")
	assert.NoError(t, update.Err)
	require.Len(t, update.Beads, 2)

	for _, b := range update.Beads {
		assert.Equal(t, "myAnvil", b.Anvil, "Anvil must be the registry name")
	}
}

func TestFetchAnvilBeadsWithExecErrorPropagated(t *testing.T) {
	execErr := errors.New("bd: connection refused")
	execFn := mockExec(nil, nil, execErr)

	cmd := fetchAnvilBeadsWithExec(execFn, "myAnvil", "/tmp/anvil", nil)
	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok)
	assert.Error(t, update.Err, "exec error must surface in UpdateBeadsMsg.Err")
}

func TestFetchAnvilBeadsWithExecClosedBeadAgeFilter(t *testing.T) {
	recent := time.Now().Add(-24 * time.Hour)
	old := time.Now().Add(-10 * 24 * time.Hour)

	closedBeads := []Bead{
		{ID: "recent-closed", Status: "closed", ClosedAt: &recent},
		{ID: "old-closed", Status: "closed", ClosedAt: &old},
	}
	execFn := mockExec(map[string][]byte{
		"--status=closed": mustMarshal(closedBeads),
	}, []byte("[]"), nil)

	cmd := fetchAnvilBeadsWithExec(execFn, "myAnvil", "/tmp/anvil", nil)
	msg := cmd()
	update := msg.(UpdateBeadsMsg)

	ids := make(map[string]bool, len(update.Beads))
	for _, b := range update.Beads {
		ids[b.ID] = true
	}
	assert.True(t, ids["recent-closed"], "bead closed within 7 days must be included")
	assert.False(t, ids["old-closed"], "bead closed more than 7 days ago must be excluded")
}

func TestFetchAnvilBeadsWithExecAnvilNameSet(t *testing.T) {
	openBeads := []Bead{{ID: "x-1", Status: "open"}}
	execFn := mockExec(map[string][]byte{
		"--status=open": mustMarshal(openBeads),
	}, []byte("[]"), nil)

	cmd := fetchAnvilBeadsWithExec(execFn, "anvil-name", "/some/path", nil)
	msg := cmd()
	update := msg.(UpdateBeadsMsg)

	require.Len(t, update.Beads, 1)
	assert.Equal(t, "anvil-name", update.Beads[0].Anvil, "Anvil must be the registry name, not the filesystem path")
}

// ---- End of fetchAnvilBeadsWithExec tests ----

func TestParseTimeSafeOutOfRangeYear(t *testing.T) {
	// bd occasionally returns timestamps whose year is outside [0,9999].
	// Verify that unmarshalling does not error and that the resulting Bead can
	// be re-marshalled to JSON without panic or error.
	outOfRangeJSON := []byte(`[
		{"id":"bad-ts","title":"bad timestamp","status":"closed",
		 "closed_at":"99999-01-01T00:00:00Z","updated_at":"99999-06-01T00:00:00Z"}
	]`)

	var beads []Bead
	require.NoError(t, json.Unmarshal(outOfRangeJSON, &beads), "unmarshal must not error on out-of-range year")
	require.Len(t, beads, 1)
	assert.Nil(t, beads[0].ClosedAt, "out-of-range closed_at must be zeroed to nil")
	assert.Nil(t, beads[0].UpdatedAt, "out-of-range updated_at must be zeroed to nil")

	// Re-marshal must succeed without panic.
	_, err := json.Marshal(beads[0])
	assert.NoError(t, err, "re-marshal of sanitised Bead must not error")
}

func TestFetchAllBeadsWithExecOutOfRangeTimestampSkipped(t *testing.T) {
	// A closed bead with an out-of-range year in closed_at must not crash the
	// fetch and must simply not appear in the recent-beads result (its ClosedAt
	// and UpdatedAt will be nil, so it fails the 7-day recency filter).
	outOfRangeJSON := []byte(`[
		{"id":"bad-ts","title":"bad timestamp","status":"closed",
		 "closed_at":"99999-01-01T00:00:00Z","updated_at":"99999-06-01T00:00:00Z"}
	]`)
	execFn := mockExec(map[string][]byte{
		"--status=closed": outOfRangeJSON,
	}, []byte("[]"), nil)

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok)
	assert.NoError(t, update.Err, "out-of-range timestamp must not produce an error")

	for _, b := range update.Beads {
		assert.NotEqual(t, "bad-ts", b.ID, "bead with out-of-range timestamp must be filtered out by recency check")
	}
}

func TestFetchAllBeadsWithExecEmptyAnvils(t *testing.T) {
	called := false
	execFn := func(ctx context.Context, anvilPath string, args ...string) ([]byte, error) {
		called = true
		return []byte("[]"), nil
	}

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{}, nil)
	msg := cmd()
	update := msg.(UpdateBeadsMsg)

	assert.False(t, called, "exec must not be called when no anvils are registered")
	assert.Empty(t, update.Beads)
	assert.NoError(t, update.Err)
}

// TestFetchAnvilBeadsWithExecPerStatusCalls verifies that separate bd list calls
// are made for "open" and "in_progress" statuses and that their results are merged.
func TestFetchAnvilBeadsWithExecPerStatusCalls(t *testing.T) {
	openBeads := []Bead{{ID: "open-1", Status: "open"}}
	inProgressBeads := []Bead{{ID: "ip-1", Status: "in_progress"}}

	execFn := mockExec(map[string][]byte{
		"--status=open":        mustMarshal(openBeads),
		"--status=in_progress": mustMarshal(inProgressBeads),
	}, []byte("[]"), nil)

	cmd := fetchAnvilBeadsWithExec(execFn, "myAnvil", "/tmp/anvil", nil)
	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok)
	require.NoError(t, update.Err)

	ids := make(map[string]bool, len(update.Beads))
	for _, b := range update.Beads {
		ids[b.ID] = true
	}
	assert.True(t, ids["open-1"], "open bead must be included")
	assert.True(t, ids["ip-1"], "in_progress bead must be included")
}

// TestFetchAllBeadsWithExecPerStatusCalls verifies that separate bd list calls
// are made for "open" and "in_progress" statuses across all anvils.
func TestFetchAllBeadsWithExecPerStatusCalls(t *testing.T) {
	openBeads := []Bead{{ID: "open-2", Status: "open"}}
	inProgressBeads := []Bead{{ID: "ip-2", Status: "in_progress"}}

	execFn := mockExec(map[string][]byte{
		"--status=open":        mustMarshal(openBeads),
		"--status=in_progress": mustMarshal(inProgressBeads),
	}, []byte("[]"), nil)

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	msg := cmd()
	update, ok := msg.(UpdateBeadsMsg)
	require.True(t, ok)
	require.NoError(t, update.Err)

	ids := make(map[string]bool, len(update.Beads))
	for _, b := range update.Beads {
		ids[b.ID] = true
	}
	assert.True(t, ids["open-2"], "open bead must be included")
	assert.True(t, ids["ip-2"], "in_progress bead must be included")
}

// TestFetchAllBeadsWithExecSeparateStatusCallsTracked verifies that both status
// values result in distinct exec invocations (not a single call with two flags).
func TestFetchAllBeadsWithExecSeparateStatusCallsTracked(t *testing.T) {
	var mu sync.Mutex
	statusesSeen := make(map[string]int)

	execFn := func(ctx context.Context, anvilPath string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "sql" {
			return nil, fmt.Errorf("mock: bd sql not supported")
		}
		for _, a := range args {
			if a == "--status=open" || a == "--status=in_progress" {
				mu.Lock()
				statusesSeen[a]++
				mu.Unlock()
				break
			}
		}
		return []byte("[]"), nil
	}

	cmd := fetchAllBeadsWithExec(execFn, map[string]string{"myAnvil": "/tmp/anvil"}, nil)
	cmd()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, statusesSeen["--status=open"], "exactly one call for open status")
	assert.Equal(t, 1, statusesSeen["--status=in_progress"], "exactly one call for in_progress status")
}
