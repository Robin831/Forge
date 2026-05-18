import '@testing-library/jest-dom/vitest'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { EventInfo } from '../api'

const { useEventSourceMock } = vi.hoisted(() => ({
  useEventSourceMock: vi.fn(),
}))

vi.mock('../hooks/useEventSource', () => ({
  useEventSource: (url: string, opts?: unknown) => useEventSourceMock(url, opts),
}))

import LiveActivity from './LiveActivity'

function makeEvents(count: number): EventInfo[] {
  // Buffer order matches what useEventSource produces: oldest first, newest
  // last. LiveActivity reverses this before rendering, so the highest id
  // ends up at the top of the rendered list.
  return Array.from({ length: count }, (_, i) => ({
    id: i + 1,
    timestamp: new Date(2026, 0, 1, 0, 0, i).toISOString(),
    type: 'bead_claimed',
    message: `event ${i + 1}`,
  }))
}

function setEvents(items: EventInfo[]) {
  useEventSourceMock.mockReturnValue({
    items,
    status: 'open',
    error: null,
    clear: () => {},
  })
}

function renderPane() {
  return render(
    <MemoryRouter>
      <LiveActivity />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  // LiveActivity persists paused/filter via useUIState. Start every test with
  // empty storage so defaults are deterministic.
  sessionStorage.clear()
  localStorage.clear()
})

describe('LiveActivity', () => {
  it('caps the initial render at 10 items and shows Fetch more', () => {
    setEvents(makeEvents(25))
    renderPane()
    expect(screen.getAllByRole('listitem')).toHaveLength(10)
    expect(screen.getByRole('button', { name: 'Fetch more' })).toBeInTheDocument()
  })

  it('renders the newest event at the top of the visible slice', () => {
    setEvents(makeEvents(25))
    renderPane()
    const rows = screen.getAllByRole('listitem')
    // The newest event has id 25 ("event 25") and should be first.
    expect(rows[0]).toHaveTextContent('event 25')
    expect(rows[9]).toHaveTextContent('event 16')
  })

  it('reveals 10 more items per Fetch more click and hides the button when fully expanded', async () => {
    const user = userEvent.setup()
    setEvents(makeEvents(25))
    renderPane()

    await user.click(screen.getByRole('button', { name: 'Fetch more' }))
    expect(screen.getAllByRole('listitem')).toHaveLength(20)
    expect(screen.getByRole('button', { name: 'Fetch more' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Fetch more' }))
    expect(screen.getAllByRole('listitem')).toHaveLength(25)
    expect(screen.queryByRole('button', { name: 'Fetch more' })).not.toBeInTheDocument()
  })

  it('does not render Fetch more when the buffer already fits within the cap', () => {
    setEvents(makeEvents(7))
    renderPane()
    expect(screen.getAllByRole('listitem')).toHaveLength(7)
    expect(screen.queryByRole('button', { name: 'Fetch more' })).not.toBeInTheDocument()
  })

  it('filters events by type, message, bead id, or anvil', async () => {
    const user = userEvent.setup()
    setEvents([
      { id: 1, timestamp: '2026-01-01T00:00:00Z', type: 'bead_claimed', message: 'alpha', bead_id: 'bd-1', anvil: 'forge' },
      { id: 2, timestamp: '2026-01-01T00:00:01Z', type: 'smith_done', message: 'beta', bead_id: 'bd-2', anvil: 'heimdall' },
      { id: 3, timestamp: '2026-01-01T00:00:02Z', type: 'warden_pass', message: 'gamma', bead_id: 'bd-3', anvil: 'forge' },
    ])
    renderPane()

    expect(screen.getAllByRole('listitem')).toHaveLength(3)

    await user.type(screen.getByRole('textbox', { name: /Filter events/ }), 'smith')
    const rows = screen.getAllByRole('listitem')
    expect(rows).toHaveLength(1)
    expect(rows[0]).toHaveTextContent('beta')
  })

  it('pauses live updates so subsequent events stay hidden until resumed', async () => {
    const user = userEvent.setup()
    const initial = makeEvents(3)
    setEvents(initial)
    const { rerender } = renderPane()
    expect(screen.getAllByRole('listitem')).toHaveLength(3)

    // Toggle pause — pauseSnapshot freezes the visible list at the current 3.
    await user.click(screen.getByRole('button', { name: /Pause live updates/ }))

    // Simulate a new SSE batch arriving after pause.
    setEvents(makeEvents(10))
    rerender(
      <MemoryRouter>
        <LiveActivity />
      </MemoryRouter>,
    )

    // Still 3 — the snapshot held the line.
    expect(screen.getAllByRole('listitem')).toHaveLength(3)

    // Resume — the snapshot clears so the live buffer takes over.
    await user.click(screen.getByRole('button', { name: /Resume live updates/ }))
    expect(screen.getAllByRole('listitem')).toHaveLength(10)
  })
})
