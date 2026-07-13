import { describe, expect, it } from 'vitest'
import { pauseDisabledReason, resumeDisabledReason, type Pausable } from './api'

describe('pauseDisabledReason', () => {
  it('rejects a missing worker as having no active pipeline', () => {
    expect(pauseDisabledReason(null)).toMatch(/no active pipeline/i)
    expect(pauseDisabledReason(undefined)).toMatch(/no active pipeline/i)
  })

  it('allows a running worker', () => {
    const w: Pausable = { status: 'running' }
    expect(pauseDisabledReason(w)).toBeNull()
  })

  it('rejects a paused worker (already paused)', () => {
    const w: Pausable = { status: 'paused' }
    expect(pauseDisabledReason(w)).toMatch(/only a running worker can be paused/i)
  })

  it('rejects a pending worker (no live spawn to park yet)', () => {
    const w: Pausable = { status: 'pending' }
    const reason = pauseDisabledReason(w)
    expect(reason).toMatch(/cannot pause a pending worker/i)
  })
})

describe('resumeDisabledReason', () => {
  it('rejects a missing worker as having no paused pipeline', () => {
    expect(resumeDisabledReason(null)).toMatch(/no paused pipeline/i)
    expect(resumeDisabledReason(undefined)).toMatch(/no paused pipeline/i)
  })

  it('allows a paused worker', () => {
    const w: Pausable = { status: 'paused' }
    expect(resumeDisabledReason(w)).toBeNull()
  })

  it('rejects a running worker (nothing to resume)', () => {
    const w: Pausable = { status: 'running' }
    expect(resumeDisabledReason(w)).toMatch(/only a paused worker can be resumed/i)
  })
})
