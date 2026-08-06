import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import type { PreviewsListResponse, PreviewSummary } from '../api/previews'
import type { RequestState } from '../api'
import { resetPreviewsStore, usePreview } from './usePreview'

// The hook is driven entirely by three endpoints, so the tests stand up a tiny
// mutable fake of the daemon rather than mocking the client module: that keeps
// the request shapes (paths, 202 body, request-status polling) under test too.
let previews: PreviewsListResponse
let requestState: RequestState
let requestMessage: string | undefined
let posts: Array<{ url: string; body: unknown }>

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function runningPreview(overrides: Partial<PreviewSummary> = {}): PreviewSummary {
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
        uptime_seconds: 12,
        log_url: '/api/preview/Forge-abc1/log/web',
      },
    ],
    entry_url: 'http://forge-box:42001/',
    created_at: '2026-08-06T10:00:00Z',
    last_active_at: '2026-08-06T10:00:00Z',
    idle_deadline: '2026-08-06T10:30:00Z',
    ...overrides,
  }
}

// advance runs the fake clock forward inside act(), flushing the promise chain
// each fetch produces. RTL's waitFor does not detect vitest's fake timers, so
// every assertion is driven from an explicit tick instead.
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

// settle flushes pending microtasks (the store's first fetch) without moving
// the clock.
async function settle() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
}

