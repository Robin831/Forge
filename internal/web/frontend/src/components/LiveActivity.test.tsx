import '@testing-library/jest-dom/vitest'
import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { EventInfo } from '../api'

const useEventSourceMock = vi.fn()

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

describe('LiveActivity', () => {
  it('caps the initial render at 10 items and shows Fetch more', () => {
    setEvents(makeEvents(25))
    render(<LiveActivity />)
    expect(screen.getAllByRole('listitem')).toHaveLength(10)
    expect(screen.getByRole('button', { name: 'Fetch more' })).toBeInTheDocument()
  })

  it('renders the newest event at the top of the visible slice', () => {
    setEvents(makeEvents(25))
    render(<LiveActivity />)
    const rows = screen.getAllByRole('listitem')
    // The newest event has id 25 ("event 25") and should be first.
    expect(rows[0]).toHaveTextContent('event 25')
    expect(rows[9]).toHaveTextContent('event 16')
  })

  it('reveals 10 more items per Fetch more click and hides the button when fully expanded', async () => {
    const user = userEvent.setup()
    setEvents(makeEvents(25))
    render(<LiveActivity />)

    await user.click(screen.getByRole('button', { name: 'Fetch more' }))
    expect(screen.getAllByRole('listitem')).toHaveLength(20)
    expect(screen.getByRole('button', { name: 'Fetch more' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Fetch more' }))
    expect(screen.getAllByRole('listitem')).toHaveLength(25)
    expect(screen.queryByRole('button', { name: 'Fetch more' })).not.toBeInTheDocument()
  })

  it('does not render Fetch more when the buffer already fits within the cap', () => {
    setEvents(makeEvents(7))
    render(<LiveActivity />)
    expect(screen.getAllByRole('listitem')).toHaveLength(7)
    expect(screen.queryByRole('button', { name: 'Fetch more' })).not.toBeInTheDocument()
  })
})
