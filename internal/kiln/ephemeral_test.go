package kiln

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestParseIPLocalPortRange(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		lo, hi int
		ok     bool
	}{
		{name: "kernel default", raw: "32768\t60999\n", lo: 32768, hi: 60999, ok: true},
		{name: "space separated", raw: "1024 65535", lo: 1024, hi: 65535, ok: true},
		{name: "extra whitespace", raw: "  32768   60999  \n", lo: 32768, hi: 60999, ok: true},
		{name: "empty", raw: ""},
		{name: "single value", raw: "32768\n"},
		{name: "three values", raw: "1 2 3"},
		{name: "non-numeric", raw: "lo hi"},
		{name: "inverted", raw: "60999 32768"},
		{name: "zero bound", raw: "0 60999"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi, ok := parseIPLocalPortRange(tc.raw)
			if ok != tc.ok {
				t.Fatalf("parseIPLocalPortRange(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			}
			if ok && (lo != tc.lo || hi != tc.hi) {
				t.Errorf("parseIPLocalPortRange(%q) = %d-%d, want %d-%d", tc.raw, lo, hi, tc.lo, tc.hi)
			}
		})
	}
}

// TestEphemeralPortRangeReadsProc pins the Linux path: the range comes from
// ip_local_port_range, and an unreadable or malformed file is "unknown" rather
// than a guess or a panic.
func TestEphemeralPortRangeReadsProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ip_local_port_range is Linux-only")
	}
	dir := t.TempDir()

	path := filepath.Join(dir, "ip_local_port_range")
	if err := os.WriteFile(path, []byte("32768\t60999\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	withProcPath(t, path)
	lo, hi, ok := EphemeralPortRange()
	if !ok || lo != 32768 || hi != 60999 {
		t.Fatalf("EphemeralPortRange() = %d-%d, ok=%v; want 32768-60999, ok=true", lo, hi, ok)
	}

	withProcPath(t, filepath.Join(dir, "does-not-exist"))
	if _, _, ok := EphemeralPortRange(); ok {
		t.Error("a missing ip_local_port_range should report an unknown range")
	}

	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, []byte("not a range at all\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	withProcPath(t, garbage)
	if _, _, ok := EphemeralPortRange(); ok {
		t.Error("a malformed ip_local_port_range should report an unknown range")
	}
}

func TestEphemeralOverlap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the overlap check needs a controllable ephemeral range")
	}
	withEphemeralRange(t, 32768, 60999)

	tests := []struct {
		name    string
		lo, hi  int
		overlap bool
	}{
		{name: "below", lo: 24000, hi: 24999},
		{name: "just below", lo: 32000, hi: 32767},
		{name: "above", lo: 61000, hi: 61999},
		{name: "contained", lo: 42000, hi: 42999, overlap: true},
		{name: "straddles the floor", lo: 32000, hi: 32768, overlap: true},
		{name: "straddles the ceiling", lo: 60999, hi: 61500, overlap: true},
		{name: "encloses", lo: 1024, hi: 65535, overlap: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			elo, ehi, overlap := EphemeralOverlap(tc.lo, tc.hi)
			if overlap != tc.overlap {
				t.Fatalf("EphemeralOverlap(%d, %d) = %v, want %v", tc.lo, tc.hi, overlap, tc.overlap)
			}
			if overlap && (elo != 32768 || ehi != 60999) {
				t.Errorf("EphemeralOverlap reported the ephemeral range as %d-%d, want 32768-60999", elo, ehi)
			}
		})
	}
}

// TestEphemeralOverlapUnknownRangeDoesNotWarn keeps the "never guess" rule: a
// host whose ephemeral range Forge cannot read gets no warning at all.
func TestEphemeralOverlapUnknownRangeDoesNotWarn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the overlap check needs a controllable ephemeral range")
	}
	withProcPath(t, filepath.Join(t.TempDir(), "missing"))

	if _, _, overlap := EphemeralOverlap(42000, 42999); overlap {
		t.Error("an unknown ephemeral range must not report an overlap")
	}
	if msg := EphemeralOverlapWarning(42000, 42999, "24000-24999"); msg != "" {
		t.Errorf("an unknown ephemeral range must not warn, got %q", msg)
	}
}

func TestEphemeralOverlapWarning(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the overlap check needs a controllable ephemeral range")
	}
	withEphemeralRange(t, 32768, 60999)

	if msg := EphemeralOverlapWarning(24000, 24999, "24000-24999"); msg != "" {
		t.Errorf("a range below the ephemeral floor must not warn, got %q", msg)
	}

	msg := EphemeralOverlapWarning(42000, 42999, "24000-24999")
	if msg == "" {
		t.Fatal("an overlapping range must warn")
	}
	for _, want := range []string{"42000-42999", "32768-60999", "address already in use", "24000-24999"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q does not name %q", msg, want)
		}
	}

	// Without a suggestion the message still names both ranges, and does not
	// trail off into an empty recommendation.
	bare := EphemeralOverlapWarning(42000, 42999, "")
	if !strings.Contains(bare, "32768-60999") || strings.Contains(bare, "e.g. ") {
		t.Errorf("unexpected bare warning: %q", bare)
	}
}

// withProcPath points the Linux reader at a fixture for the duration of a test.
func withProcPath(t *testing.T, path string) {
	t.Helper()
	original := ipLocalPortRangePath
	ipLocalPortRangePath = path
	t.Cleanup(func() { ipLocalPortRangePath = original })
}

// withEphemeralRange fakes the host ephemeral range so the overlap cases do not
// depend on whatever the test machine's kernel is configured with.
func withEphemeralRange(t *testing.T, lo, hi int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ip_local_port_range")
	body := strconv.Itoa(lo) + "\t" + strconv.Itoa(hi) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	withProcPath(t, path)
}
