import { describe, expect, it } from 'vitest'
import { steerDisabledReason, steerIsResumeDelivery, type Steerable } from './workerStatus'

describe('steerDisabledReason', () => {
  it('rejects a missing worker as having no active pipeline', () => {
    expect(steerDisabledReason(null)).toMatch(/no active pipeline/i)
    expect(steerDisabledReason(undefined)).toMatch(/no active pipeline/i)
  })

  it('rejects a completed worker as having no active pipeline', () => {
    const w: Steerable = { status: 'succeeded', session_id: 'sess-1', model: 'claude-opus-4-6' }
    expect(steerDisabledReason(w)).toMatch(/no active pipeline/i)
  })

  it('rejects terminal statuses (done/failed/killed) as having no active pipeline', () => {
    // 'stalled' is deliberately absent: it is not terminal, it is a watchdog
    // mask over a live row — see the stalled case below.
    for (const status of ['done', 'failed', 'killed', 'timeout', 'monitoring']) {
      const w: Steerable = { status, session_id: 'sess-1', model: 'claude-opus-4-6' }
      expect(steerDisabledReason(w)).toMatch(/no active pipeline/i)
    }
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

  it('allows a reviewing worker (Warden mode-B queue)', () => {
    // No Smith spawn is live during Warden review, but the daemon still accepts
    // the steer and queues it for the next spawn (mode B).
    const w: Steerable = { status: 'reviewing', session_id: 'sess-1', model: 'claude-opus-4-6' }
    expect(steerDisabledReason(w)).toBeNull()
  })

  it('allows a paused worker (delivered as resume-with-message)', () => {
    const w: Steerable = { status: 'paused', session_id: 'sess-1', model: 'claude-opus-4-6' }
    expect(steerDisabledReason(w)).toBeNull()
  })

  it('allows a stalled worker — its process is running and the daemon accepts', () => {
    // The watchdog writes 'stalled' over a live row when its LOG goes quiet;
    // the pipeline goroutine and its steer mailbox are untouched, and the
    // daemon gates on that handle rather than on the status. Disabled, the
    // composer claimed 'no active pipeline' about a worker whose process is
    // still running.
    const w: Steerable = { status: 'stalled', session_id: 'sess-1', model: 'claude-opus-4-6' }
    expect(steerDisabledReason(w)).toBeNull()
  })

  it('rejects a stalled non-Claude session like any other', () => {
    const w: Steerable = { status: 'stalled', model: 'gemini-2.5-pro' }
    expect(steerDisabledReason(w)).toMatch(/not a claude session/i)
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

  it('rejects a non-Claude session even when paused (resume would not resume it)', () => {
    const w: Steerable = { status: 'paused', model: 'gemini-2.5-pro' }
    expect(steerDisabledReason(w)).toMatch(/not a claude session/i)
  })

  it('treats a non-Claude model with a captured session as steerable', () => {
    // Defensive: only Claude reports a session_id, so a present session id wins
    // over an odd model string (mirrors workerSessionNonClaude short-circuit).
    const w: Steerable = { status: 'running', session_id: 'sess-1', model: 'gemini-2.5-pro' }
    expect(steerDisabledReason(w)).toBeNull()
  })
})

describe('steerIsResumeDelivery', () => {
  it('is true only for a paused worker', () => {
    expect(steerIsResumeDelivery({ status: 'paused' })).toBe(true)
  })

  it('is false for running/pending/reviewing/stalled and terminal statuses', () => {
    // A stalled worker is steered through the steer endpoint, not resumed:
    // nothing parked it, so there is no resume for a message to ride on.
    for (const status of ['running', 'pending', 'reviewing', 'stalled', 'done', 'failed']) {
      expect(steerIsResumeDelivery({ status })).toBe(false)
    }
  })

  it('is false for a missing worker', () => {
    expect(steerIsResumeDelivery(null)).toBe(false)
    expect(steerIsResumeDelivery(undefined)).toBe(false)
  })
})
