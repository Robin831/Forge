package state

import (
	"testing"
)

// TestConsolidatedBead_OneRowPerAnvil: the whole point of the table is that two
// anvils sharing one beads pool keep separate consolidated beads, so a write for
// one must never be readable as the other's.
func TestConsolidatedBead_OneRowPerAnvil(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetConsolidatedBead("munin", "Fhi.Metadata-h57b2"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConsolidatedBead("explorer", "Fhi.Metadata-abc12"); err != nil {
		t.Fatal(err)
	}

	for anvil, want := range map[string]string{
		"munin":    "Fhi.Metadata-h57b2",
		"explorer": "Fhi.Metadata-abc12",
	} {
		got, err := db.ConsolidatedBead(anvil)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("ConsolidatedBead(%q) = %q, want %q", anvil, got, want)
		}
	}
}

func TestConsolidatedBead_MissingAnvilIsNotAnError(t *testing.T) {
	db := openTestDB(t)

	got, err := db.ConsolidatedBead("never-scanned")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestSetConsolidatedBead_ReplacesTheAnvilsRow(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetConsolidatedBead("munin", "old"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConsolidatedBead("munin", "new"); err != nil {
		t.Fatal(err)
	}

	got, err := db.ConsolidatedBead("munin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestSetConsolidatedBead_RejectsEmptyKeys(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetConsolidatedBead("", "bead"); err == nil {
		t.Error("expected an error for an empty anvil")
	}
	if err := db.SetConsolidatedBead("munin", ""); err == nil {
		t.Error("expected an error for an empty bead id")
	}
}

func TestClearConsolidatedBead(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetConsolidatedBead("munin", "bead"); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearConsolidatedBead("munin"); err != nil {
		t.Fatal(err)
	}
	got, err := db.ConsolidatedBead("munin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty after clearing", got)
	}

	// Clearing an anvil that has no mapping is a no-op, not an error.
	if err := db.ClearConsolidatedBead("munin"); err != nil {
		t.Errorf("clearing an absent mapping: %v", err)
	}
}
