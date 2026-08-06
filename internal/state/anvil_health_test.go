package state

import (
	"strings"
	"testing"
	"time"
)

func wedgedFixture(anvil string) AnvilHealth {
	return AnvilHealth{
		Anvil:           anvil,
		ConflictTables:  "issues (3)",
		ConflictCount:   3,
		Branch:          "beads-sync",
		Ahead:           1,
		Behind:          10,
		DivergenceKnown: true,
		Detail:          "Beads database is mid-merge with unresolved conflicts — issues (3); beads-sync ahead 1 / behind 10",
	}
}

func TestAnvilHealth_MarkClearAndList(t *testing.T) {
	db := openTestDB(t)

	// A healthy forge has no wedged anvils and clearing is a no-op.
	rows, err := db.WedgedAnvils()
	if err != nil {
		t.Fatalf("WedgedAnvils: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no wedged anvils, got %d", len(rows))
	}
	cleared, err := db.ClearAnvilWedged("munin")
	if err != nil {
		t.Fatalf("ClearAnvilWedged: %v", err)
	}
	if cleared {
		t.Fatal("clearing an unknown anvil must report no change")
	}

	// First detection.
	first, detectedAt, err := db.MarkAnvilWedged(wedgedFixture("munin"))
	if err != nil {
		t.Fatalf("MarkAnvilWedged: %v", err)
	}
	if !first {
		t.Fatal("the first detection must be reported as new")
	}
	if detectedAt.IsZero() {
		t.Fatal("detectedAt must be set on first detection")
	}

	rows, err = db.WedgedAnvils()
	if err != nil {
		t.Fatalf("WedgedAnvils: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 wedged anvil, got %d", len(rows))
	}
	got := rows[0]
	if got.Anvil != "munin" || !got.Wedged {
		t.Fatalf("unexpected row: %+v", got)
	}
	if got.ConflictTables != "issues (3)" || got.ConflictCount != 3 {
		t.Fatalf("conflict detail not persisted: %+v", got)
	}
	if got.Branch != "beads-sync" || got.Ahead != 1 || got.Behind != 10 || !got.DivergenceKnown {
		t.Fatalf("divergence not persisted: %+v", got)
	}
	if got.DetectedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not persisted: %+v", got)
	}

	if wedged, err := db.IsAnvilWedged("munin"); err != nil || !wedged {
		t.Fatalf("IsAnvilWedged = %v, %v; want true, nil", wedged, err)
	}
	if wedged, err := db.IsAnvilWedged("other"); err != nil || wedged {
		t.Fatalf("IsAnvilWedged(other) = %v, %v; want false, nil", wedged, err)
	}

	// Clear: the flag drops and the entry disappears from the list.
	cleared, err = db.ClearAnvilWedged("munin")
	if err != nil {
		t.Fatalf("ClearAnvilWedged: %v", err)
	}
	if !cleared {
		t.Fatal("clearing a wedged anvil must report a change")
	}
	rows, err = db.WedgedAnvils()
	if err != nil {
		t.Fatalf("WedgedAnvils: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected the entry to clear, got %d rows", len(rows))
	}
	// Clearing again is idempotent and reports no change, so the recovery is
	// only ever logged once.
	if cleared, err := db.ClearAnvilWedged("munin"); err != nil || cleared {
		t.Fatalf("second clear = %v, %v; want false, nil", cleared, err)
	}
}

func TestAnvilHealth_RefreshPreservesDetectedAt(t *testing.T) {
	db := openTestDB(t)

	_, firstDetected, err := db.MarkAnvilWedged(wedgedFixture("munin"))
	if err != nil {
		t.Fatalf("MarkAnvilWedged: %v", err)
	}

	// A later poll refreshes the detail but must not restart the clock, and must
	// not report a fresh detection (otherwise every poll re-logs and re-notifies).
	updated := wedgedFixture("munin")
	updated.ConflictTables = "issues (5)"
	updated.ConflictCount = 5
	updated.Behind = 23
	first, detectedAt, err := db.MarkAnvilWedged(updated)
	if err != nil {
		t.Fatalf("MarkAnvilWedged (refresh): %v", err)
	}
	if first {
		t.Fatal("a refresh must not be reported as a new detection")
	}
	if !detectedAt.Equal(firstDetected) {
		t.Fatalf("detectedAt changed on refresh: %v != %v", detectedAt, firstDetected)
	}

	rows, err := db.WedgedAnvils()
	if err != nil {
		t.Fatalf("WedgedAnvils: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a refresh must update in place, got %d rows", len(rows))
	}
	if rows[0].ConflictCount != 5 || rows[0].Behind != 23 {
		t.Fatalf("refresh did not update detail: %+v", rows[0])
	}
	if rows[0].WedgedFor() <= 0 {
		t.Fatal("WedgedFor must report elapsed time once detected")
	}

	// After a clear, the next wedge is a fresh detection with a new clock.
	if _, err := db.ClearAnvilWedged("munin"); err != nil {
		t.Fatalf("ClearAnvilWedged: %v", err)
	}
	first, redetected, err := db.MarkAnvilWedged(wedgedFixture("munin"))
	if err != nil {
		t.Fatalf("MarkAnvilWedged (re-wedge): %v", err)
	}
	if !first {
		t.Fatal("a wedge after a recovery must be reported as new")
	}
	if redetected.Before(firstDetected) {
		t.Fatalf("re-detection clock went backwards: %v < %v", redetected, firstDetected)
	}
}

