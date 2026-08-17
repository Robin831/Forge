package kiln

import (
	"fmt"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// This file is the opt-in half of "a preview service died": `restart:
// on-failure` in the manifest, and the bounded relaunch it buys.
//
// Kiln's default is still that a dead service stays dead. That is not
// conservatism for its own sake — a service dies for a reason, and a restart
// loop over a real failure replaces one honest `exited` with a status that
// keeps flickering back to healthy, which is strictly harder to diagnose than
// the death was. The policy exists for the case where the reason is *known* to
// be noise: the motivating incident was a vite dev server exiting 1 once,
// silently, unreproducibly, seven minutes into a preview nobody was watching.
//
// So every restart is bounded (`max_restarts`, default 3, hard-capped at
// MaxRestartsLimit), announced twice — the death that triggered it and the
// outcome it reached — and counted on the service record for good, so a service
// that took three deaths to reach `healthy` never reads like one that was up all
// along.

const (
	// restartBackoffBase is the wait before the first relaunch; each further
	// attempt doubles it. A dev server that dies on a port that has not been
	// released yet, or on a file the previous process still held, needs the
	// pause more than the operator needs the second back instantly.
	restartBackoffBase = time.Second
	// restartBackoffMax caps the doubling. The budget is small enough that the
	// cap is mostly a guard against a future larger one.
	restartBackoffMax = 8 * time.Second
)

// restartDelay is how long to wait before the nth relaunch: 1s, 2s, 4s, capped
// at restartBackoffMax.
func restartDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return restartBackoffBase
	}
	d := restartBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= restartBackoffMax {
			return restartBackoffMax
		}
	}
	return d
}

// claimRestartLocked decides whether this service's death is one the manifest
// asked Kiln to recover from, and if so consumes an attempt from its budget.
// It runs under p.mu, inside the same hold that demoted the service, so the
// decision and the record of it cannot be separated by a concurrent teardown.
//
// The returned attempt is 1-based. exitCode is nil for a process killed by a
// signal, which counts as a failure: nothing about a preview service asks to be
// SIGKILLed, and the one death that *is* intentional — teardown — never reaches
// here.
func (s *previewService) claimRestartLocked(exitCode *int) (attempt int, restart bool) {
	if !s.spec.Service.RestartsOnFailure() {
		return 0, false
	}
	// A clean exit is the service doing what it was told. Restarting it would
	// argue with a decision the process already made.
	if exitCode != nil && *exitCode == 0 {
		return 0, false
	}
	if s.restarts >= s.spec.Service.MaxRestarts {
		return 0, false
	}
	s.restarts++
	return s.restarts, true
}

// restartService relaunches one service after a death claimRestartLocked
// accepted. It runs on the watcher goroutine that consumed the exit.
//
// The relaunch is the original spawn re-run: the same expanded command, the same
// environment, the same allocated port — which the allocator has held for this
// preview since it started, so a restart neither releases nor re-allocates one.
// Readiness is re-checked exactly as it was at startup, and the preview's status
// is re-derived by the same fold, so a service that comes back takes its preview
// from `degraded` to `running` through the identical path a fresh start uses.
//
// Nothing here restarts during teardown: the backoff, the spawn and the
// readiness wait all watch p.procCtx (which Stop cancels) and re-check
// p.stopped, and a process that manages to spawn into a teardown is stopped
// rather than adopted.
func (p *Preview) restartService(i, attempt int) {
	svc := p.serviceAt(i)
	if svc == nil {
		return
	}
	p.mu.Lock()
	spec, port, max := svc.spec, svc.port, svc.spec.Service.MaxRestarts
	p.mu.Unlock()

	if !p.waitBeforeRestart(p.runtime.restartBackoff(attempt)) {
		return
	}

	proc, err := StartService(p.procCtx, spec)
	if err != nil {
		// A relaunch that cannot even be spawned is terminal: the command line
		// and the working directory are the same ones that failed, so a further
		// attempt would fail identically.
		p.finishRestart(i, attempt, max, nil, fmt.Errorf("relaunching %s: %w", spec.Service.Name, err))
		return
	}
	if !p.adoptRestart(i, proc) {
		// A teardown began while the process was starting. Stop took its
		// snapshot before this process existed, so nothing else will reap it —
		// the context kill would, eventually, but stopping it here means the
		// group is gone before Stop returns rather than after.
		_ = proc.Stop(p.runtime.stopTimeout)
		return
	}

	check := HealthCheck{
		Host:    p.runtime.bindHost,
		Port:    port,
		Path:    spec.Service.Health,
		Timeout: spec.Service.ReadyTimeout,
		Exited:  proc.Done(),
	}
	err = check.Wait(p.procCtx)
	p.finishRestart(i, attempt, max, proc, err)
}

