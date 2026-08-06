import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import type { PreviewsListResponse, PreviewSummary, QuestRunSummary } from '../api/previews'
import { resetPreviewsStore } from '../hooks/usePreview'
import { ToastProvider } from '../hooks/useToast'
import PreviewPanel from './PreviewPanel'

// These cover the "Run quests" action on the preview panel: when it is offered,
// what it dispatches, and how a run is rendered — including the part that
// matters most, that a red run is visibly informational rather than a block.

const NOW = '2026-08-06T10:00:30Z'

let previews: PreviewsListResponse
let questRun: { found: boolean; run?: QuestRunSummary }
let posts: string[]
let questGets: number

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
    idle_deadline: null,
    ...overrides,
  }
}

function run(overrides: Partial<QuestRunSummary> = {}): QuestRunSummary {
  return {
    run_id: 'qr-17-3',
    bead_id: 'Forge-abc1',
    anvil: 'forge',
    base_url: 'http://forge-box:42001/',
    status: 'passed',
    started_at: '2026-08-06T09:59:00Z',
    finished_at: '2026-08-06T10:00:00Z',
    duration_seconds: 60,
    quests: [],
    ...overrides,
  }
}

function wrapper({ children }: { children: ReactNode }) {
  return <ToastProvider>{children}</ToastProvider>
}

async function mount() {
  const view = render(<PreviewPanel beadId="Forge-abc1" anvil="forge" hasBranch />, { wrapper })
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
  return view
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date(NOW))
  resetPreviewsStore()
  previews = {
    enabled: true,
    anvils: ['forge'],
    quest_anvils: ['forge'],
    previews: [preview()],
  }
  questRun = { found: false }
  posts = []
  questGets = 0

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : String(input)
      if (url === '/api/previews') return jsonResponse(previews)
      if (url === '/api/bead/Forge-abc1/quests') {
        if (init?.method === 'POST') {
          posts.push(url)
          const started = run({ status: 'running', finished_at: null, duration_seconds: 0 })
          questRun = { found: true, run: started }
          return jsonResponse({ started: true, run_id: started.run_id, run: started }, 202)
        }
        questGets++
        return jsonResponse(questRun)
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

describe('Run quests gating', () => {
  it('offers the action when the anvil opted in and the preview is healthy', async () => {
    await mount()
    expect(screen.getByTestId('preview-panel-run-quests')).toBeInTheDocument()
  })

  it('hides the action when the anvil did not opt into preview quests', async () => {
    previews = { ...previews, quest_anvils: [] }
    await mount()
    expect(screen.queryByTestId('preview-panel-run-quests')).not.toBeInTheDocument()
    // …and does not even ask about runs for a bead whose anvil is opted out.
    expect(questGets).toBe(0)
  })

  it('hides the action while the preview is not healthy', async () => {
    for (const status of ['starting', 'degraded', 'failed'] as const) {
      resetPreviewsStore()
      previews = { ...previews, previews: [preview({ status })] }
      const view = await mount()
      expect(screen.queryByTestId('preview-panel-run-quests')).not.toBeInTheDocument()
      view.unmount()
    }
  })
})

describe('Run quests dispatch', () => {
  it('posts a run and renders it as running', async () => {
    await mount()
    await act(async () => {
      screen.getByTestId('preview-panel-run-quests').click()
      await vi.advanceTimersByTimeAsync(0)
    })

    expect(posts).toEqual(['/api/bead/Forge-abc1/quests'])
    expect(screen.getByTestId('quest-run-status')).toHaveAttribute('data-status', 'running')
    // The button stays put but is disabled while the browser work is going.
    expect(screen.getByTestId('preview-panel-run-quests')).toBeDisabled()
  })
})

describe('Quest run rendering', () => {
  const failed = () =>
    run({
      status: 'failed',
      quests: [
        {
          name: 'login',
          passed: true,
          failed_step: -1,
          duration_seconds: 12,
          screenshots: [
            { name: 'login.png', url: '/api/bead/Forge-abc1/quests/qr-17-3/screenshot/0' },
          ],
        },
        {
          name: 'checkout',
          passed: false,
          failed_step: 3,
          error_message: "assert failed: expected 'Order placed'",
          duration_seconds: 30,
          screenshots: [
            { name: 'checkout.png', url: '/api/bead/Forge-abc1/quests/qr-17-3/screenshot/1' },
          ],
        },
      ],
    })

  it('renders a failed run with per-quest rows and screenshot thumbnails', async () => {
    questRun = { found: true, run: failed() }
    await mount()

    const block = screen.getByTestId('preview-quests-Forge-abc1')
    expect(block).toHaveAttribute('data-status', 'failed')
    expect(within(block).getByTestId('quest-run-status')).toHaveTextContent('Quests failed')
    expect(within(block).getByText('1/2 passed')).toBeInTheDocument()

    const passing = screen.getByTestId('quest-row-login')
    expect(passing).toHaveAttribute('data-passed', 'true')
    const failing = screen.getByTestId('quest-row-checkout')
    expect(failing).toHaveAttribute('data-passed', 'false')
    expect(within(failing).getByText('step 3')).toBeInTheDocument()
    expect(screen.getByTestId('quest-error-checkout')).toHaveTextContent(
      "assert failed: expected 'Order placed'",
    )

    const shots = screen.getAllByRole('img')
    expect(shots.map((img) => img.getAttribute('src'))).toEqual([
      '/api/bead/Forge-abc1/quests/qr-17-3/screenshot/0',
      '/api/bead/Forge-abc1/quests/qr-17-3/screenshot/1',
    ])
    // The thumbnail links to the full image rather than opening a lightbox.
    expect(shots[0].closest('a')).toHaveAttribute(
      'href',
      '/api/bead/Forge-abc1/quests/qr-17-3/screenshot/0',
    )
  })

  it('states that a failed run does not block the PR, and leaves the preview alone', async () => {
    questRun = { found: true, run: failed() }
    await mount()

    expect(screen.getByTestId('quest-run-advisory')).toHaveTextContent(
      /Informational — does not block/,
    )
    // The preview itself is untouched by a red run: still healthy, still
    // openable, still stoppable.
    expect(screen.getByTestId('preview-status-Forge-abc1')).toHaveAttribute(
      'data-status',
      'healthy',
    )
    expect(screen.getByTestId('preview-panel-open')).toBeInTheDocument()
    expect(screen.getByTestId('preview-panel-run-quests')).toBeEnabled()
  })

  it('renders a skipped run with its reason and no failure styling', async () => {
    questRun = {
      found: true,
      run: run({ status: 'skipped', skip_reason: 'anvil declares no quests' }),
    }
    await mount()

    expect(screen.getByTestId('preview-quests-Forge-abc1')).toHaveAttribute('data-status', 'skipped')
    expect(screen.getByTestId('quest-run-skip-reason')).toHaveTextContent('anvil declares no quests')
    expect(screen.queryByTestId('quest-run-advisory')).not.toBeInTheDocument()
  })

  it('renders nothing when the bead has never had a run', async () => {
    await mount()
    expect(screen.queryByTestId('preview-quests-Forge-abc1')).not.toBeInTheDocument()
  })

  it('keeps showing the last run after the preview goes unhealthy', async () => {
    questRun = { found: true, run: failed() }
    previews = { ...previews, previews: [preview({ status: 'degraded' })] }
    await mount()

    expect(screen.queryByTestId('preview-panel-run-quests')).not.toBeInTheDocument()
    expect(screen.getByTestId('preview-quests-Forge-abc1')).toBeInTheDocument()
  })
})
