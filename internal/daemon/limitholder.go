package daemon

import (
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// wedgedLimitHolder is one worker that has held the global max_total_smiths
// limit for more consecutive polls than the configured threshold allows,
// together with the count that crossed it. It carries the whole worker row
// rather than an id because what makes the report actionable is the bead, the
// phase and the age beside the id — an id alone sends the operator back to the
// dashboard to look up the three things the log already had.
type wedgedLimitHolder struct {
	Worker state.Worker
	Polls  int
}

// limitHolderTracker records WHICH workers are occupying the global dispatch
// limit and for how many consecutive polls, so a forge that is merely busy can
// be told apart from one worker that stopped making progress while holding a
// slot.
//
// The condition alone cannot draw that distinction: "global smith limit
// reached" is the normal state of a saturated forge and reads identically
// whether the slots are turning over every cycle or one worker has held the
// same slot since Tuesday. Identity is what separates them — a healthy forge
// cycles through worker ids, a wedged one repeats one — which is why the
// tracker keys on the worker id and not on the number of blocked polls.
//
// The zero value is usable; the maps are created on first use so the daemon's
// constructor does not have to know about it.
type limitHolderTracker struct {
	mu sync.Mutex
	// polls counts, per worker id, the consecutive polls in which that worker
	// was among the rows filling the limit. An id absent from one poll's
	// holders is dropped rather than decremented: the run it was counting has
	// ended, and a worker that takes a slot again later starts a new one.
	polls map[string]int
	// announced holds the ids already reported at WARN during their CURRENT
	// holding run. The scan repeats every poll for as long as the condition
	// lasts, so without it a genuinely wedged worker would re-announce itself
	// every poll interval forever — the failure mode depcheck's blocked-scan
	// signature and the smelter's contradiction announcer both exist to
	// prevent. An id that stops holding is un-announced, so a recurrence after
	// an operator has dealt with it is news again.
	announced map[string]bool
}

// observe records one poll in which the global limit blocked dispatch and
// returns the holders whose consecutive-poll count has just crossed threshold.
//
// It returns only the holders that crossed on THIS call: a worker already
// announced during the run it is still in is not returned again. A threshold of
// 0 or less disables the reporting while leaving the counting in place, so
// hot-reloading the setting back on does not start every count from zero.
func (t *limitHolderTracker) observe(holders []state.Worker, threshold int) []wedgedLimitHolder {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.announced == nil {
		t.announced = make(map[string]bool)
	}

	// Built fresh from this poll's holders rather than mutated in place, which
	// is what drops the ids that no longer hold the limit without a second
	// pass to find them.
	next := make(map[string]int, len(holders))
	var wedged []wedgedLimitHolder
	for _, w := range holders {
		if w.ID == "" {
			continue
		}
		polls := t.polls[w.ID] + 1
		next[w.ID] = polls
		if threshold <= 0 || polls <= threshold || t.announced[w.ID] {
			continue
		}
		wedged = append(wedged, wedgedLimitHolder{Worker: w, Polls: polls})
	}

	// Forget the announcements of workers that have stopped holding the limit,
	// before recording this poll's, so a worker that holds it again later is
	// announced again.
	for id := range t.announced {
		if _, still := next[id]; !still {
			delete(t.announced, id)
		}
	}
	for _, h := range wedged {
		t.announced[h.Worker.ID] = true
	}
	t.polls = next
	return wedged
}

// release records a poll in which the limit did NOT block dispatch. Every count
// and every announcement is dropped: the run each one was measuring is over,
// and a worker still running at that moment is one that demonstrably is not
// holding the forge shut.
func (t *limitHolderTracker) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.polls = nil
	t.announced = nil
}

// reportGlobalLimitReached logs one poll's refusal to dispatch because the
// global max_total_smiths limit is full.
//
// The INFO line is kept exactly as it was, for every such poll: it is the
// statement that this cycle dispatched nothing, and ordinary saturation must
// stay at INFO rather than becoming a WARN every poll interval on a forge doing
// precisely what it was configured to do. The WARN is additional and names one
// worker: it fires only when the SAME worker has held the limit across more
// than wedged_limit_polls consecutive polls, which saturation does not do.
func (d *Daemon) reportGlobalLimitReached(holders []state.Worker, maxTotal int) {
	d.logger.Info("global smith limit reached, skipping dispatch", "max", maxTotal)

	threshold, enabled := d.config().Settings.ResolvedWedgedLimitPolls()
	if !enabled {
		threshold = 0
	}
	for _, h := range d.limitHolders.observe(holders, threshold) {
		w := h.Worker
		d.logger.Warn("global smith limit held by the same worker across consecutive polls — it may be wedged",
			"worker", w.ID,
			"bead", w.BeadID,
			"anvil", w.Anvil,
			"phase", w.Phase,
			"status", string(w.Status),
			"pid", w.PID,
			"age", holderAge(w).String(),
			"polls", h.Polls,
			"threshold", threshold,
			"max", maxTotal)
	}
}

// holderAge is how long the worker has been running, rounded to the second
// because the reader is deciding whether to go and look at a session, not
// timing one.
//
// A row with no start time reports 0 rather than the whole span since the zero
// instant, which would read as an age of two millennia and say nothing.
func holderAge(w state.Worker) time.Duration {
	if w.StartedAt.IsZero() {
		return 0
	}
	return time.Since(w.StartedAt).Round(time.Second)
}
