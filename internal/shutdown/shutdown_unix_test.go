//go:build !windows

package shutdown

import "testing"

func TestParseStat(t *testing.T) {
	// comm contains spaces and parentheses to exercise the last-')' logic.
	// Fields after comm: state ppid pgrp(=5) ... starttime(=22).
	// index within post-')' fields: pgrp=2, starttime=19.
	line := "1234 (weird )name) S 1 4321 4321 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 987654 0 0"
	pgid, start := parseStat([]byte(line))
	if pgid != 4321 {
		t.Errorf("pgid = %d, want 4321", pgid)
	}
	if start != 987654 {
		t.Errorf("starttime = %d, want 987654", start)
	}
}
