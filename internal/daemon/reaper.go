package daemon

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// The leaked-worker reaper.
//
// A worker row is inserted by the goroutine that runs the work and ended by
// that goroutine's own exit — since Forge-52bp, by a defer, so no exit path
// inside a running daemon can skip it. What no defer can cover is the daemon
// not being there to run it: a crash, a SIGKILL, an OOM kill, a host reboot.
// Every row those leave behind stays non-terminal forever. It counts against
// max_total_smiths (state.ActiveDispatchWorkers), it holds a panel open in
// Hearth and the dashboard for a process that is gone, and nothing but an
// operator ever clears it.
//
// Startup's orphan sweep (internal/shutdown CleanupOrphans) closes most of that
// gap, but it closes it on PID evidence — a row is stale when its recorded
// process is gone — and PID evidence is exactly what this must not rest on:
//
//   - A live pid is not evidence of life. Pids are recycled, and a row whose
//     number has been reused reads as healthy forever.
//   - A dead pid is not evidence of death. The column names the last process
//     the row's PHASE spawned, so it is dead by design for every phase past the
//     spawn — and, most sharply, for a PAUSED row: on 2026-09-07 this host had
//     Fhi.Metadata-2p3ck at status=paused, phase=smith, pid=2157387, with no
//     /proc/2157387, while its Claude session was intact and resumable in place.
//     A reaper that read that pid would have destroyed it.
//
// So the evidence is ownership, not liveness, and it is read from three places
// that agree by construction:
//
//  1. The STATUS, through the same exemption the dispatch-exit backstop uses
//     (state.LeakCandidates is WorkerBackstopState.NeedsTerminalBackstop as
//     SQL). Paused is excluded there — structurally, in the query, not as a
//     judgement made further down — and so are the monitoring/detached handoffs
//     that are meant to outlive their goroutine.
//  2. The GENERATION. A row stamped with a daemon lifetime other than the
//     running one was inserted by a process that no longer exists, so no
//     goroutine anywhere owns it. That is a proof rather than an inference, and
//     it holds whatever the pid says.
//  3. The REGISTRY, for rows of the running generation: the ids this daemon is
//     currently running work for. A row of this lifetime that no goroutine has
//     registered and that nothing has heartbeated since the grace window is one
//     nothing is tracking.
//
// Every step fails CLOSED — towards leaving the row alone. A row it cannot
// prove leaked costs one dispatch slot until the next restart, which is the
// state that exists today; a row it reaps wrongly costs the work.

const (
	// workerHeartbeatInterval is how often the daemon refreshes heartbeat_at
	// for the worker rows it is running. One write per tick for all of them
	// (state.HeartbeatWorkers takes the whole id set), so the cost is one
	// UPDATE a minute regardless of how many workers are live.
	workerHeartbeatInterval = 30 * time.Second

	// workerHeartbeatGrace is how far behind a heartbeat must fall before a row
	// of the RUNNING generation, registered by nothing, is read as leaked.
	//
	// It is six intervals rather than the two or three that would detect a leak
	// soonest, because the two errors are not symmetric. Waiting longer costs a
	// held dispatch slot for another minute or two — the condition that already
	// persists until a restart today. Waiting too little would fail a live
	// worker whose heartbeat merely missed a few ticks: a loaded host, a
	// SQLITE_BUSY burst, a paused process group. The registry check runs first
	// and is what normally spares a live row; this window is the second guard
	// behind it, so it is sized for the case where the first one has already
	// been wrong.
	workerHeartbeatGrace = 6 * workerHeartbeatInterval

	// leakReapInterval is how often the periodic pass runs. A leak is created
	// only by a daemon that is no longer running, so within one lifetime the
	// pass has nothing to find except a registration that was never released —
	// there is no cadence to chase, and a slow one keeps the pass out of the
	// way of everything the poll loop does.
	leakReapInterval = 10 * time.Minute
)

