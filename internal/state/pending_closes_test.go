package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openPendingCloseDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPendingBeadCloses_RoundTrip(t *testing.T) {
	db := openPendingCloseDB(t)

	merged := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	require.NoError(t, db.UpsertPendingBeadClose(PendingBeadClose{
		BeadID:    "Forge-ir70",
		Anvil:     "forge",
		PRNumber:  773,
		Reason:    "PR #773 merged",
		Attempts:  4,
		LastError: "schema migration lock unavailable: timeout",
		MergedAt:  merged,
	}))

	got, err := db.PendingBeadCloses()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Forge-ir70", got[0].BeadID)
	assert.Equal(t, "forge", got[0].Anvil)
	assert.Equal(t, 773, got[0].PRNumber)
	assert.Equal(t, "PR #773 merged", got[0].Reason)
	assert.Equal(t, 4, got[0].Attempts)
	assert.Contains(t, got[0].LastError, "lock unavailable")
	assert.WithinDuration(t, merged, got[0].MergedAt, time.Millisecond)

	require.NoError(t, db.DeletePendingBeadClose("Forge-ir70", "forge"))
	got, err = db.PendingBeadCloses()
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestPendingBeadCloses_AttemptsAccumulate pins the semantics the reconciler
// relies on: attempts are cumulative across cycles, and the original merge
// time survives re-attempts.
func TestPendingBeadCloses_AttemptsAccumulate(t *testing.T) {
	db := openPendingCloseDB(t)

	merged := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	require.NoError(t, db.UpsertPendingBeadClose(PendingBeadClose{
		BeadID: "B", Anvil: "a", PRNumber: 1, Attempts: 4, MergedAt: merged, LastError: "first",
	}))
	require.NoError(t, db.UpsertPendingBeadClose(PendingBeadClose{
		BeadID: "B", Anvil: "a", PRNumber: 1, Attempts: 4, MergedAt: time.Now(), LastError: "second",
	}))

	got, err := db.PendingBeadCloses()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 8, got[0].Attempts)
	assert.Equal(t, "second", got[0].LastError)
	assert.WithinDuration(t, merged, got[0].MergedAt, time.Millisecond,
		"merged_at must not be overwritten by a re-attempt")
}

func TestPendingBeadCloses_OrderedByMergeTime(t *testing.T) {
	db := openPendingCloseDB(t)

	now := time.Now()
	require.NoError(t, db.UpsertPendingBeadClose(PendingBeadClose{
		BeadID: "newer", Anvil: "a", MergedAt: now,
	}))
	require.NoError(t, db.UpsertPendingBeadClose(PendingBeadClose{
		BeadID: "older", Anvil: "a", MergedAt: now.Add(-3 * time.Hour),
	}))

	got, err := db.PendingBeadCloses()
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "older", got[0].BeadID, "longest-stuck bead is retried first")
	assert.Equal(t, "newer", got[1].BeadID)
}

func TestUpsertPendingBeadClose_RequiresKeys(t *testing.T) {
	db := openPendingCloseDB(t)
	assert.Error(t, db.UpsertPendingBeadClose(PendingBeadClose{Anvil: "a"}))
	assert.Error(t, db.UpsertPendingBeadClose(PendingBeadClose{BeadID: "b"}))
}
