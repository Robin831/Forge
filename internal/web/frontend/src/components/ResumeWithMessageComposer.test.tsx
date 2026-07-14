import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const { resumeWithMessageMock } = vi.hoisted(() => ({
  resumeWithMessageMock: vi.fn(),
}))

vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  return {
    ...actual,
    actions: { ...actual.actions, resumeWithMessage: resumeWithMessageMock },
  }
})

import ResumeWithMessageComposer from './ResumeWithMessageComposer'

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('ResumeWithMessageComposer', () => {
  it('POSTs a trimmed operator message and clears the input', async () => {
    resumeWithMessageMock.mockResolvedValue({ worker_id: 'w-1' })
    const onResumed = vi.fn()
    render(
      <ResumeWithMessageComposer
        beadID="Forge-abc1"
        branch="forge/Forge-abc1"
        onResumed={onResumed}
      />,
    )

    const input = screen.getByLabelText('Resume with message') as HTMLTextAreaElement
    await userEvent.type(input, '  focus on the failing test  ')
    await userEvent.click(
      screen.getByRole('button', { name: /resume with message/i }),
    )

    expect(resumeWithMessageMock).toHaveBeenCalledWith(
      'Forge-abc1',
      'focus on the failing test',
    )
    expect(onResumed).toHaveBeenCalledTimes(1)
    expect(input.value).toBe('')
  })

  it('does not submit an empty message', async () => {
    render(<ResumeWithMessageComposer beadID="Forge-abc1" />)
    const button = screen.getByRole('button', { name: /resume with message/i })
    expect(button).toBeDisabled()
    await userEvent.click(button)
    expect(resumeWithMessageMock).not.toHaveBeenCalled()
  })

  it('renders the surviving branch so the operator knows the resume target', () => {
    render(
      <ResumeWithMessageComposer beadID="Forge-abc1" branch="forge/Forge-abc1" />,
    )
    expect(screen.getByText('forge/Forge-abc1')).toBeInTheDocument()
  })
})
