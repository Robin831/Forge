package questgiver

import (
	"fmt"
	"sync"
	"time"
)

// Status values a preview quest run can be in. A run starts RunRunning and ends
// in exactly one of the other four.
//
// RunSkipped is deliberately distinct from RunFailed: a gate said no (the anvil
// never opted in, no preview is up, the preview is not healthy, the anvil
// declares no quests), which is not the same as a browser walking the app and
// finding it broken. RunError is the run itself falling over — unreadable quest
// files, a cancelled context — where the quests never got a verdict.
const (
	RunRunning = "running"
	RunPassed  = "passed"
	RunFailed  = "failed"
	RunSkipped = "skipped"
	RunError   = "error"
)

// defaultRunHistory is how many runs a store keeps. Runs are small (a handful
// of outcomes and some strings) and only ever read by a dashboard that shows
// the latest one per bead, so this is generous rather than tuned.
const defaultRunHistory = 50

// Run is one preview quest run: what it targeted, where it got to, and what the
// quests did.
//
// A run is purely informational. Nothing in the pipeline, in Bellows or in the
// merge path reads it — a preview quest failure is an operator signal on a
// branch, not a gate on it. See RunStore for why that matters.
type Run struct {
	// RunID identifies this run for the lifetime of the daemon.
	RunID string `json:"run_id"`
	// BeadID is the bead whose preview was exercised.
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil,omitempty"`
	// PreviewID and HeadSHA identify exactly what was exercised, mirroring
	// QuestRunResult so a consumer can stay idempotent per preview+commit.
	PreviewID string `json:"preview_id,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
	// BaseURL is the preview entry URL the quests were pointed at.
	BaseURL string `json:"base_url,omitempty"`
	// Status is one of the Run* constants above.
	Status string `json:"status"`
	// SkipReason explains a RunSkipped run (one of the SkipReason* constants,
	// possibly with the offending status appended).
	SkipReason string `json:"skip_reason,omitempty"`
	// Error explains a RunError run.
	Error string `json:"error,omitempty"`
	// StartedAt is when the run was accepted; FinishedAt is zero while it runs.
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at,omitempty"`
	Duration   time.Duration  `json:"duration,omitempty"`
	Quests     []QuestOutcome `json:"quests"`
}

// Done reports whether the run has reached a terminal status. A client polling
// a run stops when this is true.
func (r Run) Done() bool { return r.Status != RunRunning }

// clone returns a deep copy so a caller cannot mutate a run the store still
// owns (or race the goroutine writing its outcome).
func (r Run) clone() Run {
	out := r
	if r.Quests != nil {
		out.Quests = make([]QuestOutcome, len(r.Quests))
		for i, q := range r.Quests {
			out.Quests[i] = q
			if q.Screenshots != nil {
				out.Quests[i].Screenshots = append([]string(nil), q.Screenshots...)
			}
		}
	}
	return out
}

// RunStore holds the preview quest runs of one daemon lifetime, keyed by run id
// and indexed by bead so a panel can ask for "the latest run for this bead"
// without knowing an id.
//
// It is in memory on purpose. A quest run describes a preview that only exists
// while the daemon does — once the preview is reaped the run has nothing left
// to point at — and keeping it out of state.db keeps a purely informational
// signal out of the tables the pipeline reads. Nothing downstream is allowed to
// gate on quest results, and the surest way to keep that true is for the
// results never to reach durable state at all.
type RunStore struct {
	mu sync.RWMutex
	// runs holds every retained run by id; order preserves insertion so the
	// oldest can be evicted once the store is full.
	runs  map[string]*Run
	order []string
	// latest maps bead id → run id of its most recent run.
	latest map[string]string
	// epoch and seq make run ids unique. epoch distinguishes daemon lifetimes,
	// so a browser polling an id from before a restart gets "unknown" rather
	// than a different run that happens to have reused the number.
	epoch int64
	seq   int64
	max   int
}

