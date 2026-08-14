import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { PreviewsListResponse, PreviewSummary } from '../api/previews'
import { resetPreviewsStore } from '../hooks/usePreview'
import { ToastProvider } from '../hooks/useToast'

// The page's own /api/status poll is irrelevant here — stub it so the test
// drives nothing but the shared previews store.
vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: () => ({ data: { running: true }, loading: false, error: null }),
}))

import PreviewsPage from './PreviewsPage'

const NOW = '2026-08-06T10:00:30Z'

let previews: PreviewsListResponse
let posts: Array<{ url: string; body: unknown }>
// When set, a preview start is refused with this message instead of queued.
let startRejection: string | null

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function preview(overrides: Partial<PreviewSummary> = {}): PreviewSummary {
  return {
    bead_id: 'Forge-abc1',
    anvil: 'forge',
    branch: 'forge/Forge-abc1',
    status: 'running',
    services: [
      {
        name: 'web',
        port: 42001,
        health: 'healthy',
        entry: true,
        uptime_seconds: 95,
        log_url: '/api/preview/Forge-abc1/log/web',
      },
    ],
    entry_url: 'http://forge-box:42001/',
    created_at: '2026-08-06T09:58:55Z',
    last_active_at: '2026-08-06T09:58:55Z',
    idle_deadline: '2026-08-06T10:08:42Z',
    // The same deadline the daemon reports as a countdown: 8m 12s from NOW.
    idle_remaining_seconds: 492,
    resource_note: '1 service, ports 42001',
    ...overrides,
  }
}

async function mount() {
  const view = render(
    <MemoryRouter>
      <ToastProvider>
        <PreviewsPage />
      </ToastProvider>
    </MemoryRouter>,
  )
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
  return view
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date(NOW))
  resetPreviewsStore()
  previews = { enabled: true, anvils: ['forge'], quest_anvils: [], previews: [preview()] }
  posts = []
  startRejection = null

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : String(input)
      if (url === '/api/previews') return jsonResponse(previews)
      if (url.endsWith('/preview/stop') || url.endsWith('/preview/start')) {
        posts.push({ url, body: init?.body ? JSON.parse(String(init.body)) : null })
        if (startRejection) return jsonResponse({ error: startRejection }, 500)
        return jsonResponse({ queued: true, request_id: 'r1', poll_url: '/api/requests/r1' }, 202)
      }
      if (url.startsWith('/api/requests/')) {
        return jsonResponse({ request_id: 'r1', state: 'pending' })
      }
      throw new Error(`unexpected fetch: ${url}`)
    }),
  )
})

