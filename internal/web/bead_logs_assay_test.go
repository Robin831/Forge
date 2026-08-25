package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

func TestParseLogFilename(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		stage  string
		runKey string
		pass   string
	}{
		{"new format", "assay-1755000000000-logic-1755000000123-7.log", "assay", "1755000000000", "logic"},
		{"dashed pass name", "assay-1755000000000-tests-missing-1755000000123-8.log", "assay", "1755000000000", "tests-missing"},
		{"triage", "assay-1755000000000-triage-1755000000001-1.log", "assay", "1755000000000", "triage"},
		// Written before the run key and pass name existed: still stage
		// "assay", still listed, just not grouped or named.
		{"legacy assay", "assay-1730000000-3.log", "assay", "", ""},
		// No run key (an assay session spawned without one) still reports its
		// pass, so the session is named even where it cannot be grouped.
		{"pass without run key", "assay-security-1730000000-3.log", "assay", "", "security"},
		{"other stage", "smith-1000-1.log", "smith", "", ""},
		{"unknown prefix", "mystery-3000-3.log", "other", "", ""},
		{"no separator", "assay.log", "other", "", ""},
		{"non-numeric tail", "assay-1755000000000-logic-abc-7.log", "assay", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLogFilename(tc.file)
			if got.stage != tc.stage {
				t.Errorf("stage = %q, want %q", got.stage, tc.stage)
			}
			if got.runKey != tc.runKey {
				t.Errorf("runKey = %q, want %q", got.runKey, tc.runKey)
			}
			if got.pass != tc.pass {
				t.Errorf("pass = %q, want %q", got.pass, tc.pass)
			}
		})
	}
}

// logFile builds the beadLogFile a directory listing would produce for name.
func logFile(name string, mtime time.Time) beadLogFile {
	parsed := parseLogFilename(name)
	return beadLogFile{
		Filename: name,
		Stage:    parsed.stage,
		Pass:     parsed.pass,
		RunKey:   parsed.runKey,
		MTime:    mtime.UTC().Format(time.RFC3339),
	}
}

func TestGroupAssayRuns_OneRunOneEntry(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	key := "1755000000000"
	names := []string{
		"assay-" + key + "-triage-1-1.log",
		"assay-" + key + "-logic-2-2.log",
		"assay-" + key + "-security-3-3.log",
		"assay-" + key + "-conventions-4-4.log",
		"assay-" + key + "-tests-missing-5-5.log",
		"assay-" + key + "-repo-specific-6-6.log",
	}
	files := make([]beadLogFile, 0, len(names))
	for i, n := range names {
		files = append(files, logFile(n, base.Add(time.Duration(i)*time.Second)))
	}

	runs := groupAssayRuns(files, map[string]state.AssayRun{
		key: {
			ID:              953,
			LogKey:          key,
			Status:          state.AssayStatusComplete,
			CompletedPasses: 5,
			TotalPasses:     5,
			FindingsCount:   1,
			CostUSD:         8.75,
			DurationMs:      213700,
			StartedAt:       base,
			PassFindings: []state.AssayPassFindings{
				{Name: "triage", Findings: 0},
				{Name: "logic", Findings: 1},
				{Name: "security", Findings: 0},
			},
		},
	})

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %+v", len(runs), runs)
	}
	r := runs[0]
	if !r.HasSummary || r.RunID != 953 {
		t.Errorf("run summary not attached: %+v", r)
	}
	if r.CompletedPasses != 5 || r.TotalPasses != 5 || r.FindingsCount != 1 || r.CostUSD != 8.75 {
		t.Errorf("run totals = %+v", r)
	}
	if len(r.Files) != len(names) {
		t.Fatalf("run holds %d files, want %d", len(r.Files), len(names))
	}
	// Each session is identified by its pass, not by a sequence number.
	wantPasses := []string{"triage", "logic", "security", "conventions", "tests-missing", "repo-specific"}
	for i, want := range wantPasses {
		if files[i].Pass != want {
			t.Errorf("file[%d] pass = %q, want %q", i, files[i].Pass, want)
		}
	}
	// The pass that found something is distinguishable from the ones that did
	// not, and from the passes with no recorded count at all.
	if files[0].Findings == nil || *files[0].Findings != 0 {
		t.Errorf("triage findings = %v, want 0", files[0].Findings)
	}
	if files[1].Findings == nil || *files[1].Findings != 1 {
		t.Errorf("logic findings = %v, want 1", files[1].Findings)
	}
	if files[3].Findings != nil {
		t.Errorf("conventions findings = %v, want nil (not recorded)", *files[3].Findings)
	}
}

