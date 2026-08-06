import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import type { PreviewsListResponse, PreviewSummary } from '../api/previews'
import { resetPreviewsStore } from '../hooks/usePreview'
import { ToastProvider } from '../hooks/useToast'
import PreviewPanel from './PreviewPanel'

const NOW = '2026-08-06T10:00:30Z'

let previews: PreviewsListResponse
let posts: Array<{ url: string; body: unknown }>
let logRequests: string[]
let logLines: string[]

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
      {
        name: 'api',
        port: 42002,
        health: 'starting',
        entry: false,
        uptime_seconds: 95,
        log_url: '/api/preview/Forge-abc1/log/api',
      },
    ],
    entry_url: 'http://forge-box:42001/',
    created_at: '2026-08-06T09:58:55Z',
    last_active_at: '2026-08-06T09:58:55Z',
    idle_deadline: '2026-08-06T10:08:42Z',
    ...overrides,
  }
}

function wrapper({ children }: { children: ReactNode }) {
  return <ToastProvider>{children}</ToastProvider>
}

// mount renders the panel and flushes the shared store's first fetch, which is
// what decides whether the panel is allowed to appear at all.
async function mount(props: Partial<React.ComponentProps<typeof PreviewPanel>> = {}) {
  const view = render(<PreviewPanel beadId="Forge-abc1" anvil="forge" hasBranch {...props} />, {
    wrapper,
  })
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
  return view
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date(NOW))
  resetPreviewsStore()
  previews = { enabled: true, anvils: ['forge'], previews: [preview()] }
  posts = []
  logRequests = []
  logLines = ['listening on :42001', 'ready']

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : String(input)
      if (url === '/api/previews') return jsonResponse(previews)
      if (url.startsWith('/api/preview/') && url.includes('/log/')) {
        logRequests.push(url)
        return jsonResponse({ lines: logLines })
      }
      if (url.endsWith('/preview/start') || url.endsWith('/preview/stop')) {
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

describe('PreviewPanel gating', () => {
  it('renders nothing while the previews snapshot has not loaded', () => {
    render(<PreviewPanel beadId="Forge-abc1" anvil="forge" hasBranch />, { wrapper })
    expect(screen.queryByTestId('preview-panel-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders nothing when Kiln is disabled daemon-wide', async () => {
    previews = { enabled: false, anvils: [], previews: [] }
    await mount()
    expect(screen.queryByTestId('preview-panel-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders nothing when the anvil declares no preview manifest', async () => {
    previews = { ...previews, anvils: ['other'] }
    await mount()
    expect(screen.queryByTestId('preview-panel-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders nothing when the bead has no surviving branch', async () => {
    await mount({ hasBranch: false })
    expect(screen.queryByTestId('preview-panel-Forge-abc1')).not.toBeInTheDocument()
  })
})

describe('PreviewPanel service rows', () => {
  it('renders one row per service with port, health and uptime', async () => {
    await mount()

    const web = screen.getByTestId('preview-service-web')
    expect(within(web).getByText('web')).toBeInTheDocument()
    expect(within(web).getByText('entry')).toBeInTheDocument()
    expect(within(web).getByText('42001')).toBeInTheDocument()
    expect(within(web).getByTestId('preview-service-health-web')).toHaveAttribute(
      'data-health',
      'healthy',
    )
    expect(within(web).getByText('1m 35s')).toBeInTheDocument()

    const api = screen.getByTestId('preview-service-api')
    expect(within(api).getByText('42002')).toBeInTheDocument()
    expect(within(api).getByTestId('preview-service-health-api')).toHaveAttribute(
      'data-health',
      'starting',
    )
    // Only the manifest's entry service is marked as such.
    expect(within(api).queryByText('entry')).not.toBeInTheDocument()
  })

  it('renders the idle countdown from the daemon-supplied deadline', async () => {
    await mount()
    // 10:00:30 → 10:08:42 is 8m 12s.
    expect(screen.getByTestId('preview-panel-countdown')).toHaveTextContent('idles in 8m 12s')
  })

  it('omits the countdown when the idle reaper is disabled', async () => {
    previews = { ...previews, previews: [preview({ idle_deadline: null })] }
    await mount()
    expect(screen.queryByTestId('preview-panel-countdown')).not.toBeInTheDocument()
  })

  it('attributes a failed service and reports no uptime for it', async () => {
    previews = {
      ...previews,
      previews: [
        preview({
          status: 'failed',
          entry_url: '',
          services: [
            {
              name: 'web',
              port: 42001,
              health: 'failed',
              entry: true,
              uptime_seconds: 0,
              log_url: '/api/preview/Forge-abc1/log/web',
              error: 'health check timed out after 60s',
            },
          ],
        }),
      ],
    }
    await mount()

    const web = screen.getByTestId('preview-service-web')
    expect(within(web).getByTestId('preview-service-health-web')).toHaveAttribute(
      'data-health',
      'failed',
    )
    expect(within(web).getByText('—')).toBeInTheDocument()
    expect(screen.getByText(/health check timed out after 60s/)).toBeInTheDocument()
    expect(screen.queryByTestId('preview-panel-open')).not.toBeInTheDocument()
  })

  it('invites a start, with no service table, when no preview is running', async () => {
    previews = { ...previews, previews: [] }
    await mount()

    expect(screen.getByTestId('preview-panel-start')).toBeInTheDocument()
    expect(screen.queryByTestId('preview-service-web')).not.toBeInTheDocument()
    expect(screen.queryByTestId('preview-panel-stop')).not.toBeInTheDocument()
    expect(
      screen.getByText('No preview environment is running for this bead.'),
    ).toBeInTheDocument()
  })
})

describe('PreviewPanel actions', () => {
  it('links Open preview at the entry URL in a new tab', async () => {
    await mount()
    const open = screen.getByTestId('preview-panel-open')
    expect(open).toHaveAttribute('href', 'http://forge-box:42001/')
    expect(open).toHaveAttribute('target', '_blank')
    expect(open).toHaveAttribute('rel', 'noreferrer')
  })

  it('stops the preview through the stop endpoint', async () => {
    await mount()
    await act(async () => {
      screen.getByTestId('preview-panel-stop').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(posts).toEqual([{ url: '/api/bead/Forge-abc1/preview/stop', body: { anvil: 'forge' } }])
    expect(screen.getByTestId('preview-status-Forge-abc1')).toHaveAttribute(
      'data-status',
      'stopping',
    )
  })

  it('starts a preview and shows the starting chip', async () => {
    previews = { ...previews, previews: [] }
    await mount()
    await act(async () => {
      screen.getByTestId('preview-panel-start').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(posts).toEqual([{ url: '/api/bead/Forge-abc1/preview/start', body: { anvil: 'forge' } }])
    expect(screen.getByTestId('preview-status-Forge-abc1')).toHaveAttribute(
      'data-status',
      'starting',
    )
  })

  it('opens a service log tail from the row link', async () => {
    await mount()
    expect(screen.queryByTestId('preview-log-modal')).not.toBeInTheDocument()

    await act(async () => {
      screen.getByTestId('preview-service-log-api').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(logRequests).toEqual(['/api/preview/Forge-abc1/log/api?tail=500'])
    const modal = screen.getByTestId('preview-log-modal')
    expect(within(modal).getByTestId('preview-log-body')).toHaveTextContent('listening on :42001')
  })
})
