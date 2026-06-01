import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  assay,
  findingsStreamURL,
  subscribeFindings,
  type PRFindingsResponse,
} from './api'

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

const SNAPSHOT: PRFindingsResponse = {
  pr: 42,
  anvil: 'forge',
  run: { status: 'running', findings_count: 0, posted_count: 0 },
  findings: [],
}

// FakeEventSource is a minimal EventSource stand-in: it records the URL and
// lets the test drive named events synchronously.
class FakeEventSource {
  static last: FakeEventSource | null = null
  url: string
  withCredentials: boolean
  closed = false
  onerror: ((this: EventSource, ev: Event) => unknown) | null = null
  private listeners = new Map<string, Array<(ev: MessageEvent) => void>>()

  constructor(url: string, init?: EventSourceInit) {
    this.url = url
    this.withCredentials = init?.withCredentials ?? false
    FakeEventSource.last = this
  }

  addEventListener(type: string, fn: (ev: MessageEvent) => void) {
    const arr = this.listeners.get(type) ?? []
    arr.push(fn)
    this.listeners.set(type, arr)
  }

  emit(type: string, data: string) {
    for (const fn of this.listeners.get(type) ?? []) {
      fn(new MessageEvent(type, { data }))
    }
  }

  close() {
    this.closed = true
  }
}

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  FakeEventSource.last = null
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('assay findings client', () => {
  it('getFindings calls the id-keyed findings endpoint', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(SNAPSHOT))
    const out = await assay.getFindings(7)
    expect(out).toEqual(SNAPSHOT)
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/prs/7/findings',
      expect.objectContaining({ credentials: 'include' }),
    )
  })

  it('rerunAssay POSTs to the rerun endpoint with the anvil body', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ message: 'queued' }, { status: 202 }))
    await assay.rerunAssay({ anvil: 'forge', pr: 7 })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/prs/7/rerun-assay')
    expect(init).toMatchObject({ method: 'POST' })
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({ anvil: 'forge' })
    expect((init as RequestInit).headers).toMatchObject({ 'X-Forge-Action': '1' })
  })

  it('findingsStreamURL builds the SSE path', () => {
    expect(findingsStreamURL(7)).toBe('/api/prs/7/findings/stream')
  })

  it('subscribeFindings forwards named findings events to onSnapshot', () => {
    const onSnapshot = vi.fn()
    const onOpen = vi.fn()
    const sub = subscribeFindings(
      7,
      { onSnapshot, onOpen },
      { eventSourceImpl: FakeEventSource as unknown as typeof EventSource },
    )
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/prs/7/findings/stream')
    expect(es.withCredentials).toBe(true)

    es.emit('open', '')
    expect(onOpen).toHaveBeenCalledOnce()

    es.emit('findings', JSON.stringify(SNAPSHOT))
    expect(onSnapshot).toHaveBeenCalledWith(SNAPSHOT)

    // Malformed frames are dropped, not thrown.
    es.emit('findings', 'not json{')
    expect(onSnapshot).toHaveBeenCalledOnce()

    sub.close()
    expect(es.closed).toBe(true)

    // No further callbacks after close.
    es.emit('findings', JSON.stringify(SNAPSHOT))
    expect(onSnapshot).toHaveBeenCalledOnce()
  })

  it('subscribeFindings is a no-op when EventSource is unavailable', () => {
    const onSnapshot = vi.fn()
    // No eventSourceImpl and jsdom has no global EventSource → no-op handle.
    const sub = subscribeFindings(7, { onSnapshot })
    expect(typeof sub.close).toBe('function')
    expect(() => sub.close()).not.toThrow()
    expect(onSnapshot).not.toHaveBeenCalled()
  })
})
