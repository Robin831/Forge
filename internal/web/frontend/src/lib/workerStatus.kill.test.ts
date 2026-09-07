import { describe, expect, it } from 'vitest'
import { canKillWorker, type Killable } from './workerStatus'

describe('canKillWorker', () => {
  it('allows a pending or running worker', () => {
    for (const status of ['pending', 'running']) {
      expect(canKillWorker({ status })).toBe(true)
    }
  })

  it('allows a stalled worker — the daemon applies no status gate at all', () => {
    // killWorkerProcess reads the PID off the worker row and signals it
    // whatever the row's status says, and a stalled worker's process is still
    // running: it is the status an operator most often wants to end. Hidden,
    // the panel that Forge-wl5s kept visible offered no way to do it.
    const w: Killable = { status: 'stalled' }
    expect(canKillWorker(w)).toBe(true)
  })

  it('refuses terminal statuses, which have no process to signal', () => {
    for (const status of ['done', 'failed', 'killed', 'timeout', 'partial']) {
      expect(canKillWorker({ status })).toBe(false)
    }
  })

  it('refuses a paused worker, whose verb is resume', () => {
    expect(canKillWorker({ status: 'paused' })).toBe(false)
  })

  it('refuses a missing worker', () => {
    expect(canKillWorker(null)).toBe(false)
    expect(canKillWorker(undefined)).toBe(false)
  })
})