// NewRunStore returns a store retaining at most max runs (0 or less selects the
// default). Runs older than that are evicted oldest-first.
func NewRunStore(max int) *RunStore {
	if max <= 0 {
		max = defaultRunHistory
	}
	return &RunStore{
		runs:   make(map[string]*Run),
		latest: make(map[string]string),
		epoch:  time.Now().UnixNano(),
		max:    max,
	}
}

// BeginOptions describes the run being started. Everything but BeadID is
// context the panel renders; none of it is validated here — the caller has
// already resolved the preview it is about to drive a browser at.
type BeginOptions struct {
	BeadID    string
	Anvil     string
	PreviewID string
	HeadSHA   string
	BaseURL   string
}

// Begin records a new run in the RunRunning state and returns it. The returned
// copy carries the run id the caller answers its client with; the caller then
// hands that id back to Complete once RunQuestsForPreview returns.
func (s *RunStore) Begin(opts BeginOptions) Run {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	run := &Run{
		RunID:     fmt.Sprintf("qr-%d-%d", s.epoch, s.seq),
		BeadID:    opts.BeadID,
		Anvil:     opts.Anvil,
		PreviewID: opts.PreviewID,
		HeadSHA:   opts.HeadSHA,
		BaseURL:   opts.BaseURL,
		Status:    RunRunning,
		StartedAt: time.Now(),
		Quests:    []QuestOutcome{},
	}
	s.runs[run.RunID] = run
	s.order = append(s.order, run.RunID)
	if opts.BeadID != "" {
		s.latest[opts.BeadID] = run.RunID
	}
	s.evictLocked()
	return run.clone()
}

// Complete writes the outcome of a run. It classifies the result the way the
// panel renders it: an error from RunQuestsForPreview is RunError (the quests
// never got a verdict), a gated result is RunSkipped, and only an actual
// browser pass/fail is RunPassed/RunFailed.
//
// An unknown run id is ignored rather than an error: the run may have been
// evicted while it was still going, and there is nothing useful to do about it.
func (s *RunStore) Complete(runID string, res *QuestRunResult, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return
	}
	run.FinishedAt = time.Now()
	run.Duration = run.FinishedAt.Sub(run.StartedAt)

	if res != nil {
		if res.PreviewID != "" {
			run.PreviewID = res.PreviewID
		}
		if res.BaseURL != "" {
			run.BaseURL = res.BaseURL
		}
		run.Quests = res.Quests
		if run.Quests == nil {
			run.Quests = []QuestOutcome{}
		}
	}

	switch {
	case runErr != nil:
		run.Status = RunError
		run.Error = runErr.Error()
	case res == nil:
		run.Status = RunError
		run.Error = "quest run returned no result"
	case res.Skipped:
		run.Status = RunSkipped
		run.SkipReason = res.SkipReason
	case res.Passed:
		run.Status = RunPassed
	default:
		run.Status = RunFailed
	}
}

// Get returns one run by id.
func (s *RunStore) Get(runID string) (Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	run, ok := s.runs[runID]
	if !ok {
		return Run{}, false
	}
	return run.clone(), true
}

// Latest returns the most recent run for a bead, which is what a preview panel
// polls: it asks about the bead it is showing, not about a run id it would have
// to remember across a reload.
func (s *RunStore) Latest(beadID string) (Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runID, ok := s.latest[beadID]
	if !ok {
		return Run{}, false
	}
	run, ok := s.runs[runID]
	if !ok {
		return Run{}, false
	}
	return run.clone(), true
}

// Running reports whether the bead has a run in flight, so a caller can refuse
// to start a second browser against the same preview.
func (s *RunStore) Running(beadID string) bool {
	run, ok := s.Latest(beadID)
	return ok && !run.Done()
}

// evictLocked drops the oldest runs until the store is within its cap. The
// bead→latest index is only cleared when it still points at the evicted run, so
// a bead whose newer run survives keeps its pointer.
func (s *RunStore) evictLocked() {
	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		if run, ok := s.runs[oldest]; ok {
			if s.latest[run.BeadID] == oldest {
				delete(s.latest, run.BeadID)
			}
			delete(s.runs, oldest)
		}
	}
}
