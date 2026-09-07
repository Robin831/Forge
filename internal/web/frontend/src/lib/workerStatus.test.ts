import { describe, expect, it } from 'vitest'
import {
  BACKGROUND_PHASES,
  DISPATCH_STATUSES,
  FINISHED_WORKER_STATUSES,
  SLOT_STATUSES,
  WORKER_STATUS_CLASSES,
  holdsDispatchSlot,
  isBackgroundPhase,
  isSlotStatus,
  workerStatusClass,
} from './workerStatus'

// Both sets are kept in step with state.ActiveDispatchWorkers by the Go-side
// guard TestFrontendDispatchStatusesMatchDispatchQuery /
// TestFrontendBackgroundPhasesMatchPayloadPhases
// (internal/state/workerstatus_frontend_test.go), which reads this module and
// compares it with the SQL the capacity queries are built from — plus, on the
// phase axis, the display rewrites the workers IPC handler applies before the
// payload leaves the daemon. An assertion here against a hand-written literal
// could not do that job — it would hold for any pair of values somebody edited
// both halves of, and it would keep passing when the Go list moved, which is
// precisely the drift that let this set omit 'stalled'. The tests below cover
// behaviour instead.

describe('SLOT_STATUSES', () => {
  it('counts a stalled worker as holding its slot', () => {
    // The watchdog flags a worker stalled while its process is still running;
    // the daemon will not dispatch into that slot, so neither may the UI
    // report it as idle.
    expect(isSlotStatus('stalled')).toBe(true)
  })

  it('rejects terminal statuses', () => {
    for (const status of ['done', 'failed', 'killed', 'timeout', 'partial']) {
      expect(isSlotStatus(status)).toBe(false)
    }
  })

  it('is the dispatch set minus the one status that gets no panel', () => {
    // Panelling is the wider question and capacity is the narrower one, but
    // they differ by exactly one status: a monitoring row is either a bellows
    // pseudo-worker with no log at all or a pipeline row past Warden approval
    // waiting on push/PR creation. Neither streams a transcript, and both are
    // still counted by the daemon — which is why the capacity predicate keeps
    // the status and only the panelling set drops it.
    expect(SLOT_STATUSES.has('monitoring')).toBe(false)
    expect(DISPATCH_STATUSES.has('monitoring')).toBe(true)
    for (const status of SLOT_STATUSES) {
      expect(DISPATCH_STATUSES.has(status)).toBe(true)
    }
    expect(SLOT_STATUSES.size).toBe(DISPATCH_STATUSES.size - 1)
  })
})

describe('holdsDispatchSlot', () => {
  // ActiveDispatchWorkers filters on two axes — status IN (...) AND phase NOT
  // IN (backgroundPhases) — and the UI reproduced only the first. A lifecycle
  // fix worker has a real id, a real log path and status 'running', so nothing
  // but its phase tells it apart from a dispatched Smith.
  it('excludes a lifecycle fix worker by phase even though its status counts', () => {
    for (const phase of ['quench', 'cifix', 'burnish', 'reviewfix', 'rebase', 'assay']) {
      expect(isSlotStatus('running')).toBe(true)
      expect(holdsDispatchSlot({ status: 'running', phase })).toBe(false)
    }
  })

  it('counts a Smith pipeline phase, including schematic', () => {
    // schematic is deliberately absent from the daemon's backgroundPhases: it
    // runs on the dispatched worker before Smith and must keep its slot.
    for (const phase of ['schematic', 'smith', 'temper', 'warden']) {
      expect(holdsDispatchSlot({ status: 'running', phase })).toBe(true)
    }
  })

  it('counts a worker with no phase recorded', () => {
    // An unstamped row is a Smith row; reading "unknown" as background would
    // hide a real worker from the slot count. workers.phase is NOT NULL in the
    // schema, so the daemon's `phase NOT IN (...)` sees '' and counts the row
    // too — the two agree rather than merely resembling each other.
    expect(holdsDispatchSlot({ status: 'running' })).toBe(true)
    expect(holdsDispatchSlot({ status: 'running', phase: '' })).toBe(true)
  })

  it('excludes a bellows monitor promoted to ready_to_merge', () => {
    // The workers IPC handler rewrites a ready monitor's stored 'bellows' to
    // 'ready_to_merge', so that value reaches the dashboard and no SQL list.
    // Its exclusion must rest on the phase axis in its own right: a payload
    // carrying it with any counted status would otherwise eat a Smith slot.
    expect(isBackgroundPhase('ready_to_merge')).toBe(true)
    expect(holdsDispatchSlot({ status: 'monitoring', phase: 'ready_to_merge' })).toBe(false)
    expect(holdsDispatchSlot({ status: 'running', phase: 'ready_to_merge' })).toBe(false)
  })

  it('counts a monitoring row whose phase is not a background one', () => {
    // The daemon does (status IN dispatchStatuses AND phase NOT IN
    // backgroundPhases), and a pipeline holds its own row at 'monitoring' from
    // Warden approval until the worktree is removed. Every such row carries
    // phase 'bellows' today, but the capacity predicate must agree with the
    // query rather than with that coincidence.
    expect(holdsDispatchSlot({ status: 'monitoring', phase: 'warden' })).toBe(true)
    expect(holdsDispatchSlot({ status: 'monitoring', phase: 'bellows' })).toBe(false)
  })

  it('still excludes terminal statuses whatever the phase', () => {
    expect(holdsDispatchSlot({ status: 'done', phase: 'smith' })).toBe(false)
  })

  it('reads the background set through isBackgroundPhase', () => {
    expect(isBackgroundPhase('bellows')).toBe(true)
    expect(isBackgroundPhase('smith')).toBe(false)
    expect(isBackgroundPhase(undefined)).toBe(false)
    expect(BACKGROUND_PHASES.has('schematic')).toBe(false)
  })
})

describe('workerStatusClass', () => {
  it('gives stalled its own chip, distinct from paused and from running', () => {
    const stalled = workerStatusClass('stalled')
    expect(stalled).toContain('orange')
    expect(stalled).not.toEqual(workerStatusClass('paused'))
    expect(stalled).not.toEqual(workerStatusClass('running'))
  })

  it('styles every terminal status', () => {
    // A terminal status with no key here renders as an unrecognised one. That
    // is how 'killed' read on every surface — none of the three maps folded
    // into this one styled it, though kill_worker / `forge queue stop` is a
    // routine operator action — so the guard is over the set rather than over
    // the statuses somebody remembered.
    for (const status of FINISHED_WORKER_STATUSES) {
      expect(WORKER_STATUS_CLASSES).toHaveProperty(status)
      expect(workerStatusClass(status)).not.toEqual(workerStatusClass('some-new-status'))
    }
    expect(workerStatusClass('killed')).toContain('red')
  })

  it('falls back to the neutral chip for an unknown status', () => {
    expect(workerStatusClass('some-new-status')).toBe(
      'bg-slate-800 text-slate-300 border-slate-700',
    )
  })
})
