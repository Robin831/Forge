import { describe, expect, it } from 'vitest'
import {
  BACKGROUND_PHASES,
  SLOT_STATUSES,
  holdsDispatchSlot,
  isBackgroundPhase,
  isSlotStatus,
  workerStatusClass,
} from './workerStatus'

// Both sets are kept in step with state.ActiveDispatchWorkers by the Go-side
// guard TestFrontendSlotStatusesMatchDispatchQuery /
// TestFrontendBackgroundPhasesMatchDispatchQuery
// (internal/state/workerstatus_frontend_test.go), which reads this module and
// compares it with the SQL the capacity queries are built from. An assertion
// here against a hand-written literal could not do that job — it would hold
// for any pair of values somebody edited both halves of, and it would keep
// passing when the Go list moved, which is precisely the drift that let this
// set omit 'stalled'. The tests below cover behaviour instead.

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

  it('leaves the bellows monitor status out of the set', () => {
    // 'monitoring' is in the daemon's own status list but not here: those rows
    // are bellows PR-monitor pseudo-workers, filtered out by isBellowsMonitor
    // on the UI side because they carry no claude log.
    expect(SLOT_STATUSES.has('monitoring')).toBe(false)
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
    // hide a real worker from the slot count.
    expect(holdsDispatchSlot({ status: 'running' })).toBe(true)
    expect(holdsDispatchSlot({ status: 'running', phase: '' })).toBe(true)
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

  it('falls back to the neutral chip for an unknown status', () => {
    expect(workerStatusClass('some-new-status')).toBe(
      'bg-slate-800 text-slate-300 border-slate-700',
    )
  })
})
