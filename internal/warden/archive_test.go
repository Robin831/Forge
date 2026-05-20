package warden

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadArchive_NonExistent(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadArchive(filepath.Join(dir, "missing.yaml"))
	require.NoError(t, err)
	assert.Empty(t, a.Rules)
}

func TestArchive_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ArchiveFileName)

	a := &Archive{}
	a.Add(
		Rule{ID: "r1", Category: "testing", Pattern: "p", Check: "c", Source: SourceList{"manual"}, Added: "2026-03-07"},
		ArchiveReasonDuplicate,
		"r2",
	)

	require.NoError(t, a.Save(path))

	// Parent .forge directory must have been created.
	_, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)

	loaded, err := LoadArchive(path)
	require.NoError(t, err)
	require.Len(t, loaded.Rules, 1)
	assert.Equal(t, "r1", loaded.Rules[0].ID)
	assert.Equal(t, "testing", loaded.Rules[0].Category)
	assert.Equal(t, "r2", loaded.Rules[0].SupersededBy)
	assert.Equal(t, ArchiveReasonDuplicate, loaded.Rules[0].ArchiveReason)
	assert.False(t, loaded.Rules[0].ArchivedAt.IsZero())
}

func TestArchive_AddReplacesExistingID(t *testing.T) {
	a := &Archive{}
	a.Add(Rule{ID: "dup", Check: "first"}, ArchiveReasonStale, "")
	a.Add(Rule{ID: "dup", Check: "second"}, ArchiveReasonDuplicate, "winner")

	require.Len(t, a.Rules, 1)
	assert.Equal(t, "second", a.Rules[0].Check)
	assert.Equal(t, ArchiveReasonDuplicate, a.Rules[0].ArchiveReason)
	assert.Equal(t, "winner", a.Rules[0].SupersededBy)
}

func TestArchive_AddSetsTimestamps(t *testing.T) {
	a := &Archive{}
	before := time.Now().UTC().Add(-time.Second)
	a.Add(Rule{ID: "r1"}, ArchiveReasonStale, "")
	after := time.Now().UTC().Add(time.Second)

	got := a.Rules[0]
	assert.True(t, !got.ArchivedAt.Before(before) && !got.ArchivedAt.After(after), "ArchivedAt within window")
	assert.True(t, !got.LastSeen.Before(before) && !got.LastSeen.After(after), "LastSeen within window")
}

func TestArchive_Find(t *testing.T) {
	a := &Archive{}
	a.Add(Rule{ID: "r1"}, ArchiveReasonStale, "")
	a.Add(Rule{ID: "r2"}, ArchiveReasonDuplicate, "r3")

	got, ok := a.Find("r2")
	require.True(t, ok)
	assert.Equal(t, "r2", got.ID)
	assert.Equal(t, "r3", got.SupersededBy)

	_, ok = a.Find("missing")
	assert.False(t, ok)
}

func TestArchive_Remove(t *testing.T) {
	a := &Archive{}
	a.Add(Rule{ID: "r1"}, ArchiveReasonStale, "")
	a.Add(Rule{ID: "r2"}, ArchiveReasonDuplicate, "r3")

	removed, ok := a.Remove("r1")
	require.True(t, ok)
	assert.Equal(t, "r1", removed.ID)
	assert.Len(t, a.Rules, 1)
	assert.Equal(t, "r2", a.Rules[0].ID)

	_, ok = a.Remove("missing")
	assert.False(t, ok)
	assert.Len(t, a.Rules, 1)
}

func TestArchivePath(t *testing.T) {
	got := ArchivePath("/anvil")
	assert.Equal(t, filepath.Join("/anvil", ".forge", "warden-rules.archive.yaml"), got)
}

func TestArchive_AddArchived_AppendsAndDedupes(t *testing.T) {
	a := &Archive{}
	when := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	ar := ArchivedRule{
		Rule:          Rule{ID: "r1", Check: "first"},
		LastSeen:      when,
		ArchivedAt:    when,
		ArchiveReason: ArchiveReasonStale,
	}
	a.AddArchived(ar)
	require.Len(t, a.Rules, 1)
	assert.Equal(t, "first", a.Rules[0].Check)
	assert.Equal(t, when, a.Rules[0].LastSeen)
	assert.Equal(t, ArchiveReasonStale, a.Rules[0].ArchiveReason)

	// Same ID with different content should replace in place, preserving
	// the new LastSeen/reason verbatim (no auto-rewriting like Add does).
	when2 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	ar2 := ArchivedRule{
		Rule:          Rule{ID: "r1", Check: "second"},
		LastSeen:      when2,
		ArchivedAt:    when2,
		ArchiveReason: ArchiveReasonDuplicate,
		SupersededBy:  "merged-1",
	}
	a.AddArchived(ar2)
	require.Len(t, a.Rules, 1)
	assert.Equal(t, "second", a.Rules[0].Check)
	assert.Equal(t, when2, a.Rules[0].LastSeen)
	assert.Equal(t, ArchiveReasonDuplicate, a.Rules[0].ArchiveReason)
	assert.Equal(t, "merged-1", a.Rules[0].SupersededBy)
}