func TestGroupAssayRuns_SeveralRunsInTimeOrder(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	older, newer := "1755000000000", "1755000600000"
	files := []beadLogFile{
		logFile("assay-"+older+"-triage-1-1.log", base),
		logFile("assay-"+older+"-logic-2-2.log", base.Add(time.Second)),
		logFile("assay-"+newer+"-triage-3-3.log", base.Add(10*time.Minute)),
		logFile("assay-"+newer+"-logic-4-4.log", base.Add(10*time.Minute+time.Second)),
	}
	runs := groupAssayRuns(files, map[string]state.AssayRun{
		newer: {ID: 2, LogKey: newer, StartedAt: base.Add(10 * time.Minute), TotalPasses: 5, CompletedPasses: 5},
		older: {ID: 1, LogKey: older, StartedAt: base, TotalPasses: 5, CompletedPasses: 5},
	})
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].RunKey != older || runs[1].RunKey != newer {
		t.Errorf("runs out of time order: %q then %q", runs[0].RunKey, runs[1].RunKey)
	}
}

func TestGroupAssayRuns_NoRecordIsNotZeroedTotals(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	key := "1755000000000"
	files := []beadLogFile{logFile("assay-"+key+"-logic-1-1.log", base)}
	runs := groupAssayRuns(files, nil)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].HasSummary {
		t.Error("run with no record reported has_summary=true")
	}
	if runs[0].StartedAt == "" {
		t.Error("run with no record lost its start time; the key encodes it")
	}
}

func TestGroupAssayRuns_LegacyAndOtherStagesUngrouped(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	files := []beadLogFile{
		logFile("smith-1000-1.log", base),
		logFile("assay-1730000000-3.log", base.Add(time.Minute)),
	}
	if runs := groupAssayRuns(files, nil); runs != nil {
		t.Fatalf("expected no runs from ungrouped files, got %+v", runs)
	}
	if files[1].Stage != "assay" {
		t.Errorf("legacy assay log stage = %q, want assay", files[1].Stage)
	}
}

// TestBeadLogs_AssayRunGrouping is the end-to-end shape check over a directory
// holding one grouped run plus a legacy assay log and an ordinary stage log.
func TestBeadLogs_AssayRunGrouping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServerWithDefaults(t, nil)

	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	key := "1755000000000"
	writePreservedLog(t, "Forge-assay1", "smith-1000-1.log", "s\n", base)
	writePreservedLog(t, "Forge-assay1", "assay-1730000000-2.log", "old\n", base.Add(time.Minute))
	writePreservedLog(t, "Forge-assay1", "assay-"+key+"-triage-3-3.log", "t\n", base.Add(2*time.Minute))
	writePreservedLog(t, "Forge-assay1", "assay-"+key+"-logic-4-4.log", "l\n", base.Add(3*time.Minute))

	if err := srv.db.RecordAssayRun(&state.AssayRun{
		Anvil:           "forge",
		PRNumber:        347,
		LogKey:          key,
		StartedAt:       base.Add(2 * time.Minute),
		Status:          state.AssayStatusComplete,
		CompletedPasses: 5,
		TotalPasses:     5,
		FindingsCount:   1,
		CostUSD:         8.75,
		DurationMs:      213700,
		PassFindings:    []state.AssayPassFindings{{Name: "logic", Findings: 1}},
	}); err != nil {
		t.Fatalf("record assay run: %v", err)
	}

	rec := authedGet(t, srv, "/api/bead/Forge-assay1/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp beadLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The flat list stays complete, so a client that ignores runs is unchanged.
	if len(resp.Files) != 4 {
		t.Fatalf("expected 4 files, got %d: %+v", len(resp.Files), resp.Files)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %+v", len(resp.Runs), resp.Runs)
	}
	run := resp.Runs[0]
	if !run.HasSummary || run.CompletedPasses != 5 || run.TotalPasses != 5 || run.FindingsCount != 1 {
		t.Errorf("run summary = %+v", run)
	}
	if len(run.Files) != 2 {
		t.Errorf("run holds %d sessions, want 2: %v", len(run.Files), run.Files)
	}
	for _, f := range resp.Files {
		if f.Filename == "assay-1730000000-2.log" {
			if f.Stage != "assay" || f.RunKey != "" || f.Pass != "" {
				t.Errorf("legacy assay file = %+v", f)
			}
		}
		if f.Filename == "assay-"+key+"-logic-4-4.log" {
			if f.Pass != "logic" || f.RunKey != key {
				t.Errorf("new-format assay file = %+v", f)
			}
			if f.Findings == nil || *f.Findings != 1 {
				t.Errorf("logic findings = %v, want 1", f.Findings)
			}
		}
	}
}