func TestAnvilHealth_WedgedAnvilsOrderedByOldestFirst(t *testing.T) {
	db := openTestDB(t)

	older := wedgedFixture("munin")
	if _, _, err := db.MarkAnvilWedged(older); err != nil {
		t.Fatalf("MarkAnvilWedged: %v", err)
	}
	// Ensure a distinct timestamp; dbTimeLayout has nanosecond precision so a
	// short sleep is plenty.
	time.Sleep(2 * time.Millisecond)
	if _, _, err := db.MarkAnvilWedged(wedgedFixture("hugin")); err != nil {
		t.Fatalf("MarkAnvilWedged: %v", err)
	}

	rows, err := db.WedgedAnvils()
	if err != nil {
		t.Fatalf("WedgedAnvils: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 wedged anvils, got %d", len(rows))
	}
	if rows[0].Anvil != "munin" {
		t.Fatalf("expected the longest-wedged anvil first, got %q", rows[0].Anvil)
	}
}

func TestAnvilHealth_PruneRemovesUnregisteredAnvils(t *testing.T) {
	db := openTestDB(t)

	if _, _, err := db.MarkAnvilWedged(wedgedFixture("munin")); err != nil {
		t.Fatalf("MarkAnvilWedged: %v", err)
	}
	if _, _, err := db.MarkAnvilWedged(wedgedFixture("removed")); err != nil {
		t.Fatalf("MarkAnvilWedged: %v", err)
	}

	// An empty keep-list must not wipe the table (a config read hiccup should
	// never silently clear real escalations).
	if err := db.PruneAnvilHealth(nil); err != nil {
		t.Fatalf("PruneAnvilHealth(nil): %v", err)
	}
	rows, _ := db.WedgedAnvils()
	if len(rows) != 2 {
		t.Fatalf("empty keep-list must be a no-op, got %d rows", len(rows))
	}

	if err := db.PruneAnvilHealth([]string{"munin"}); err != nil {
		t.Fatalf("PruneAnvilHealth: %v", err)
	}
	rows, _ = db.WedgedAnvils()
	if len(rows) != 1 || rows[0].Anvil != "munin" {
		t.Fatalf("prune did not drop the deregistered anvil: %+v", rows)
	}
}

func TestNeedsAttentionBeads_IncludesWedgedAnvils(t *testing.T) {
	db := openTestDB(t)

	// Healthy: no anvil entry at all.
	items, err := db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	for _, it := range items {
		if it.Kind == AttentionKindAnvil {
			t.Fatalf("healthy forge must not produce an anvil entry: %+v", it)
		}
	}

	if _, _, err := db.MarkAnvilWedged(wedgedFixture("munin")); err != nil {
		t.Fatalf("MarkAnvilWedged: %v", err)
	}

	items, err = db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	var found *NeedsAttentionBead
	for i := range items {
		if items[i].Kind == AttentionKindAnvil {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatal("a wedged anvil must appear in needs-attention")
	}
	if found.Anvil != "munin" {
		t.Fatalf("unexpected anvil: %+v", found)
	}
	if found.BeadID != "" {
		t.Fatalf("an anvil entry must carry no bead id, got %q", found.BeadID)
	}
	if !found.NeedsHuman {
		t.Fatal("a wedged anvil needs human intervention")
	}
	// The entry must name the conflicted table(s), the count and the divergence.
	for _, want := range []string{"issues (3)", "beads-sync ahead 1 / behind 10"} {
		if !strings.Contains(found.Reason, want) {
			t.Fatalf("reason %q missing %q", found.Reason, want)
		}
	}

	// Resolving the conflict removes the entry with no operator action.
	if _, err := db.ClearAnvilWedged("munin"); err != nil {
		t.Fatalf("ClearAnvilWedged: %v", err)
	}
	items, err = db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	for _, it := range items {
		if it.Kind == AttentionKindAnvil {
			t.Fatalf("entry must clear automatically: %+v", it)
		}
	}
}
