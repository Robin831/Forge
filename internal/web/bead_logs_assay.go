package web

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

// beadLogRun is one Assay run in GET /api/bead/{id}/logs: the six-or-so pass
// sessions it wrote, plus the run-level totals the operator went to the panel
// looking for. Without it a single review renders as six rows all labelled
// "assay", which reads as six runs — and therefore as six times the spend.
type beadLogRun struct {
	// RunKey is the token the run stamped into its session filenames. It is
	// the grouping identity and is stable across polls.
	RunKey string `json:"run_key"`
	// RunID is the assay_runs row id, present only once the run has ended and
	// its record was written. Zero means the totals below are unknown, not
	// that they are zero — the frontend renders the session list alone.
	RunID int `json:"run_id,omitempty"`
	// HasSummary reports exactly that: whether a run record was found. It is
	// the flag the frontend branches on rather than inferring from RunID.
	HasSummary bool `json:"has_summary"`
	// StartedAt is the run's recorded start, or the time its key encodes when
	// no record was found. RFC3339, UTC.
	StartedAt string `json:"started_at"`
	// Status is the run's coverage outcome (complete/partial/failed), empty
	// for a run with no record or one written before coverage was tracked.
	Status string `json:"status,omitempty"`
	// CompletedPasses/TotalPasses is the pass tally behind Status.
	CompletedPasses int `json:"completed_passes"`
	TotalPasses     int `json:"total_passes"`
	// FindingsCount is the run's final, aggregated findings count — the number
	// the PR carries, not the sum of the per-session counts below, which are
	// pre-dedupe.
	FindingsCount int     `json:"findings_count"`
	CostUSD       float64 `json:"cost_usd"`
	DurationMs    int64   `json:"duration_ms"`
	PRNumber      int     `json:"pr_number,omitempty"`
	HeadSHA       string  `json:"head_sha,omitempty"`
	// ShadowMode reports a run that posted nothing on the PR by design.
	ShadowMode bool `json:"shadow_mode,omitempty"`
	// Files names the run's session log files, in the order they were written.
	// They are also present in the flat `files` list, which stays the complete
	// set so a client that ignores runs entirely keeps working.
	Files []string `json:"files"`
}

// parsedLogName is what a log filename says about itself: the pipeline stage,
// and — for an Assay session — the run it belongs to and the pass it ran.
type parsedLogName struct {
	stage  string
	runKey string
	pass   string
}

// parseLogFilename decomposes a stage log filename. Every stage writes
// "<stage>-<ts>-<seq>.log"; Assay additionally writes
// "assay-<runKey>-<pass>-<ts>-<seq>.log", where runKey is all-digits and pass
// is not, which is what makes the two shapes distinguishable from each other
// and from the older "assay-<ts>-<seq>.log" still on disk. Anything that does
// not parse degrades to stage-only, never to an error: a file Forge cannot
// name is still a file the operator can open.
func parseLogFilename(name string) parsedLogName {
	base := strings.TrimSuffix(name, ".log")
	segs := strings.Split(base, "-")
	if len(segs) == 0 || segs[0] == "" {
		return parsedLogName{stage: "other"}
	}
	stage := stageFromFilename(name)
	if stage != assay.LogStage {
		return parsedLogName{stage: stage}
	}
	// Trailing <ts>-<seq> are always numeric; everything between them and the
	// leading "assay" is the optional run key and pass name.
	if len(segs) < 4 || !isAllDigits(segs[len(segs)-1]) || !isAllDigits(segs[len(segs)-2]) {
		return parsedLogName{stage: stage}
	}
	middle := segs[1 : len(segs)-2]
	switch {
	case len(middle) >= 2 && isAllDigits(middle[0]):
		return parsedLogName{stage: stage, runKey: middle[0], pass: strings.Join(middle[1:], "-")}
	case len(middle) >= 1 && !isAllDigits(middle[0]):
		return parsedLogName{stage: stage, pass: strings.Join(middle, "-")}
	default:
		return parsedLogName{stage: stage}
	}
}

// isAllDigits reports whether s is non-empty and consists only of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// startedAtFromRunKey recovers the run's start from its key, which the daemon
// mints as the start time in milliseconds. It is the fallback for a run whose
// record has not been written yet (the row lands when the run ends, so a
// review in flight has none) or was lost.
func startedAtFromRunKey(key string) string {
	ms, err := strconv.ParseInt(key, 10, 64)
	if err != nil || ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// groupAssayRuns folds the assay session files in files into one entry per run
// and returns the runs in start order. It also stamps each session's findings
// count onto its file entry, which is what lets the expanded run row tell the
// pass that found the PR's one real problem from the four that found nothing
// and from triage, which produces none by design.
//
// Files with no run key — every non-assay stage, and assay logs written before
// the key existed — are left alone in files and appear in no run.
//
// runs is a lookup by log key, from the DB; a key it does not answer for
// yields a run with HasSummary false rather than one with zeroed totals, since
// "not recorded" and "cost nothing, found nothing" must not render the same.
func groupAssayRuns(files []beadLogFile, runs map[string]state.AssayRun) []beadLogRun {
	byKey := map[string]*beadLogRun{}
	var order []string
	for _, f := range files {
		if f.RunKey == "" {
			continue
		}
		r, ok := byKey[f.RunKey]
		if !ok {
			r = &beadLogRun{RunKey: f.RunKey, StartedAt: startedAtFromRunKey(f.RunKey)}
			byKey[f.RunKey] = r
			order = append(order, f.RunKey)
		}
		r.Files = append(r.Files, f.Filename)
	}
	if len(byKey) == 0 {
		return nil
	}

	// Pass name -> findings, per run key, for the runs that recorded a
	// breakdown. A retried pass wrote two sessions; both carry the pass-level
	// number, which is the honest one — the retry replaced the attempt, it did
	// not add findings of its own.
	passCounts := map[string]map[string]int{}
	for key, r := range byKey {
		rec, ok := runs[key]
		if !ok {
			continue
		}
		r.HasSummary = true
		r.RunID = rec.ID
		r.Status = rec.Status
		r.CompletedPasses = rec.CompletedPasses
		r.TotalPasses = rec.TotalPasses
		r.FindingsCount = rec.FindingsCount
		r.CostUSD = rec.CostUSD
		r.DurationMs = rec.DurationMs
		r.PRNumber = rec.PRNumber
		r.HeadSHA = rec.HeadSHA
		r.ShadowMode = rec.ShadowMode
		if !rec.StartedAt.IsZero() {
			r.StartedAt = rec.StartedAt.UTC().Format(time.RFC3339)
		}
		if len(rec.PassFindings) > 0 {
			counts := map[string]int{}
			for _, p := range rec.PassFindings {
				counts[p.Name] = p.Findings
			}
			passCounts[key] = counts
		}
	}

	for i := range files {
		counts, ok := passCounts[files[i].RunKey]
		if !ok || files[i].Pass == "" {
			continue
		}
		n, ok := counts[files[i].Pass]
		if !ok {
			continue
		}
		files[i].Findings = &n
	}

	out := make([]beadLogRun, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	// Time order, so a re-reviewed PR reads top-to-bottom as the sequence of
	// reviews it actually got. StartedAt is the recorded truth where there is
	// one; the key (a millisecond timestamp) breaks ties and covers runs with
	// no record.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		return out[i].RunKey < out[j].RunKey
	})
	return out
}