// newDaemonGeneration mints the marker for one daemon lifetime.
//
// The pid alone would not do: pids are recycled, and a daemon that came back
// under a number a previous one held would read its predecessor's abandoned
// rows as its own. The start time alone would not do either, on a host whose
// clock is stepped backwards at boot. The pair is unique in practice for the
// only comparison that is ever made — "is this the generation I am running as"
// — and, unlike an opaque UUID, it is legible in a log line and in the row.
func newDaemonGeneration() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// liveWorkerRegistry is the set of worker row ids this daemon is currently
// running work for.
//
// Registration is the goroutine saying "this row is mine": it is taken at the
// point the goroutine begins owning the row and released by a defer at the one
// exit every path passes through, exactly like the dispatch-exit backstop.
// Nothing else may add to it — in particular, a row's mere presence in the
// database is not registration, which is the whole point.
//
// The zero value is usable.
type liveWorkerRegistry struct {
	mu  sync.Mutex
	ids map[string]int
}

// hold registers a worker id and returns the release. The count is a count
// rather than a flag so two owners of one id (a resume that overlaps its
// predecessor's teardown) cannot have the first release drop the second's
// registration.
func (r *liveWorkerRegistry) hold(id string) func() {
	if id == "" {
		return func() {}
	}
	r.mu.Lock()
	if r.ids == nil {
		r.ids = make(map[string]int)
	}
	r.ids[id]++
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.ids[id] <= 1 {
				delete(r.ids, id)
				return
			}
			r.ids[id]--
		})
	}
}

// snapshot returns the registered ids, sorted so a caller's logging and its
// heartbeat write are reproducible.
func (r *liveWorkerRegistry) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.ids))
	for id := range r.ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// has reports whether a worker id is registered.
func (r *liveWorkerRegistry) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ids[id]
	return ok
}

// trackWorker registers a worker row as owned by a live goroutine and returns
// the release, which the caller must defer.
//
// Callers hold it for as long as they may still write to the row — through
// teardown, not only through the session — because the release is what makes
// the row reapable and a row released early is one the next pass may end while
// its owner is still finishing.
func (d *Daemon) trackWorker(workerID string) func() {
	return d.liveWorkers.hold(workerID)
}

// runWorkerHeartbeat refreshes heartbeat_at for every registered worker row
// until the context ends.
//
// One ticker for the whole daemon rather than one per worker: the write is a
// single UPDATE over the registered id set, so a daemon running the maximum
// number of workers costs the same as one running none.
//
// A failed write is logged and nothing more. It must not be escalated and must
// not stop the loop: the heartbeat is the WEAKER of the reaper's two ownership
// signals and is only ever consulted for a row the registry does not know, so a
// tick lost to a busy database cannot by itself end anything.
func (d *Daemon) runWorkerHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(workerHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ids := d.liveWorkers.snapshot()
			if len(ids) == 0 {
				continue
			}
			if _, err := d.db.HeartbeatWorkers(ids); err != nil {
				d.logger.Warn("failed to refresh worker heartbeats", "workers", len(ids), "error", err)
			}
		}
	}
}

// runLeakedWorkerReaper runs the periodic pass until the context ends. The
// startup pass is made by the caller (Daemon.Run), before dispatch begins, so
// the rows a previous lifetime left behind stop holding slots on the first
// poll rather than ten minutes into it.
func (d *Daemon) runLeakedWorkerReaper(ctx context.Context) {
	ticker := time.NewTicker(leakReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.reapLeakedWorkers(ctx); err != nil {
				d.logger.Warn("leaked-worker reap pass failed", "error", err)
			}
		}
	}
}

// leakVerdict is why a candidate row was, or was not, judged leaked. The reason
// is carried rather than re-derived because it is the whole content of the WARN
// and the event: "from a daemon lifetime that ended" and "nothing has tracked
// this for eight minutes" are different claims about the row, and an operator
// reading a reaped worker needs to know which one was made.
type leakVerdict struct {
	leaked bool
	reason string
}

