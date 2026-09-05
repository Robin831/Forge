package state

import (
	"strings"
	"testing"
	"time"
)

// TestNeedsAttentionBeads_SurfacesAStaleChecker is the end of the chain: a
// checker that stopped completing has to actually reach the panel, carrying a
// kind that marks it as anvil-level so no consumer offers bead actions on it.
func TestNeedsAttentionBeads_SurfacesAStaleChecker(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerPRReconcile); err != nil {
		t.Fatal(err)
	}

	items, err := db.NeedsAttentionBeads(5, 5, 3, StalenessParams{
		Thresholds: map[string]time.Duration{CheckerPRReconcile: time.Hour},
		Now:        time.Now().Add(6 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	var found *NeedsAttentionBead
	for i := range items {
		if items[i].Kind == AttentionKindStale {
			found = &items[i]
			break
		}
	}
	if found == nil {
		t.Fatal("a stale checker must reach the needs-attention list")
	}
	if found.Anvil != "explorer" {
		t.Errorf("anvil = %q", found.Anvil)
	}
	if found.BeadID != "" {
		t.Error("an anvil-level entry must carry no bead ID")
	}
	if !strings.Contains(found.Title, "PR reconcile") {
		t.Errorf("title = %q", found.Title)
	}
}

// With staleness switched off (nil thresholds, which is what
// staleness_check: false produces) the list is exactly what it was before.
func TestNeedsAttentionBeads_NoStalenessParamsAddsNothing(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerPRReconcile); err != nil {
		t.Fatal(err)
	}

	items, err := db.NeedsAttentionBeads(5, 5, 3, StalenessParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == AttentionKindStale {
			t.Fatalf("staleness is off; got %+v", it)
		}
	}
}

// A checker that is completing normally produces no entry, which is the
// property that keeps the panel worth reading.
func TestNeedsAttentionBeads_HealthyCheckerIsSilent(t *testing.T) {
	db := openTestDB(t)
	if err := db.RecordCheckSuccess("explorer", CheckerPRReconcile); err != nil {
		t.Fatal(err)
	}

	items, err := db.NeedsAttentionBeads(5, 5, 3, StalenessParams{
		Thresholds: map[string]time.Duration{CheckerPRReconcile: time.Hour},
		Now:        time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == AttentionKindStale {
			t.Fatalf("a healthy checker must raise nothing; got %+v", it)
		}
	}
}
