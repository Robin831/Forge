import { describe, expect, it } from 'vitest'
import {
  resumeWithMessageEligible,
  type EscalationDetail,
  type EscalationType,
} from './forge'

// A minimal EscalationDetail whose only relevant field for the gate is `branch`.
function detailWith(branch?: string): Pick<EscalationDetail, 'branch'> {
  return { branch }
}

describe('resumeWithMessageEligible', () => {
  const branched = detailWith('forge/Forge-abc1')

  it('is true for worker/dispatch classes when a branch is recorded', () => {
    const types: EscalationType[] = [
      'dispatch_failed',
      'smith_failed',
      'recovery_failed',
      'dispatch_blocked_stranded_branch',
      'pr_create_failed',
    ]
    for (const type of types) {
      expect(resumeWithMessageEligible(type, branched)).toBe(true)
    }
  })

  it('is false for clarification even when a branch is present', () => {
    expect(resumeWithMessageEligible('clarification', branched)).toBe(false)
  })

  it('is false when no branch was recorded', () => {
    expect(resumeWithMessageEligible('smith_failed', detailWith(undefined))).toBe(
      false,
    )
    expect(resumeWithMessageEligible('smith_failed', detailWith(''))).toBe(false)
    expect(resumeWithMessageEligible('smith_failed', detailWith('   '))).toBe(
      false,
    )
  })

  it('is false for a missing detail', () => {
    expect(resumeWithMessageEligible('smith_failed', null)).toBe(false)
    expect(resumeWithMessageEligible('smith_failed', undefined)).toBe(false)
  })
})
