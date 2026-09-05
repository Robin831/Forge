package state

import "testing"

// TestAnvilBead_TwoAnvilsOneKey is the whole point of the table: two anvils
// sharing one beads pool hit the same CVE, or run a quest of the same name, and
// neither may be handed the other's bead.
func TestAnvilBead_TwoAnvilsOneKey(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetAnvilBead("munin", "vulncheck", "GO-2026-1234", "Fhi.Metadata-h57b2"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAnvilBead("explorer", "vulncheck", "GO-2026-1234", "Fhi.Metadata-abc12"); err != nil {
		t.Fatal(err)
	}

	for anvil, want := range map[string]string{
		"munin":    "Fhi.Metadata-h57b2",
		"explorer": "Fhi.Metadata-abc12",
	} {
		got, err := db.AnvilBead(anvil, "vulncheck", "GO-2026-1234")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("AnvilBead(%q) = %q, want %q", anvil, got, want)
		}
	}
}

// TestAnvilBead_KindsDoNotCollide: one anvil, one key, two scanners. A quest
// named after a vulnerability id is absurd, but the kind column is what makes
// the answer not depend on that.
func TestAnvilBead_KindsDoNotCollide(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetAnvilBead("munin", "vulncheck", "login", "bd-vuln"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAnvilBead("munin", "questgiver", "login", "bd-quest"); err != nil {
		t.Fatal(err)
	}

	got, err := db.AnvilBead("munin", "questgiver", "login")
	if err != nil {
		t.Fatal(err)
	}
	if got != "bd-quest" {
		t.Errorf("got %q, want %q", got, "bd-quest")
	}
}

func TestAnvilBead_MissingIsNotAnError(t *testing.T) {
	db := openTestDB(t)

	got, err := db.AnvilBead("never-scanned", "questgiver", "login")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestSetAnvilBead_ReplacesTheRow(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetAnvilBead("munin", "questgiver", "login", "old"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAnvilBead("munin", "questgiver", "login", "new"); err != nil {
		t.Fatal(err)
	}

	got, err := db.AnvilBead("munin", "questgiver", "login")
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestSetAnvilBead_RejectsEmptyKeys(t *testing.T) {
	db := openTestDB(t)

	for _, c := range []struct {
		name                   string
		anvil, kind, key, bead string
	}{
		{"no anvil", "", "questgiver", "login", "bd-1"},
		{"no kind", "munin", "", "login", "bd-1"},
		{"no key", "munin", "questgiver", "", "bd-1"},
		{"no bead", "munin", "questgiver", "login", ""},
	} {
		if err := db.SetAnvilBead(c.anvil, c.kind, c.key, c.bead); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}

// TestClearAnvilBead_LeavesTheOtherAnvilAlone: the clear runs when a bead turns
// out to be gone, and it must not take the other anvil's pin for the same key
// with it.
func TestClearAnvilBead_LeavesTheOtherAnvilAlone(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetAnvilBead("munin", "vulncheck", "GO-2026-1234", "bd-munin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAnvilBead("explorer", "vulncheck", "GO-2026-1234", "bd-explorer"); err != nil {
		t.Fatal(err)
	}

	if err := db.ClearAnvilBead("munin", "vulncheck", "GO-2026-1234"); err != nil {
		t.Fatal(err)
	}

	got, err := db.AnvilBead("munin", "vulncheck", "GO-2026-1234")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("munin pin = %q, want it cleared", got)
	}

	got, err = db.AnvilBead("explorer", "vulncheck", "GO-2026-1234")
	if err != nil {
		t.Fatal(err)
	}
	if got != "bd-explorer" {
		t.Errorf("explorer pin = %q, want %q", got, "bd-explorer")
	}
}

func TestClearAnvilBead_MissingIsANoOp(t *testing.T) {
	db := openTestDB(t)

	if err := db.ClearAnvilBead("munin", "questgiver", "never-recorded"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
