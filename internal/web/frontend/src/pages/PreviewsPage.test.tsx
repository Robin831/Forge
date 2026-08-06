import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, within } from '@testing-library/react'
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

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : String(input)
      if (url === '/api/previews') return jsonResponse(previews)
      if (url.endsWith('/preview/stop')) {
        posts.push({ url, body: init?.body ? JSON.parse(String(init.body)) : null })
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
