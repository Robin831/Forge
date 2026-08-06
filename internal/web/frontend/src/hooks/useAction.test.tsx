import { describe, expect, it } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { ApiError } from '../api'
import { useAction } from './useAction'
import { ToastProvider, useToast, type Toast } from './useToast'

function wrapper({ children }: { children: ReactNode }) {
  return <ToastProvider>{children}</ToastProvider>
}

// renderAction exposes both the action runner and the live toast list so each
// case can assert on the variant the user actually sees.
function renderAction() {
  return renderHook(
    () => ({ action: useAction(), toasts: useToast().toasts as Toast[] }),
    { wrapper },
  )
}

describe('useAction', () => {
  it('confirms a successful action with a success toast', async () => {
    const { result } = renderAction()

    let ok = false
    await act(async () => {
      ok = await result.current.action.run(async () => ({ tag: 'forgeReady' }), {
        successMessage: 'Applied forgeReady to Forge-abc1',
      })
    })

    expect(ok).toBe(true)
    expect(result.current.toasts).toHaveLength(1)
    expect(result.current.toasts[0]).toMatchObject({
      variant: 'success',
      message: 'Applied forgeReady to Forge-abc1',
    })
  })

  it('surfaces a failed action as an error toast, never as success', async () => {
    const { result } = renderAction()

    let ok = true
    await act(async () => {
      ok = await result.current.action.run(
        async () => {
          throw new ApiError(500, 'bd update failed: exit status 1')
        },
        { successMessage: 'Applied forgeReady to Forge-abc1' },
      )
    })

    expect(ok).toBe(false)
    expect(result.current.toasts).toHaveLength(1)
    expect(result.current.toasts[0]).toMatchObject({
      variant: 'error',
      message: 'bd update failed: exit status 1',
    })
  })

  // Forge-4r2n: an action whose queued outcome could not be resolved must read
  // as "queued, outcome unknown" — not as a completed write.
  it('reports an unresolved queued action neutrally', async () => {
    const { result } = renderAction()

    let ok = true
    await act(async () => {
      ok = await result.current.action.run(
        async () => ({ queued: true, queued_unresolved: true, queued_state: 'unknown' }),
        { successMessage: 'Applied forgeReady to Forge-abc1' },
      )
    })

    expect(ok).toBe(false)
    expect(result.current.toasts).toHaveLength(1)
    expect(result.current.toasts[0].variant).not.toBe('success')
    expect(result.current.toasts[0].message).toMatch(/queued, outcome unknown/i)
  })
})
