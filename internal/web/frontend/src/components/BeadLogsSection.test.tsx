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

  it('folds one assay run into a single row naming each session by its pass', async () => {
    const runKey = '1755000000000'
    const session = (pass: string, seq: number, findings?: number) => ({
      filename: `assay-${runKey}-${pass}-${seq}-${seq}.log`,
      stage: 'assay',
      pass,
      run_key: runKey,
      findings,
      size_bytes: 64,
      mtime: `2026-08-25T10:0${seq}:00Z`,
      live: false,
    })
    useApiPollMock.mockReturnValue({
      data: {
        bead_id: 'Forge-test',
        files: [
          {
            filename: 'smith-1000-1.log',
            stage: 'smith',
            size_bytes: 128,
            mtime: '2026-08-25T09:00:00Z',
            live: false,
          },
          session('triage', 1, 0),
          session('logic', 2, 1),
          session('security', 3, 0),
        ],
        runs: [
          {
            run_key: runKey,
            run_id: 953,
            has_summary: true,
            started_at: '2026-08-25T10:01:00Z',
            status: 'complete',
            completed_passes: 5,
            total_passes: 5,
            findings_count: 1,
            cost_usd: 8.75,
            duration_ms: 213700,
            files: [
              `assay-${runKey}-triage-1-1.log`,
              `assay-${runKey}-logic-2-2.log`,
              `assay-${runKey}-security-3-3.log`,
            ],
          },
        ],
      } satisfies BeadLogsResponse,
      error: null,
      loading: false,
    })
    tailMock.mockResolvedValue({ lines: [assistantText('pass output')] })

    render(<BeadLogsSection beadID="Forge-test" />)

    // One row for the run, carrying the totals — not three "assay" rows.
    const runRow = screen.getByRole('button', { expanded: true })
    expect(runRow).toHaveTextContent('5/5 passes')
    expect(runRow).toHaveTextContent('1 finding')
    expect(runRow).toHaveTextContent('$8.75')
    expect(runRow).toHaveTextContent('214s')

    // Its sessions are named by pass, and the one that found something says so.
    expect(screen.getByRole('tab', { name: /triage/i })).toBeInTheDocument()
    const logicTab = screen.getByRole('tab', { name: /logic/i })
    expect(logicTab).toHaveTextContent('1 finding')
    expect(screen.getByRole('tab', { name: /security/i })).not.toHaveTextContent('finding')

    // Collapsing hides the sessions but keeps the run row.
    await userEvent.click(runRow)
    expect(screen.queryByRole('tab', { name: /triage/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { expanded: false })).toHaveTextContent('5/5 passes')
  })

  it('lists a run with no record yet without claiming it cost nothing', () => {
    const runKey = '1755000000000'
    useApiPollMock.mockReturnValue({
      data: {
        bead_id: 'Forge-test',
        files: [
          {
            filename: `assay-${runKey}-triage-1-1.log`,
            stage: 'assay',
            pass: 'triage',
            run_key: runKey,
            size_bytes: 64,
            mtime: '2026-08-25T10:01:00Z',
            live: false,
          },
        ],
        runs: [
          {
            run_key: runKey,
            has_summary: false,
            started_at: '2026-08-25T10:01:00Z',
            completed_passes: 0,
            total_passes: 0,
            findings_count: 0,
            cost_usd: 0,
            duration_ms: 0,
            files: [`assay-${runKey}-triage-1-1.log`],
          },
        ],
      } satisfies BeadLogsResponse,
      error: null,
      loading: false,
    })
    tailMock.mockResolvedValue({ lines: [assistantText('pass output')] })

    render(<BeadLogsSection beadID="Forge-test" />)

    const runRow = screen.getByRole('button', { expanded: true })
    expect(runRow).toHaveTextContent('summary pending')
    expect(runRow).not.toHaveTextContent('$0.00')
  })

  it('lists an assay log written before passes were named, labelled by stage', async () => {
    useApiPollMock.mockReturnValue({
      data: {
        bead_id: 'Forge-test',
        files: [
          {
            filename: 'assay-1730000000-3.log',
            stage: 'assay',
            size_bytes: 64,
            mtime: '2026-08-25T10:01:00Z',
            live: false,
          },
        ],
      } satisfies BeadLogsResponse,
      error: null,
      loading: false,
    })
    tailMock.mockResolvedValue({ lines: [assistantText('legacy assay output')] })

    render(<BeadLogsSection beadID="Forge-test" />)

    expect(screen.getByRole('tab', { name: /assay/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { expanded: true })).not.toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('log').textContent).toContain('legacy assay output'))
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
