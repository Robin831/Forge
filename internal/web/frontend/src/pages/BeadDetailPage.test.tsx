import '@testing-library/jest-dom/vitest'
import { describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import type { BeadDetailResponse, StatusResponse } from '../api'

const { useApiPollMock } = vi.hoisted(() => ({
  useApiPollMock: vi.fn(),
}))

vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: (path: string, intervalMs?: number) => useApiPollMock(path, intervalMs),
}))

import BeadDetailPage from './BeadDetailPage'

function detail(overrides: Partial<BeadDetailResponse> = {}): BeadDetailResponse {
  return {
    bead_id: 'Forge-test',
    anvil: 'forge',
    queue: {
      bead_id: 'Forge-test',
      anvil: 'forge',
      title: 'Example bead',
      description: 'a description',
      priority: 2,
      status: 'open',
      section: 'ready',
      labels: [],
    },
    workers: [],
    events: [],
    prs: [],
    blocks: [],
    blocked_by: [],
    comments: [],
    ...overrides,
  }
}

function status(): StatusResponse {
  return { running: true, pid: 1 }
}

function setApiResponses(beadDetail: BeadDetailResponse | null) {
  useApiPollMock.mockImplementation((path: string) => {
    if (path === '/api/status') {
      return { data: status(), error: null, loading: false }
    }
    return { data: beadDetail, error: null, loading: false }
  })
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={['/bead/Forge-test?anvil=forge']}>
      <Routes>
        <Route path="/bead/:bead_id" element={<BeadDetailPage />} />
      </Routes>
    </MemoryRouter>,
  )
}

describe('BeadDetailPage notes and comments panels', () => {
  it('renders the Notes and Comments panels with content when both are present', () => {
    const oneHourAgo = new Date(Date.now() - 60 * 60 * 1000).toISOString()
    const twoHoursAgo = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString()
    setApiResponses(
      detail({
        notes: 'first line\nsecond line',
        comments: [
          { id: 'c1', author: 'alice', body: 'Looks good', created_at: oneHourAgo },
          { id: 'c2', author: 'bob', body: 'one more thing', created_at: twoHoursAgo },
        ],
      }),
    )
    renderPage()

    const notesHeader = screen.getByRole('button', { name: 'Notes' })
    expect(notesHeader).toHaveAttribute('aria-expanded', 'true')
    const notesSection = notesHeader.closest('section')!
    expect(within(notesSection).getByText(/first line\s*second line/)).toBeInTheDocument()

    const commentsHeader = screen.getByRole('button', { name: /^Comments\s+2$/ })
    expect(commentsHeader).toHaveAttribute('aria-expanded', 'true')
    const commentsSection = commentsHeader.closest('section')!
    expect(within(commentsSection).getByText('alice')).toBeInTheDocument()
    expect(within(commentsSection).getByText('bob')).toBeInTheDocument()
    expect(within(commentsSection).getByText('Looks good')).toBeInTheDocument()
    expect(within(commentsSection).getByText('one more thing')).toBeInTheDocument()
    expect(within(commentsSection).getByText('1h ago')).toHaveAttribute('title', oneHourAgo)
    expect(within(commentsSection).getByText('2h ago')).toHaveAttribute('title', twoHoursAgo)
  })

  it('omits both panels when notes and comments are empty', () => {
    setApiResponses(detail({ notes: '', comments: [] }))
    renderPage()

    expect(screen.queryByRole('button', { name: 'Notes' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Comments/ })).not.toBeInTheDocument()
  })

  it('omits the Notes panel when notes is only whitespace', () => {
    setApiResponses(detail({ notes: '   \n  ', comments: [] }))
    renderPage()

    expect(screen.queryByRole('button', { name: 'Notes' })).not.toBeInTheDocument()
  })

  it('collapses a panel when its header is clicked', async () => {
    const user = userEvent.setup()
    setApiResponses(
      detail({
        notes: 'visible note',
        comments: [],
      }),
    )
    renderPage()

    const notesHeader = screen.getByRole('button', { name: 'Notes' })
    expect(notesHeader).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('visible note')).toBeInTheDocument()

    await user.click(notesHeader)
    expect(notesHeader).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('visible note')).not.toBeInTheDocument()
  })
})
