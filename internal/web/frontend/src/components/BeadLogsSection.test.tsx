import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { BeadLogsResponse } from '../api'

const { useApiPollMock, tailMock, useEventSourceMock } = vi.hoisted(() => ({
  useApiPollMock: vi.fn(),
  tailMock: vi.fn(),
  useEventSourceMock: vi.fn(),
}))

vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: (path: string, intervalMs?: number) => useApiPollMock(path, intervalMs),
}))

vi.mock('../hooks/useEventSource', () => ({
  useEventSource: (url: string | null, opts?: unknown) => useEventSourceMock(url, opts),
}))

vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  beadLogs: {
    list: vi.fn(),
    tail: (beadID: string, filename: string, tail?: number) => tailMock(beadID, filename, tail),
  },
}))

import BeadLogsSection from './BeadLogsSection'

function line(obj: unknown): string {
  return JSON.stringify(obj)
}

function assistantText(text: string): string {
  return line({ type: 'assistant', message: { content: [{ type: 'text', text }] } })
}

const listResponse: BeadLogsResponse = {
  bead_id: 'Forge-test',
  files: [
    {
      filename: 'smith-1000-1.log',
      stage: 'smith',
      size_bytes: 128,
      mtime: '2026-07-13T12:01:00Z',
      live: false,
    },
    {
      filename: 'warden-2000-2.log',
      stage: 'warden',
      size_bytes: 256,
      mtime: '2026-07-13T12:02:00Z',
      live: false,
    },
  ],
}

beforeEach(() => {
  useEventSourceMock.mockReturnValue({ items: [], status: 'closed', error: null, clear: () => {} })
  useApiPollMock.mockReturnValue({ data: listResponse, error: null, loading: false })
  tailMock.mockImplementation((_bead: string, filename: string) => {
    if (filename.startsWith('warden')) {
      return Promise.resolve({ lines: [assistantText('warden output')] })
    }
    return Promise.resolve({ lines: [assistantText('smith output')] })
  })
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('BeadLogsSection', () => {
  it('lists stage files and auto-selects the most recent, rendering its transcript', async () => {
    render(<BeadLogsSection beadID="Forge-test" />)

    // Both stage tabs are listed.
    expect(screen.getByRole('tab', { name: /smith/i })).toBeInTheDocument()
    const wardenTab = screen.getByRole('tab', { name: /warden/i })
    expect(wardenTab).toHaveAttribute('aria-selected', 'true')

    // The most recent file (warden) is tailed and rendered.
    await waitFor(() => expect(screen.getByRole('log').textContent).toContain('warden output'))
    expect(tailMock).toHaveBeenCalledWith('Forge-test', 'warden-2000-2.log', 500)
  })

  it('switches to a different stage file on click', async () => {
    render(<BeadLogsSection beadID="Forge-test" />)
    await waitFor(() => expect(screen.getByRole('log').textContent).toContain('warden output'))

    await userEvent.click(screen.getByRole('tab', { name: /smith/i }))

    await waitFor(() => expect(screen.getByRole('log').textContent).toContain('smith output'))
    expect(tailMock).toHaveBeenCalledWith('Forge-test', 'smith-1000-1.log', 500)
  })

  it('streams a live file via the worker SSE stream instead of tailing', async () => {
    useApiPollMock.mockReturnValue({
      data: {
        bead_id: 'Forge-test',
        files: [
          {
            filename: 'smith-9000-1.log',
            stage: 'smith',
            size_bytes: 10,
            mtime: '2026-07-13T12:05:00Z',
            live: true,
            worker_id: 'anvil-Forge-test-1',
          },
        ],
      } satisfies BeadLogsResponse,
      error: null,
      loading: false,
    })
    useEventSourceMock.mockReturnValue({
      items: [{ line: assistantText('live streaming'), timestamp: '' }],
      status: 'open',
      error: null,
      clear: () => {},
    })

    render(<BeadLogsSection beadID="Forge-test" />)

    // The live file is streamed, not tailed.
    await waitFor(() => expect(screen.getByRole('log').textContent).toContain('live streaming'))
    expect(tailMock).not.toHaveBeenCalled()
    expect(useEventSourceMock).toHaveBeenCalledWith(
      '/api/worker/anvil-Forge-test-1/stream',
      expect.anything(),
    )
  })

  it('shows an empty state when the bead has no logs', () => {
    useApiPollMock.mockReturnValue({
      data: { bead_id: 'Forge-test', files: [] } satisfies BeadLogsResponse,
      error: null,
      loading: false,
    })
    render(<BeadLogsSection beadID="Forge-test" />)
    expect(screen.getByText(/no stage logs recorded/i)).toBeInTheDocument()
  })
})
