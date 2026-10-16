package warden

import (
	"path/filepath"
	"strings"
	"testing"
)

// A reason the Forge does not write, such as a restore's lost-in-force-push,
// is counted under its own name instead of vanishing from the summary.
func TestSummarizeArchiveRun_CountsAnUnknownReasonByName(t *testing.T) {
	archived := []ArchivedRule{
		{Rule: Rule{ID: "a"}, ArchiveReason: ArchiveReasonStale},
		{Rule: Rule{ID: "b"}, ArchiveReason: ArchiveReasonLostInForcePush},
		{Rule: Rule{ID: "c"}, ArchiveReason: ArchiveReasonLostInForcePush},
		{Rule: Rule{ID: "d"}, ArchiveReason: "Bad Reason\n@team"},
	}
	s := SummarizeArchiveRun(nil, nil, archived)
	if s.Stale != 1 || s.Other[ArchiveReasonLostInForcePush] != 2 || s.Other["other"] != 1 {
		t.Fatalf("summary = %+v, want 1 stale, 2 lost-in-force-push, 1 other", s)
	}
	line := s.String()
	if !strings.Contains(line, "2 lost-in-force-push") || !strings.Contains(line, "1 other") || strings.Contains(line, "@team") {
		t.Errorf("line %q must name the counted reasons and nothing of the raw text", line)
	}
}

func TestArchivedRule_SourcePRRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.yaml")
	a := &Archive{Rules: []ArchivedRule{{Rule: Rule{ID: "r", Category: "style", Pattern: "p", Check: "c"}, ArchiveReason: ArchiveReasonLostInForcePush, SourcePR: "FHIDev/Munin#6700"}}}
	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Rules) != 1 || back.Rules[0].SourcePR != "FHIDev/Munin#6700" || back.Rules[0].ArchiveReason != ArchiveReasonLostInForcePush {
		t.Errorf("round trip = %+v", back.Rules)
	}
}
