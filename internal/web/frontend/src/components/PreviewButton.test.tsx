import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import type { PreviewsListResponse, PreviewSummary } from '../api/previews'
import { resetPreviewsStore } from '../hooks/usePreview'
import { ToastProvider } from '../hooks/useToast'
import PreviewButton from './PreviewButton'

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
        uptime_seconds: 30,
        log_url: '/api/preview/Forge-abc1/log/web',
      },
    ],
    entry_url: 'http://forge-box:42001/',
    created_at: '2026-08-06T10:00:00Z',
    last_active_at: '2026-08-06T10:00:00Z',
    idle_deadline: null,
    ...overrides,
  }
}

function wrapper({ children }: { children: ReactNode }) {
  return <ToastProvider>{children}</ToastProvider>
}

// mount renders the button and flushes the shared store's first fetch, which is
// what decides whether the button is allowed to appear at all.
async function mount(props: Partial<React.ComponentProps<typeof PreviewButton>> = {}) {
  const view = render(
    <PreviewButton beadId="Forge-abc1" anvil="forge" hasBranch {...props} />,
    { wrapper },
  )
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
  return view
}

beforeEach(() => {
  vi.useFakeTimers()
  resetPreviewsStore()
  previews = { enabled: true, anvils: ['forge'], quest_anvils: [], previews: [] }
  posts = []

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : String(input)
      if (url === '/api/previews') return jsonResponse(previews)
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

describe('PreviewButton gating', () => {
  it('renders nothing while the previews snapshot has not loaded', () => {
    render(<PreviewButton beadId="Forge-abc1" anvil="forge" hasBranch />, { wrapper })
    expect(screen.queryByTestId('preview-controls-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders nothing when the anvil declares no preview manifest', async () => {
    previews = { enabled: true, anvils: ['other'], quest_anvils: [], previews: [] }
    await mount()
    expect(screen.queryByTestId('preview-controls-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders nothing when Kiln is disabled daemon-wide', async () => {
    previews = { enabled: false, anvils: [], quest_anvils: [], previews: [] }
    await mount()
    expect(screen.queryByTestId('preview-controls-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders nothing when the bead has no surviving branch or PR', async () => {
    await mount({ hasBranch: false })
    expect(screen.queryByTestId('preview-controls-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders nothing when the bead has no anvil', async () => {
    await mount({ anvil: undefined })
    expect(screen.queryByTestId('preview-controls-Forge-abc1')).not.toBeInTheDocument()
  })

  it('renders the trigger when Kiln, the manifest and the branch all line up', async () => {
    await mount()
    expect(screen.getByTestId('preview-start-Forge-abc1')).toBeInTheDocument()
    expect(screen.queryByTestId('preview-status-Forge-abc1')).not.toBeInTheDocument()
  })
})

describe('PreviewButton states', () => {
  it('starts a preview and shows the starting chip', async () => {
    await mount()
    await act(async () => {
      screen.getByTestId('preview-start-Forge-abc1').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(posts).toEqual([
      { url: '/api/bead/Forge-abc1/preview/start', body: { anvil: 'forge' } },
    ])
    const chip = screen.getByTestId('preview-status-Forge-abc1')
    expect(chip).toHaveAttribute('data-status', 'starting')
    expect(chip).toHaveTextContent('Starting')
    expect(screen.queryByTestId('preview-start-Forge-abc1')).not.toBeInTheDocument()
  })

  it('offers Open and Stop — and no trigger — for a healthy preview', async () => {
    previews = { ...previews, previews: [preview()] }
    await mount()

    expect(screen.getByTestId('preview-status-Forge-abc1')).toHaveAttribute(
      'data-status',
      'healthy',
    )
    const open = screen.getByTestId('preview-open-Forge-abc1')
    expect(open).toHaveAttribute('href', 'http://forge-box:42001/')
    expect(open).toHaveAttribute('target', '_blank')
    expect(screen.getByTestId('preview-stop-Forge-abc1')).toBeInTheDocument()
    expect(screen.queryByTestId('preview-start-Forge-abc1')).not.toBeInTheDocument()
  })

  it('stops a preview through the stop endpoint', async () => {
    previews = { ...previews, previews: [preview()] }
    await mount()
    await act(async () => {
      screen.getByTestId('preview-stop-Forge-abc1').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(posts).toEqual([
      { url: '/api/bead/Forge-abc1/preview/stop', body: { anvil: 'forge' } },
    ])
    expect(screen.getByTestId('preview-status-Forge-abc1')).toHaveAttribute(
      'data-status',
      'stopping',
    )
  })

  it('explains a failed preview and offers Stop rather than a retry', async () => {
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

    const chip = screen.getByTestId('preview-status-Forge-abc1')
    expect(chip).toHaveAttribute('data-status', 'failed')
    expect(chip).toHaveAttribute('title', 'Failed — health check timed out after 60s')
    // Starting a bead that already has an environment returns the existing one,
    // so the record has to be cleared with Stop before a retry means anything.
    expect(screen.queryByTestId('preview-start-Forge-abc1')).not.toBeInTheDocument()
    expect(screen.getByTestId('preview-stop-Forge-abc1')).toBeInTheDocument()
    expect(screen.queryByTestId('preview-open-Forge-abc1')).not.toBeInTheDocument()
  })

  it('offers a retry when the start failed without leaving an environment', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : String(input)
        if (url === '/api/previews') return jsonResponse(previews)
        if (url.endsWith('/preview/start')) {
          return jsonResponse({ error: 'preview cap reached (2 running)' }, 409)
        }
        throw new Error(`unexpected fetch: ${url}`)
      }),
    )
    await mount()
    await act(async () => {
      screen.getByTestId('preview-start-Forge-abc1').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(screen.getByTestId('preview-status-Forge-abc1')).toHaveAttribute(
      'data-status',
      'failed',
    )
    const retry = screen.getByTestId('preview-start-Forge-abc1')
    expect(retry).toHaveAccessibleName('Retry preview for Forge-abc1')
    expect(screen.queryByTestId('preview-stop-Forge-abc1')).not.toBeInTheDocument()
  })
})