// judgeLeak decides one candidate. It is a pure function of the row, the
// running generation, the registry snapshot and the clock, so the whole
// decision table is testable without a daemon.
//
// Order matters. The registry is asked FIRST and is an unconditional veto: a
// row a goroutine has claimed is never reaped, whatever the columns say. Only
// then is the generation read, and only for a row of the running generation is
// the heartbeat consulted at all.
func judgeLeak(c state.LeakCandidate, generation string, live map[string]struct{}, grace time.Duration, now time.Time) leakVerdict {
	if _, owned := live[c.ID]; owned {
		return leakVerdict{reason: "a live goroutine has this worker registered"}
	}

	// A row from another lifetime — or from a build with no generation column,
	// which is the same claim with less to say — cannot be owned by a goroutine
	// in this process. Nothing else needs to be established: this is the
	// crash/restart leak, and it is decided without reading a heartbeat, whose
	// age says nothing once the process that would refresh it is gone.
	if c.Generation != generation {
		from := c.Generation
		if from == "" {
			from = "unrecorded"
		}
		return leakVerdict{
			leaked: true,
			reason: fmt.Sprintf("inserted by daemon generation %s, which is not the running one", from),
		}
	}

	// Same generation, unregistered. That is ALMOST always a row whose owner
	// registered a moment ago and has not yet been observed, or one written by
	// a path between its insert and its registration, so it is not acted on
	// until the row has also gone unheartbeated for the whole grace window.
	last := c.LastSeen()
	if last.IsZero() {
		// No heartbeat and no start time: nothing to measure an age against.
		// Deciding from the generation alone here would reap a row this daemon
		// may well own, so it is left for a later pass — by which point the
		// heartbeat loop has almost certainly stamped it.
		return leakVerdict{reason: "no heartbeat or start time to age against"}
	}
	if age := now.Sub(last); age >= grace {
		return leakVerdict{
			leaked: true,
			reason: fmt.Sprintf("no goroutine holds it and nothing has heartbeated it for %s", age.Round(time.Second)),
		}
	}
	return leakVerdict{reason: "heartbeat is still within the grace window"}
}

// reapLeakedWorkers ends every worker row whose owning goroutine is provably
// gone, and reports only the errors that stopped it from looking.
//
// A failure to reap ONE row does not fail the pass: each row is an independent
// decision, and a busy database that refuses one UPDATE must not hide the
// others. The error returned is the one from reading the candidate set, since
// that is the failure after which nothing was decided at all.
func (d *Daemon) reapLeakedWorkers(ctx context.Context) error {
	if d.db == nil {
		return nil
	}
	candidates, err := d.db.LeakCandidates()
	if err != nil {
		return fmt.Errorf("reading leaked-worker candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	// One snapshot for the whole pass, taken before any write, so two
	// candidates are never judged against two different views of the registry.
	// Taking it after the candidate read is the safe order: an id registered in
	// between appears here and spares its row, where the reverse order could
	// miss it.
	live := make(map[string]struct{})
	for _, id := range d.liveWorkers.snapshot() {
		live[id] = struct{}{}
	}
	generation := d.workerGeneration
	now := time.Now()

	for _, c := range candidates {
		if ctx.Err() != nil {
			return nil
		}
		verdict := judgeLeak(c, generation, live, workerHeartbeatGrace, now)
		if !verdict.leaked {
			continue
		}
		reaped, err := d.db.ReapLeakedWorker(c.ID, c.Generation, c.Heartbeat)
		if err != nil {
			d.logger.Warn("could not reap a leaked worker row",
				"bead", c.BeadID, "anvil", c.Anvil, "worker", c.ID, "error", err)
			continue
		}
		if !reaped {
			// The row changed under the snapshot — an owner re-stamped it, a
			// heartbeat landed, or an ordinary path finalised it. The
			// compare-and-set working, not a failure.
			continue
		}
		d.reportReapedWorker(c, verdict.reason)
	}
	return nil
}

// reportReapedWorker announces one reaped row. The pid is named as diagnostic
// context and nothing more — it took no part in the decision (see the top of
// this file) — because it is what tells an operator whether a process was left
// behind for them to clean up by hand.
func (d *Daemon) reportReapedWorker(c state.LeakCandidate, reason string) {
	status := string(c.Status)
	if status == "" {
		status = "unknown"
	}
	lastSeen := "unrecorded"
	if ls := c.LastSeen(); !ls.IsZero() {
		lastSeen = ls.Format(time.RFC3339)
	}
	d.logger.Warn("reaped a leaked worker row: no goroutine owns it",
		"bead", c.BeadID, "anvil", c.Anvil, "worker", c.ID,
		"status", status, "phase", c.Phase, "pid", c.PID,
		"generation", c.Generation, "last_seen", lastSeen, "reason", reason)

	msg := fmt.Sprintf("Leaked worker %s (%s, phase %s) marked failed — %s", c.ID, status, c.Phase, reason)
	if err := d.db.LogEvent(state.EventWorkerLeaked, msg, c.BeadID, c.Anvil); err != nil {
		d.logger.Warn("failed to log leaked worker event",
			"bead", c.BeadID, "worker", c.ID, "error", err)
	}
}
