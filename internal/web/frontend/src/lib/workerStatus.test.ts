import { describe, expect, it } from 'vitest'
import { SLOT_STATUSES, isSlotStatus, workerStatusClass } from './workerStatus'

describe('SLOT_STATUSES', () => {
  // The set must stay in step with state.ActiveDispatchWorkers, whose status
  // list is what max_total_smiths is actually measured against. 'monitoring'
  // is deliberately absent: those rows are bellows PR monitors, excluded by
  // phase on the daemon side and by isBellowsMonitor on the UI side.
  it('holds exactly the statuses the daemon counts against dispatch capacity', () => {
    expect([...SLOT_STATUSES].sort()).toEqual([
      'paused',
      'pending',
      'reviewing',
      'running',
      'stalled',
    ])
  })

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
