package state

import (
	"testing"
	"time"
)

func blockedFixture(anvil, signature string) DepcheckFailure {
	return DepcheckFailure{
		Anvil:     anvil,
		Kind:      DepcheckKindBlocked,
		Signature: signature,
		Detail:    "git said: fatal: You are not currently on a branch.",
		LastSeen:  time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC),
	}
}

// TestRecordDepcheckFailureEscalatesOnce is the whole contract of the table: the
// first sighting of a condition is news, every repeat of it is not.
func TestRecordDepcheckFailureEscalatesOnce(t *testing.T) {
	db := openTestDB(t)

	fresh, err := db.RecordDepcheckFailure(blockedFixture("heimdall", "sig-a"))
	if err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}
	if !fresh {
		t.Fatal("the first sighting of a condition must escalate")
	}

	for i := 0; i < 3; i++ {
		fresh, err = db.RecordDepcheckFailure(blockedFixture("heimdall", "sig-a"))
		if err != nil {
			t.Fatalf("RecordDepcheckFailure: %v", err)
		}
		if fresh {
			t.Fatal("an unchanged condition must not re-escalate")
		}
	}

	rows, err := db.DepcheckFailures()
	if err != nil {
		t.Fatalf("DepcheckFailures: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly one row per anvil, got %+v", rows)
	}
	if rows[0].Occurrences != 4 {
		t.Fatalf("the suppressed runs must still be counted, got %d", rows[0].Occurrences)
	}
	if rows[0].Kind != DepcheckKindBlocked || rows[0].Signature != "sig-a" {
		t.Fatalf("round-trip lost the kind/signature: %+v", rows[0])
	}
	if rows[0].FirstSeen.IsZero() || rows[0].LastSeen.IsZero() {
		t.Fatalf("round-trip lost the timestamps: %+v", rows[0])
	}
}

// TestRecordDepcheckFailurePreservesFirstSeen: the row answers "how long has
// this been blocking", so a refresh must move last_seen and leave first_seen
// where it was.
func TestRecordDepcheckFailurePreservesFirstSeen(t *testing.T) {
	db := openTestDB(t)

	first := blockedFixture("heimdall", "sig-a")
	first.LastSeen = time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	if _, err := db.RecordDepcheckFailure(first); err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}

	later := blockedFixture("heimdall", "sig-a")
	later.LastSeen = time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC)
	if _, err := db.RecordDepcheckFailure(later); err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}

	rows, err := db.DepcheckFailures()
	if err != nil {
		t.Fatalf("DepcheckFailures: %v", err)
	}
	if !rows[0].FirstSeen.Equal(first.LastSeen.UTC()) {
		t.Fatalf("first_seen moved: %v", rows[0].FirstSeen)
	}
	if !rows[0].LastSeen.Equal(later.LastSeen.UTC()) {
		t.Fatalf("last_seen did not move: %v", rows[0].LastSeen)
	}
}

// TestRecordDepcheckFailureReescalatesOnANewSignature: a different condition is
// a different next action for the operator, so it is news again — and it
// replaces the row rather than adding one, since the old condition is gone.
func TestRecordDepcheckFailureReescalatesOnANewSignature(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.RecordDepcheckFailure(blockedFixture("heimdall", "sig-a")); err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}
	fresh, err := db.RecordDepcheckFailure(blockedFixture("heimdall", "sig-b"))
	if err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}
	if !fresh {
		t.Fatal("a changed condition must escalate")
	}

	rows, err := db.DepcheckFailures()
	if err != nil {
		t.Fatalf("DepcheckFailures: %v", err)
	}
	if len(rows) != 1 || rows[0].Signature != "sig-b" || rows[0].Occurrences != 1 {
		t.Fatalf("want a single replaced row counting from one, got %+v", rows)
	}
}

// TestRecordDepcheckFailureRefusesAnUnkeyedRow: a row with no signature would
// compare equal to nothing and re-escalate forever, and one with no anvil has
// nowhere to be shown.
func TestRecordDepcheckFailureRefusesAnUnkeyedRow(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.RecordDepcheckFailure(DepcheckFailure{Signature: "sig-a"}); err == nil {
		t.Fatal("want a refusal for a failure with no anvil")
	}
	if _, err := db.RecordDepcheckFailure(DepcheckFailure{Anvil: "heimdall"}); err == nil {
		t.Fatal("want a refusal for a failure with no signature")
	}
}