// waitBeforeRestart sleeps out the backoff, reporting false when the preview was
// torn down (or the daemon shut down) while it waited.
func (p *Preview) waitBeforeRestart(d time.Duration) bool {
	if p.tornDown() {
		return false
	}
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-p.procCtx.Done():
			return false
		}
	}
	return !p.tornDown()
}

// tornDown reports whether this preview is stopping or already stopped — the
// same test handleServiceExit uses to tell a death from a teardown.
func (p *Preview) tornDown() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tornDownLocked()
}

// tornDownLocked is tornDown with p.mu already held, which is how the two
// writers in this file (adoptRestart, finishRestart) make the check and the
// write they gate one indivisible step against Stop.
func (p *Preview) tornDownLocked() bool {
	return p.stopped || (p.procCtx != nil && p.procCtx.Err() != nil)
}

// adoptRestart puts the relaunched process on the service record and returns it
// to `starting`, clearing the previous life's exit. The uptime restarts with the
// process, which is the honest reading: it is how long *this* process has run,
// and the restart count beside it is what says it is not the first.
//
// It reports false when a teardown got there first, under the same lock Stop
// sets `stopped` in — so a process spawned into a preview that is going away is
// never adopted, and the caller knows it owns reaping it.
func (p *Preview) adoptRestart(i int, proc *ServiceProcess) bool {
	p.mu.Lock()
	if p.tornDownLocked() {
		p.mu.Unlock()
		return false
	}
	svc := p.services[i]
	svc.proc = proc
	svc.logPath = proc.LogPath
	svc.startedAt = proc.StartedAt
	svc.exitedAt = time.Time{}
	svc.exitCode = nil
	svc.health = state.PreviewServiceStarting
	svc.failure = ""
	p.status = p.deriveStatusLocked()
	persistErr := p.runtime.persistRecord(p.recordLocked())
	p.mu.Unlock()

	if persistErr != nil {
		p.runtime.logger.Warn("kiln: persisting a preview service restart failed",
			"bead", p.BeadID, "service", proc.Name, "error", persistErr)
	}
	return true
}

// finishRestart records how a relaunch settled and announces it.
//
// A relaunch that never became healthy leaves the service `failed` rather than
// `exited`: it is a service that did not come up, which is what `failed` means
// everywhere else, and the process may well still be running behind a readiness
// check that never passed. Either way it is not watched again — only a healthy
// service can die after readiness, so only a healthy one is worth a watcher.
func (p *Preview) finishRestart(i, attempt, max int, proc *ServiceProcess, waitErr error) {
	p.mu.Lock()
	if p.tornDownLocked() {
		// The preview is going away; Stop owns the record from here, and a
		// write after its `stopped` snapshot would put a live-looking service
		// back into a torn-down preview's last row.
		p.mu.Unlock()
		return
	}
	svc := p.services[i]
	if waitErr != nil {
		svc.health = state.PreviewServiceFailed
		svc.failure = waitErr.Error()
	} else {
		svc.health = state.PreviewServiceHealthy
		svc.failure = ""
	}
	status := p.deriveStatusLocked()
	p.status = status
	persistErr := p.runtime.persistRecord(p.recordLocked())
	name, entry, health := svc.name, svc.entry, svc.health
	p.mu.Unlock()

	if persistErr != nil {
		p.runtime.logger.Warn("kiln: persisting a preview service restart failed",
			"bead", p.BeadID, "service", name, "error", persistErr)
	}

	detail := FormatServiceRestart(attempt, max, health, waitErr)
	// A relaunch that did not come back ends the story regardless of how much
	// budget is left: see ServiceRestart.Exhausted.
	exhausted := waitErr != nil
	if waitErr != nil {
		p.runtime.logger.Warn("kiln: restarting a preview service did not bring it back",
			"bead", p.BeadID, "service", name, "detail", detail,
			"preview_status", status, "exhausted", exhausted)
	} else {
		p.runtime.logger.Info("kiln: preview service restarted",
			"bead", p.BeadID, "service", name, "detail", detail, "preview_status", status)
		if proc != nil {
			p.watchService(i, proc)
		}
	}

	if hook := p.runtime.onServiceRestart; hook != nil {
		hook(ServiceRestart{
			BeadID:      p.BeadID,
			Anvil:       p.Anvil,
			Service:     name,
			Entry:       entry,
			Attempt:     attempt,
			MaxRestarts: max,
			Health:      health,
			Err:         waitErr,
			Status:      status,
			Detail:      detail,
			Exhausted:   exhausted,
		})
	}
}
