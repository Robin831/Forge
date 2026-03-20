package questgiver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	anvils := map[string]string{
		"repo1": "/path/to/repo1",
		"repo2": "/path/to/repo2",
	}

	m := New(nil, 5*time.Minute, 30*time.Second, anvils, nil)

	if m.interval != 5*time.Minute {
		t.Errorf("interval = %v, want %v", m.interval, 5*time.Minute)
	}
	if m.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want %v", m.timeout, 30*time.Second)
	}
	if len(m.anvils) != 2 {
		t.Errorf("anvils count = %d, want 2", len(m.anvils))
	}
	if m.anvils["repo1"] != "/path/to/repo1" {
		t.Errorf("anvils[repo1] = %q, want /path/to/repo1", m.anvils["repo1"])
	}
	if m.logger == nil {
		t.Error("logger should not be nil")
	}
	if m.newExec == nil {
		t.Error("newExec should default to non-nil when nil is passed")
	}
}

func TestIsDuplicate_MatchingTitle(t *testing.T) {
	beads := []bdBead{
		{Title: "E2E failure: login-flow — step 2 (timeout)", Status: "open"},
		{Title: "Some other bead", Status: "open"},
	}
	data, err := json.Marshal(beads)
	if err != nil {
		t.Fatal(err)
	}

	if !hasDuplicateInJSON(data, "login-flow") {
		t.Error("expected duplicate to be found for quest 'login-flow'")
	}
}

func TestIsDuplicate_NoMatch(t *testing.T) {
	beads := []bdBead{
		{Title: "Some other bead", Status: "open"},
		{Title: "Deps(Go): update foo 1.0 → 2.0", Status: "open"},
	}
	data, err := json.Marshal(beads)
	if err != nil {
		t.Fatal(err)
	}

	if hasDuplicateInJSON(data, "login-flow") {
		t.Error("expected no duplicate for quest 'login-flow'")
	}
}

func TestIsDuplicate_ClosedNotMatched(t *testing.T) {
	beads := []bdBead{
		{Title: "E2E failure: login-flow — step 1 (assert)", Status: "closed"},
	}
	data, err := json.Marshal(beads)
	if err != nil {
		t.Fatal(err)
	}

	if hasDuplicateInJSON(data, "login-flow") {
		t.Error("closed bead should not count as duplicate")
	}
}

func TestIsDuplicate_InProgressMatches(t *testing.T) {
	beads := []bdBead{
		{Title: "E2E failure: checkout — step 0 (navigate)", Status: "in_progress"},
	}
	data, err := json.Marshal(beads)
	if err != nil {
		t.Fatal(err)
	}

	if !hasDuplicateInJSON(data, "checkout") {
		t.Error("in_progress bead should count as duplicate")
	}
}

func TestRunCancellation(t *testing.T) {
	m := New(nil, 1*time.Hour, 10*time.Second, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := m.Run(ctx)
	if err != nil {
		t.Errorf("Run() returned error on cancelled context: %v", err)
	}
}

func TestTruncateUTF8(t *testing.T) {
	// ASCII-only: should truncate cleanly at byte boundary.
	s := strings.Repeat("a", 210)
	got := truncateUTF8(s, 197)
	if len(got) != 197 {
		t.Errorf("ASCII truncation: len = %d, want 197", len(got))
	}

	// Multi-byte: em dash (—) is 3 bytes (E2 80 94).
	// Build a string where byte 197 falls mid-character.
	base := strings.Repeat("x", 195) + "—" // 195 + 3 = 198 bytes
	got = truncateUTF8(base, 197)
	if len(got) != 195 {
		t.Errorf("multi-byte truncation: len = %d, want 195 (should back up past partial rune)", len(got))
	}

	// Short string: should return unchanged.
	short := "hello"
	got = truncateUTF8(short, 200)
	if got != short {
		t.Errorf("short string: got %q, want %q", got, short)
	}
}

// hasDuplicateInJSON is a test helper that checks parsed bd list JSON output
// for a duplicate quest bead, mirroring the logic in isDuplicate without
// needing to shell out to bd.
func hasDuplicateInJSON(data []byte, questName string) bool {
	prefix := "E2E failure: " + questName
	var beads []bdBead
	if err := json.Unmarshal(data, &beads); err != nil {
		return false
	}
	for _, b := range beads {
		if strings.Contains(b.Title, prefix) && (b.Status == "open" || b.Status == "in_progress") {
			return true
		}
	}
	return false
}