func TestClearDepcheckFailure(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.RecordDepcheckFailure(blockedFixture("heimdall", "sig-a")); err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}

	cleared, err := db.ClearDepcheckFailure("heimdall")
	if err != nil {
		t.Fatalf("ClearDepcheckFailure: %v", err)
	}
	if !cleared {
		t.Fatal("want the outstanding failure reported as cleared")
	}

	// The success path calls this on every scan of every anvil, so clearing an
	// unblocked anvil must be a silent no-op rather than an error.
	cleared, err = db.ClearDepcheckFailure("heimdall")
	if err != nil {
		t.Fatalf("ClearDepcheckFailure (idempotent): %v", err)
	}
	if cleared {
		t.Fatal("clearing an unblocked anvil must report nothing cleared")
	}
}

// TestPruneDepcheckFailures: a deregistered anvil never gets the successful scan
// that would clear its row, so nothing else can withdraw it.
func TestPruneDepcheckFailures(t *testing.T) {
	db := openTestDB(t)

	for _, anvil := range []string{"heimdall", "retired"} {
		if _, err := db.RecordDepcheckFailure(blockedFixture(anvil, "sig-a")); err != nil {
			t.Fatalf("RecordDepcheckFailure: %v", err)
		}
	}

	// An empty keep list is a config that has not loaded, not a config with no
	// anvils: it must not wipe the table.
	if err := db.PruneDepcheckFailures(nil); err != nil {
		t.Fatalf("PruneDepcheckFailures(nil): %v", err)
	}
	rows, err := db.DepcheckFailures()
	if err != nil {
		t.Fatalf("DepcheckFailures: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("an empty keep list must be a no-op, got %+v", rows)
	}

	if err := db.PruneDepcheckFailures([]string{"heimdall"}); err != nil {
		t.Fatalf("PruneDepcheckFailures: %v", err)
	}
	rows, err = db.DepcheckFailures()
	if err != nil {
		t.Fatalf("DepcheckFailures: %v", err)
	}
	if len(rows) != 1 || rows[0].Anvil != "heimdall" {
		t.Fatalf("want only the registered anvil left, got %+v", rows)
	}
}

// TestBlockedScanSurfacesInNeedsAttention: the row exists to be READ. It is
// anvil-scoped, carries no bead, and must not be mistaken for a wedged beads
// database — whose remediation is a different one.
func TestBlockedScanSurfacesInNeedsAttention(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.RecordDepcheckFailure(blockedFixture("heimdall", "sig-a")); err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}

	items, err := db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	var found *NeedsAttentionBead
	for i := range items {
		if items[i].Kind == AttentionKindDepcheck {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatalf("blocked scan missing from needs-attention: %+v", items)
	}
	if found.Anvil != "heimdall" || found.BeadID != "" || !found.NeedsHuman {
		t.Fatalf("want an anvil-scoped entry needing a human, got %+v", *found)
	}
	if found.Title == "" || found.Reason == "" {
		t.Fatalf("an entry with no headline or detail says nothing: %+v", *found)
	}

	if _, err := db.ClearDepcheckFailure("heimdall"); err != nil {
		t.Fatalf("ClearDepcheckFailure: %v", err)
	}
	items, err = db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	for _, it := range items {
		if it.Kind == AttentionKindDepcheck {
			t.Fatalf("cleared failure still surfaced: %+v", it)
		}
	}
}

// TestDepcheckFailureRendersItsOwnTitle: the headline is derived from the row,
// not stored on it, so a row escalated before an edit to that sentence renders
// the current one — the same arrangement DeployFailure.Title uses.
func TestDepcheckFailureRendersItsOwnTitle(t *testing.T) {
	got := DepcheckFailure{Anvil: "heimdall"}.Title()
	if got != "Anvil heimdall: dependency scan blocked" {
		t.Fatalf("unexpected title: %q", got)
	}
}

// TestDepcheckAttentionTitleSurvivesAnEmptyTitleColumn: nothing reads the
// stored title back, so a row whose column is empty (written by an older build,
// or by hand) still gets a headline.
func TestDepcheckAttentionTitleSurvivesAnEmptyTitleColumn(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.RecordDepcheckFailure(blockedFixture("heimdall", "sig-a")); err != nil {
		t.Fatalf("RecordDepcheckFailure: %v", err)
	}
	if _, err := db.conn.Exec(`UPDATE depcheck_failures SET title = '' WHERE anvil = ?`, "heimdall"); err != nil {
		t.Fatalf("blanking the title column: %v", err)
	}

	items, err := db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	for _, it := range items {
		if it.Kind != AttentionKindDepcheck {
			continue
		}
		if it.Title != "Anvil heimdall: dependency scan blocked" {
			t.Fatalf("unexpected title: %q", it.Title)
		}
		return
	}
	t.Fatalf("blocked scan missing from needs-attention: %+v", items)
}