beforeEach(() => {
  vi.useFakeTimers()
  resetPreviewsStore()
  previews = { enabled: true, anvils: ['forge'], quest_anvils: [], previews: [] }
  requestState = 'pending'
  requestMessage = undefined
  posts = []

  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : String(input)
      if (url === '/api/previews') {
        return jsonResponse(previews)
      }
      if (url.endsWith('/preview/start') || url.endsWith('/preview/stop')) {
        posts.push({ url, body: init?.body ? JSON.parse(String(init.body)) : null })
        return jsonResponse(
          { queued: true, request_id: 'req-1', poll_url: '/api/requests/req-1' },
          202,
        )
      }
      if (url.startsWith('/api/requests/')) {
        return jsonResponse({ request_id: 'req-1', state: requestState, message: requestMessage })
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

// render mounts the hook with the usual anvil. Pass `anvil: null` for the
// "bead whose anvil is unknown" case — an explicit undefined would just pick up
// the default.
interface RenderOptions {
  anvil?: string | null
  onError?: (message: string) => void
}

function render(beadID = 'Forge-abc1', opts: RenderOptions = {}) {
  const anvil = opts.anvil === null ? undefined : (opts.anvil ?? 'forge')
  return renderHook(() => usePreview(beadID, { anvil, onError: opts.onError }))
}

describe('usePreview gating', () => {
  it('is available when Kiln is on and the bead anvil declares a manifest', async () => {
    const { result } = render()
    await settle()
    expect(result.current.enabled).toBe(true)
    expect(result.current.available).toBe(true)
    expect(result.current.status).toBe('idle')
  })

  it('is unavailable when the anvil has no preview manifest', async () => {
    previews = { enabled: true, anvils: ['other'], quest_anvils: [], previews: [] }
    const { result } = render()
    await settle()
    expect(result.current.available).toBe(false)
  })

  it('is unavailable when Kiln is disabled daemon-wide', async () => {
    previews = { enabled: false, anvils: [], quest_anvils: [], previews: [] }
    const { result } = render()
    await settle()
    expect(result.current.enabled).toBe(false)
    expect(result.current.available).toBe(false)
  })

  it('is unavailable when the bead has no anvil to start against', async () => {
    const { result } = render('Forge-abc1', { anvil: null })
    await settle()
    expect(result.current.available).toBe(false)
  })
})

describe('usePreview state machine', () => {
  it('goes idle → starting → healthy and exposes the entry URL', async () => {
    const { result } = render()
    await settle()

    await act(async () => {
      await result.current.start()
    })
    expect(posts).toEqual([
      { url: '/api/bead/Forge-abc1/preview/start', body: { anvil: 'forge' } },
    ])
    expect(result.current.status).toBe('starting')
    expect(result.current.isBusy).toBe(true)
    expect(result.current.previewUrl).toBeNull()

    // Still coming up: the daemon publishes no row until every service is
    // healthy, so the request stays pending and the chip stays "starting".
    await advance(2000)
    expect(result.current.status).toBe('starting')

    requestState = 'ok'
    previews = { ...previews, previews: [runningPreview()] }
    await advance(2000)

    expect(result.current.status).toBe('healthy')
    expect(result.current.previewUrl).toBe('http://forge-box:42001/')
    expect(result.current.isBusy).toBe(false)
    expect(result.current.error).toBeNull()
  })

  it('fails with the daemon message when the queued start errors', async () => {
    const onError = vi.fn()
    const { result } = render('Forge-abc1', { onError })
    await settle()

    await act(async () => {
      await result.current.start()
    })
    requestState = 'error'
    requestMessage = 'starting preview for Forge-abc1 failed: no preview manifest'
    await advance(2000)

    expect(result.current.status).toBe('failed')
    expect(result.current.error).toBe(
      'starting preview for Forge-abc1 failed: no preview manifest',
    )
    expect(result.current.isBusy).toBe(false)
    expect(onError).toHaveBeenCalledWith(
      'starting preview for Forge-abc1 failed: no preview manifest',
    )
  })

  it('fails when the start never resolves before the timeout', async () => {
    const { result } = render()
    await settle()

    await act(async () => {
      await result.current.start()
    })
    // The request stays pending forever — a daemon that stopped answering.
    await advance(10 * 60_000)
    expect(result.current.status).toBe('starting')

    await advance(7 * 60_000)
    expect(result.current.status).toBe('failed')
    expect(result.current.error).toMatch(/timed out/i)
    expect(result.current.isBusy).toBe(false)
  })

  it('surfaces a failed record from the list even while a start is pending', async () => {
    const { result } = render()
    await settle()

    await act(async () => {
      await result.current.start()
    })
    previews = {
      ...previews,
      previews: [
        runningPreview({
          status: 'failed',
          services: [
            {
              name: 'web',
              port: 42001,
              health: 'failed',
              entry: true,
              uptime_seconds: 0,
              log_url: '/api/preview/Forge-abc1/log/web',
              error: 'health check timed out',
            },
          ],
        }),
      ],
    }
    await advance(2000)

    expect(result.current.status).toBe('failed')
    expect(result.current.error).toBe('health check timed out')
    expect(result.current.isBusy).toBe(false)
  })

  it('goes healthy → stopping → idle', async () => {
    previews = { ...previews, previews: [runningPreview()] }
    const { result } = render()
    await settle()
    expect(result.current.status).toBe('healthy')

    await act(async () => {
      await result.current.stop()
    })
    expect(posts).toEqual([
      { url: '/api/bead/Forge-abc1/preview/stop', body: { anvil: 'forge' } },
    ])
    expect(result.current.status).toBe('stopping')

    requestState = 'ok'
    previews = { ...previews, previews: [] }
    await advance(2000)

    expect(result.current.status).toBe('idle')
    expect(result.current.isBusy).toBe(false)
  })

  it('ignores a second start while one is already in flight', async () => {
    const { result } = render()
    await settle()

    await act(async () => {
      await result.current.start()
      await result.current.start()
    })
    expect(posts).toHaveLength(1)
  })

  it('stops polling once unmounted', async () => {
    const { result, unmount } = render()
    await settle()
    await act(async () => {
      await result.current.start()
    })
    await advance(2000)

    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>
    unmount()
    const callsAtUnmount = fetchMock.mock.calls.length
    await advance(60_000)
    expect(fetchMock.mock.calls.length).toBe(callsAtUnmount)
  })

  it('shares one previews request across every mounted consumer', async () => {
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>
    const a = render('Forge-abc1')
    const b = render('Forge-def2')
    await settle()

    const listCalls = fetchMock.mock.calls.filter((c) => c[0] === '/api/previews')
    expect(listCalls).toHaveLength(1)
    a.unmount()
    b.unmount()
  })
})
