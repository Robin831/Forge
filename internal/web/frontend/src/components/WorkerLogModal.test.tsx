import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { WorkerInfo } from '../api'

const { apiGetMock, useEventSourceMock } = vi.hoisted(() => ({
  apiGetMock: vi.fn(),
  useEventSourceMock: vi.fn(),
}))

vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  apiGet: (path: string) => apiGetMock(path),
}))

vi.mock('../hooks/useEventSource', () => ({
  useEventSource: (url: string | null, opts?: unknown) => useEventSourceMock(url, opts),
}))

vi.mock('../auth', () => ({
  useAuth: () => ({ logout: vi.fn() }),
}))

import WorkerLogModal from './WorkerLogModal'

const completedWorker: WorkerInfo = {
  id: 'w1',
  bead_id: 'Forge-abc1',
  anvil: 'forge',
  title: 'Test worker',
  status: 'succeeded',
  started_at: '2024-01-01T00:00:00Z',
}

function line(obj: unknown): string {
  return JSON.stringify(obj)
}

beforeEach(() => {
  useEventSourceMock.mockReturnValue({ items: [], status: 'closed', error: null, clear: () => {} })
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('WorkerLogModal transcript rendering', () => {
  it('collapses a long tool result and expands it on click', async () => {
    apiGetMock.mockResolvedValue({
      lines: [
        line({
          type: 'assistant',
          message: {
            content: [
              { type: 'tool_use', id: 't1', name: 'Read', input: { file_path: '/repo/a.ts' } },
            ],
          },
        }),
        line({
          type: 'user',
          message: {
            content: [
              { type: 'tool_result', tool_use_id: 't1', content: 'l1\nl2\nl3\nl4\nl5' },
            ],
          },
        }),
      ],
    })

    render(<WorkerLogModal worker={completedWorker} onClose={() => {}} />)

    // Headline renders once loaded.
    await screen.findByText('Read')

    const log = screen.getByRole('log')
    expect(log.textContent).toContain('l3')
    // Collapsed: later lines hidden behind the expander.
    expect(log.textContent).not.toContain('l5')

    const expander = screen.getByRole('button', { name: /\+2 lines/i })
    await userEvent.click(expander)
    expect(log.textContent).toContain('l5')
  })

  it('hides noise by default and reveals it via the verbose toggle', async () => {
    apiGetMock.mockResolvedValue({
      lines: [
        line({ type: 'system', subtype: 'thinking_tokens', tokens: 42 }),
        line({ type: 'assistant', message: { content: [{ type: 'text', text: 'visible text' }] } }),
      ],
    })

    render(<WorkerLogModal worker={completedWorker} onClose={() => {}} />)

    await screen.findByText('visible text')
    const log = screen.getByRole('log')
    expect(log.textContent).not.toContain('thinking_tokens')

    const verbose = screen.getByRole('checkbox', { name: /verbose/i })
    await userEvent.click(verbose)

    await waitFor(() => expect(log.textContent).toContain('thinking_tokens'))
  })

  it('renders the final result event as a summary line', async () => {
    apiGetMock.mockResolvedValue({
      lines: [
        line({
          type: 'result',
          duration_ms: 65000,
          num_turns: 3,
          total_cost_usd: 0.5,
          usage: { input_tokens: 10, output_tokens: 20 },
        }),
      ],
    })

    render(<WorkerLogModal worker={completedWorker} onClose={() => {}} />)

    await waitFor(() => {
      const log = screen.getByRole('log')
      expect(log.textContent).toContain('3 turns')
      expect(log.textContent).toContain('$0.5000')
      expect(log.textContent).toContain('1m 5s')
    })
  })
})
