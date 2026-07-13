import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const { steerMock } = vi.hoisted(() => ({
  steerMock: vi.fn(),
}))

vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  return {
    ...actual,
    actions: { ...actual.actions, steer: steerMock },
  }
})

import SteerComposer from './SteerComposer'

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('SteerComposer', () => {
  it('sends a trimmed steer message and clears the input', async () => {
    steerMock.mockResolvedValue({ message: 'ok' })
    render(<SteerComposer beadID="Forge-abc1" disabledReason={null} />)

    const input = screen.getByLabelText('Steer message') as HTMLInputElement
    await userEvent.type(input, '  focus on the failing test  ')
    await userEvent.click(screen.getByRole('button', { name: /steer/i }))

    expect(steerMock).toHaveBeenCalledWith('Forge-abc1', 'focus on the failing test')
    expect(input.value).toBe('')
  })

  it('clears the draft on Escape without submitting', async () => {
    render(<SteerComposer beadID="Forge-abc1" disabledReason={null} />)
    const input = screen.getByLabelText('Steer message') as HTMLInputElement
    await userEvent.type(input, 'wip draft')
    expect(input.value).toBe('wip draft')
    await userEvent.type(input, '{Escape}')
    expect(input.value).toBe('')
    expect(steerMock).not.toHaveBeenCalled()
  })

  it('does not submit an empty message', async () => {
    render(<SteerComposer beadID="Forge-abc1" disabledReason={null} />)
    const button = screen.getByRole('button', { name: /steer/i })
    expect(button).toBeDisabled()
    await userEvent.click(button)
    expect(steerMock).not.toHaveBeenCalled()
  })

  it('disables the input and shows the reason when not steerable', () => {
    render(
      <SteerComposer
        beadID="Forge-abc1"
        disabledReason="Not a Claude session (model gemini) — steering is only supported for Claude sessions."
      />,
    )
    expect(screen.getByLabelText('Steer message')).toBeDisabled()
    expect(screen.getByRole('button', { name: /steer/i })).toBeDisabled()
    expect(screen.getByText(/not a claude session/i)).toBeInTheDocument()
  })

  it('keeps the input disabled so a disabled composer cannot steer', async () => {
    render(<SteerComposer beadID="Forge-abc1" disabledReason="No active pipeline — steering requires a running Smith worker." />)
    await userEvent.type(screen.getByLabelText('Steer message'), 'hello').catch(() => {})
    await userEvent.click(screen.getByRole('button', { name: /steer/i }))
    expect(steerMock).not.toHaveBeenCalled()
  })
})
