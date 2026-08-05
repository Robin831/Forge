import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'
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

  it('omits the Notes panel when notes is empty; Comments panel stays for the composer', () => {
    setApiResponses(detail({ notes: '', comments: [] }))
    renderPage()

    expect(screen.queryByRole('button', { name: 'Notes' })).not.toBeInTheDocument()
    // The Comments panel hosts the composer, so it renders even when the
    // server-side comment list is empty (provided we know the anvil).
    expect(screen.getByRole('button', { name: /^Comments\s+0$/ })).toBeInTheDocument()
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

describe('BeadDetailPage comment composer', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the composer even when there are no comments and the anvil is known', () => {
    setApiResponses(detail({ comments: [] }))
    renderPage()

    expect(screen.getByRole('button', { name: /^Comments\s+0$/ })).toBeInTheDocument()
    expect(screen.getByLabelText('Add a comment')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /add comment/i })).toBeDisabled()
  })

  it('posts the typed body and appends the returned comment to the list', async () => {
    const user = userEvent.setup()
    setApiResponses(detail({ comments: [] }))

    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(
        JSON.stringify({
          comment: {
            id: 'c-new',
            author: 'Test User',
            body: 'Looks great',
            created_at: new Date().toISOString(),
          },
        }),
        { status: 201, headers: { 'Content-Type': 'application/json' } },
      ),
    )

    renderPage()

    const textarea = screen.getByLabelText('Add a comment') as HTMLTextAreaElement
    await user.type(textarea, 'Looks great')

    const submit = screen.getByRole('button', { name: /add comment/i })
    expect(submit).toBeEnabled()
    await user.click(submit)

    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledTimes(1)
    })

    const [url, init] = fetchSpy.mock.calls[0]
    expect(url).toBe('/api/bead/Forge-test/comment')
    expect((init as RequestInit | undefined)?.method).toBe('POST')
    const body = JSON.parse(((init as RequestInit | undefined)?.body as string) ?? '{}')
    expect(body).toEqual({ anvil: 'forge', body: 'Looks great' })
    expect((init as RequestInit | undefined)?.headers).toMatchObject({
      'X-Forge-Action': '1',
      'Content-Type': 'application/json',
    })

    const commentsHeader = await screen.findByRole('button', { name: /^Comments\s+1$/ })
    const commentsSection = commentsHeader.closest('section')!
    expect(within(commentsSection).getByText('Looks great')).toBeInTheDocument()
    expect(within(commentsSection).getByText('Test User')).toBeInTheDocument()

    expect((screen.getByLabelText('Add a comment') as HTMLTextAreaElement).value).toBe('')
  })

  it('surfaces an inline error and keeps the draft on failure', async () => {
    const user = userEvent.setup()
    setApiResponses(detail({ comments: [] }))

    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ error: 'bd comments add failed: exit status 1' }), {
        status: 502,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    renderPage()

    const textarea = screen.getByLabelText('Add a comment') as HTMLTextAreaElement
    await user.type(textarea, 'will fail')
    await user.click(screen.getByRole('button', { name: /add comment/i }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/bd comments add failed/i)
    // Draft should not have been cleared, so the user can retry without
    // retyping the comment.
    expect(textarea.value).toBe('will fail')
    // Comments count should remain zero since no optimistic append happened.
    expect(screen.getByRole('button', { name: /^Comments\s+0$/ })).toBeInTheDocument()
  })
})
