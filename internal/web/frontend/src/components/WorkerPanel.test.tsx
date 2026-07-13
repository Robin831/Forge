import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { WorkerInfo } from '../api'
import { KEY_PREFIX } from '../hooks/useUIState'

const { useEventSourceMock, killWorkerMock, pauseMock, resumeMock } = vi.hoisted(() => ({
  useEventSourceMock: vi.fn(),
  killWorkerMock: vi.fn(),
  pauseMock: vi.fn(),
  resumeMock: vi.fn(),
}))

vi.mock('../hooks/useEventSource', () => ({
  useEventSource: (url: string | null, opts?: unknown) => useEventSourceMock(url, opts),
}))

vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  actions: {
    ...(await importOriginal<typeof import('../api')>()).actions,
    killWorker: (id: string) => killWorkerMock(id),
    pause: (beadID: string) => pauseMock(beadID),
    resume: (beadID: string, message?: string) => resumeMock(beadID, message),
  },
}))

import WorkerPanel, { formatElapsed } from './WorkerPanel'

function worker(overrides: Partial<WorkerInfo>): WorkerInfo {
  return {
    id: 'w1',
    bead_id: 'Forge-abc1',
    anvil: 'forge',
    status: 'running',
    started_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

function renderPanel(w: WorkerInfo, props: Partial<Parameters<typeof WorkerPanel>[0]> = {}) {
  return render(
    <MemoryRouter>
      <WorkerPanel worker={w} {...props} />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  useEventSourceMock.mockReturnValue({ items: [], status: 'open', error: null, clear: () => {} })
  killWorkerMock.mockResolvedValue({})
  pauseMock.mockResolvedValue({})
  resumeMock.mockResolvedValue({})
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('WorkerPanel collapse', () => {
  it('opens the SSE stream for an expanded active worker', () => {
    renderPanel(worker({ id: 'w1', status: 'running' }))
    expect(useEventSourceMock).toHaveBeenCalledWith(
      '/api/worker/w1/stream',
      expect.objectContaining({ maxItems: 1000 }),
    )
  })

  it('collapses on toggle, shows the preview, and closes the stream (url=null)', async () => {
    const user = userEvent.setup()
    renderPanel(worker({ id: 'w1', status: 'running' }))

    const toggle = screen.getByTestId('worker-panel-toggle-w1')
    expect(toggle).toHaveAttribute('aria-expanded', 'true')

    await user.click(toggle)

    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(screen.getByTestId('worker-panel-preview-w1')).toBeInTheDocument()
    // Latest call after collapsing passes a null url so the hook tears the
    // EventSource down for the hidden panel.
    expect(useEventSourceMock).toHaveBeenLastCalledWith(null, expect.anything())
  })

  it('hydrates collapsed from seeded localStorage on first paint (no stream)', () => {
    localStorage.setItem(
      `${KEY_PREFIX}root.worker-panel.collapsed.w1`,
      JSON.stringify(true),
    )

    renderPanel(worker({ id: 'w1', status: 'running' }))

    expect(screen.getByTestId('worker-panel-toggle-w1')).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    // Never opens a stream while starting collapsed.
    expect(useEventSourceMock).toHaveBeenCalledWith(null, expect.anything())
    expect(useEventSourceMock).not.toHaveBeenCalledWith(
      '/api/worker/w1/stream',
      expect.anything(),
    )
  })
})

describe('WorkerPanel actions', () => {
  it('invokes onExpand with the worker when the expand button is clicked', async () => {
    const user = userEvent.setup()
    const onExpand = vi.fn()
    const w = worker({ id: 'w1' })
    renderPanel(w, { onExpand })

    await user.click(screen.getByTestId('worker-panel-expand-w1'))
    expect(onExpand).toHaveBeenCalledWith(w)
  })

  it('kills the worker through a confirm dialog', async () => {
    const user = userEvent.setup()
    renderPanel(worker({ id: 'w1', status: 'running' }))

    await user.click(screen.getByTestId('worker-panel-kill-w1'))
    // ConfirmModal is now open — its confirm button label is exactly
    // "Kill worker" (the header icon button is "Kill worker <id>").
    const confirm = await screen.findByRole('button', { name: 'Kill worker' })
    await user.click(confirm)

    expect(killWorkerMock).toHaveBeenCalledWith('w1')
  })

  it('hides the kill button for a paused worker but still shows the panel', () => {
    renderPanel(worker({ id: 'w1', status: 'paused' }))
    expect(screen.queryByTestId('worker-panel-kill-w1')).not.toBeInTheDocument()
    expect(screen.getByTestId('worker-panel-w1')).toBeInTheDocument()
  })
})

describe('WorkerPanel pause/resume', () => {
  it('pauses a running worker through a confirm dialog', async () => {
    const user = userEvent.setup()
    renderPanel(worker({ id: 'w1', status: 'running' }))

    // Only running workers expose a pause control (no resume).
    expect(screen.queryByTestId('worker-panel-resume-w1')).not.toBeInTheDocument()
    await user.click(screen.getByTestId('worker-panel-pause-w1'))

    const confirm = await screen.findByRole('button', { name: 'Pause worker' })
    await user.click(confirm)

    expect(pauseMock).toHaveBeenCalledWith('Forge-abc1')
  })

  it('resumes a paused worker without a confirm dialog', async () => {
    const user = userEvent.setup()
    renderPanel(worker({ id: 'w1', status: 'paused' }))

    // Paused workers expose resume, not pause.
    expect(screen.queryByTestId('worker-panel-pause-w1')).not.toBeInTheDocument()
    await user.click(screen.getByTestId('worker-panel-resume-w1'))

    expect(resumeMock).toHaveBeenCalledWith('Forge-abc1', undefined)
  })

  it('renders a distinct paused visual state with a frozen-stream note and visible transcript', () => {
    useEventSourceMock.mockReturnValue({
      items: [{ line: 'assistant: still thinking', timestamp: '' }],
      status: 'open',
      error: null,
      clear: () => {},
    })
    renderPanel(worker({ id: 'w1', status: 'paused' }))

    expect(screen.getByTestId('worker-panel-w1')).toHaveAttribute('data-paused', 'true')
    expect(screen.getByTestId('worker-panel-frozen-w1')).toHaveTextContent(/stream frozen/i)
    // Transcript stays visible while paused (no "no live output" placeholder).
    expect(screen.getByText(/still thinking/)).toBeInTheDocument()
  })
})

describe('WorkerPanel steer composer', () => {
  it('mounts an enabled steer composer for a running worker', () => {
    renderPanel(worker({ id: 'w1', status: 'running' }))
    expect(screen.getByLabelText('Steer message')).toBeEnabled()
  })

  it('disables the steer composer for a paused worker (not steerable)', () => {
    renderPanel(worker({ id: 'w1', status: 'paused' }))
    expect(screen.getByLabelText('Steer message')).toBeDisabled()
  })

  it('omits the steer composer when the panel is collapsed', async () => {
    const user = userEvent.setup()
    renderPanel(worker({ id: 'w1', status: 'running' }))
    expect(screen.getByLabelText('Steer message')).toBeInTheDocument()

    await user.click(screen.getByTestId('worker-panel-toggle-w1'))
    expect(screen.queryByLabelText('Steer message')).not.toBeInTheDocument()
  })
})

describe('formatElapsed', () => {
  const base = Date.parse('2024-01-01T00:00:00Z')
  it('formats seconds, minutes, and hours', () => {
    expect(formatElapsed('2024-01-01T00:00:00Z', base + 5_000)).toBe('5s')
    expect(formatElapsed('2024-01-01T00:00:00Z', base + 125_000)).toBe('2m 5s')
    expect(formatElapsed('2024-01-01T00:00:00Z', base + 3_900_000)).toBe('1h 5m')
  })

  it('falls back to an em dash for an unparseable timestamp', () => {
    expect(formatElapsed('not-a-date', base)).toBe('—')
  })
})
