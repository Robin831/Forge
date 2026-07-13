import { describe, expect, it } from 'vitest'
import { steerDisabledReason, type Steerable } from './api'

describe('steerDisabledReason', () => {
  it('rejects a missing worker as having no active pipeline', () => {
    expect(steerDisabledReason(null)).toMatch(/no active pipeline/i)
    expect(steerDisabledReason(undefined)).toMatch(/no active pipeline/i)
  })

  it('rejects a completed worker as having no active pipeline', () => {
    const w: Steerable = { status: 'succeeded', session_id: 'sess-1', model: 'claude-opus-4-6' }
    expect(steerDisabledReason(w)).toMatch(/no active pipeline/i)
  })

  it('allows a running Claude worker with a captured session', () => {
    const w: Steerable = { status: 'running', session_id: 'sess-1', model: 'claude-opus-4-6' }
    expect(steerDisabledReason(w)).toBeNull()
  })

  it('allows a pending worker whose session is not yet recorded', () => {
    // Both session_id and model empty — spawn still starting. The daemon is
    // optimistic here, so the UI must be too.
    const w: Steerable = { status: 'pending' }
    expect(steerDisabledReason(w)).toBeNull()
  })

  it('allows a running Claude worker before its session id is captured', () => {
    // model recorded but no session_id yet — still Claude, still steerable.
    const w: Steerable = { status: 'running', model: 'claude-sonnet-4-6' }
    expect(steerDisabledReason(w)).toBeNull()
  })

  it('rejects a positively non-Claude session', () => {
    const w: Steerable = { status: 'running', model: 'gemini-2.5-pro' }
    const reason = steerDisabledReason(w)
    expect(reason).toMatch(/not a claude session/i)
    expect(reason).toContain('gemini-2.5-pro')
  })

  it('treats a non-Claude model with a captured session as steerable', () => {
    // Defensive: only Claude reports a session_id, so a present session id wins
    // over an odd model string (mirrors workerSessionNonClaude short-circuit).
    const w: Steerable = { status: 'running', session_id: 'sess-1', model: 'gemini-2.5-pro' }
    expect(steerDisabledReason(w)).toBeNull()
  })
})