afterEach(() => {
  resetPreviewsStore()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('PreviewsPage', () => {
  it('lists every live preview with its services and entry link', async () => {
    previews = {
      ...previews,
      previews: [preview(), preview({ bead_id: 'Forge-def2', anvil: 'hytte', entry_url: '' })],
    }
    await mount()

    const row = screen.getByTestId('preview-row-Forge-abc1')
    expect(within(row).getByText('Forge-abc1')).toBeInTheDocument()
    expect(within(row).getByText('forge/Forge-abc1')).toBeInTheDocument()
    const service = within(row).getByTestId('preview-row-service-Forge-abc1-web')
    expect(within(service).getByText('web:42001')).toBeInTheDocument()
    expect(within(service).getByText('1m 35s')).toBeInTheDocument()

    const open = within(row).getByTestId('preview-row-open-Forge-abc1')
    expect(open).toHaveAttribute('href', 'http://forge-box:42001/')
    expect(open).toHaveAttribute('target', '_blank')

    // The second preview has no entry service with a port yet, so there is
    // nothing to open.
    const other = screen.getByTestId('preview-row-Forge-def2')
    expect(within(other).queryByTestId('preview-row-open-Forge-def2')).not.toBeInTheDocument()
  })

  // Forge-bci1: the fleet view has to agree with the bead panel about a service
  // that came up and later died — a preview that reads healthy on one page and
  // dead on the other is the same lie in a different place.
  it('shows an exited service with its frozen lifetime and no dead link', async () => {
    previews = {
      ...previews,
      previews: [
        preview({
          status: 'degraded',
          entry_url: '',
          entry_note: 'entry service "web" is not serving: exited (exit 1, lived 7m31s)',
          services: [
            {
              name: 'web',
              port: 42001,
              health: 'exited',
              entry: true,
              uptime_seconds: 451,
              log_url: '/api/preview/Forge-abc1/log/web',
              error: 'exited (exit 1, lived 7m31s)',
              exit_code: 1,
            },
          ],
        }),
      ],
    }
    await mount()

    const row = screen.getByTestId('preview-row-Forge-abc1')
    const service = within(row).getByTestId('preview-row-service-Forge-abc1-web')
    expect(within(service).getByText('lived 7m 31s')).toBeInTheDocument()
    expect(within(row).queryByTestId('preview-row-open-Forge-abc1')).not.toBeInTheDocument()
    expect(within(row).getByTestId('preview-row-entry-note-Forge-abc1')).toHaveAttribute(
      'title',
      'entry service "web" is not serving: exited (exit 1, lived 7m31s)',
    )
  })

  it('counts down each preview to its idle deadline and keeps ticking', async () => {
    await mount()
    expect(screen.getByTestId('preview-row-countdown-Forge-abc1')).toHaveTextContent(
      'idles in 8m 12s',
    )

    // Under the 5s list poll: the value can only move by being aged locally
    // from the seconds-remaining the daemon last sent.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000)
    })
    expect(screen.getByTestId('preview-row-countdown-Forge-abc1')).toHaveTextContent(
      'idles in 8m 09s',
    )
  })

  it('omits the countdown when the idle reaper is disabled', async () => {
    previews = {
      ...previews,
      previews: [preview({ idle_deadline: null, idle_remaining_seconds: null })],
    }
    await mount()
    expect(screen.queryByTestId('preview-row-countdown-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders the resource note for each preview', async () => {
    await mount()
    expect(screen.getByTestId('preview-row-resource-note-Forge-abc1')).toHaveTextContent(
      '1 service, ports 42001',
    )
  })

  it('links each row to the bead detail page, carrying the anvil', async () => {
    await mount()
    expect(screen.getByRole('link', { name: 'Forge-abc1' })).toHaveAttribute(
      'href',
      '/bead/Forge-abc1?anvil=forge',
    )
  })

  it('stops a preview from its row', async () => {
    await mount()
    await act(async () => {
      screen.getByTestId('preview-row-stop-Forge-abc1').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(posts).toEqual([{ url: '/api/bead/Forge-abc1/preview/stop', body: { anvil: 'forge' } }])
    expect(screen.getByTestId('preview-row-status-Forge-abc1')).toHaveAttribute(
      'data-status',
      'stopping',
    )
  })

  it('says previews are disabled rather than showing an empty list', async () => {
    previews = { enabled: false, anvils: [], quest_anvils: [], previews: [] }
    await mount()
    expect(screen.getByText(/Previews are disabled/)).toBeInTheDocument()
    expect(screen.queryByTestId('preview-row-Forge-abc1')).not.toBeInTheDocument()
  })

  it('distinguishes an enabled-but-empty fleet', async () => {
    previews = { enabled: true, anvils: ['forge'], quest_anvils: [], previews: [] }
    await mount()
    expect(screen.getByText(/No preview environments are running/)).toBeInTheDocument()
  })
})

// The ad-hoc form is the browser half of `forge preview start`: a preview id
// that need not be a bead, an anvil, and an optional branch.
describe('PreviewsPage ad-hoc preview form', () => {
  // fill types an id and (optionally) a branch, then submits.
  async function submitAdHoc(id: string, branch?: string) {
    fireEvent.change(screen.getByTestId('adhoc-preview-id'), { target: { value: id } })
    if (branch !== undefined) {
      fireEvent.change(screen.getByTestId('adhoc-preview-branch'), { target: { value: branch } })
    }
    await act(async () => {
      screen.getByTestId('adhoc-preview-submit').click()
      await vi.advanceTimersByTimeAsync(0)
    })
  }

  it('offers every previewable anvil from the payload', async () => {
    previews = { ...previews, anvils: ['forge', 'hytte'] }
    await mount()
    const options = within(screen.getByTestId('adhoc-preview-anvil')).getAllByRole('option')
    expect(options.map((o) => o.textContent)).toEqual(['Choose an anvil…', 'forge', 'hytte'])
  })

  it('starts a preview without a branch, letting the daemon default it', async () => {
    await mount()
    await submitAdHoc('kiln-smoke-1')

    expect(posts).toEqual([
      { url: '/api/bead/kiln-smoke-1/preview/start', body: { anvil: 'forge' } },
    ])
    expect(screen.getByTestId('adhoc-preview-pending')).toHaveTextContent('kiln-smoke-1')
  })

  it('sends the branch when one is given', async () => {
    await mount()
    await submitAdHoc('kiln-smoke-2', '  main  ')

    expect(posts).toEqual([
      { url: '/api/bead/kiln-smoke-2/preview/start', body: { anvil: 'forge', branch: 'main' } },
    ])
  })

  it('reports the daemon refusal and keeps what was typed', async () => {
    startRejection = 'previews are disabled for anvil "forge"'
    await mount()
    await submitAdHoc('kiln-smoke-3', 'main')

    expect(screen.getByTestId('adhoc-preview-error')).toHaveTextContent(
      'previews are disabled for anvil "forge"',
    )
    expect(screen.getByTestId('adhoc-preview-id')).toHaveValue('kiln-smoke-3')
    expect(screen.getByTestId('adhoc-preview-branch')).toHaveValue('main')
  })

  it('will not submit without an id, nor without an anvil when there is a choice', async () => {
    previews = { ...previews, anvils: ['forge', 'hytte'] }
    await mount()
    expect(screen.getByTestId('adhoc-preview-submit')).toBeDisabled()

    // An id alone is not enough while the anvil is still unchosen.
    fireEvent.change(screen.getByTestId('adhoc-preview-id'), { target: { value: 'kiln-smoke-1' } })
    expect(screen.getByTestId('adhoc-preview-submit')).toBeDisabled()

    fireEvent.change(screen.getByTestId('adhoc-preview-anvil'), { target: { value: 'hytte' } })
    expect(screen.getByTestId('adhoc-preview-submit')).toBeEnabled()
  })

  it('rejects an id the bead-id route would refuse, without asking the daemon', async () => {
    await mount()
    await submitAdHoc('kiln smoke/1')

    expect(posts).toEqual([])
    expect(screen.getByTestId('adhoc-preview-error')).toHaveTextContent('must start with a letter')
  })

  it('refuses an id that already has a preview rather than adopting it', async () => {
    await mount()
    fireEvent.change(screen.getByTestId('adhoc-preview-id'), { target: { value: 'Forge-abc1' } })

    expect(screen.getByTestId('adhoc-preview-taken')).toBeInTheDocument()
    expect(screen.getByTestId('adhoc-preview-submit')).toBeDisabled()
  })

  it('stays hidden while previews are disabled', async () => {
    previews = { enabled: false, anvils: [], quest_anvils: [], previews: [] }
    await mount()
    expect(screen.queryByTestId('adhoc-preview-form')).not.toBeInTheDocument()
  })
})
