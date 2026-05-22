import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { startTurn, type StreamTurnHandlers } from './turnStream'
import { ApiError } from '../api'

// FakeEventSource is a stub that exposes the same surface ForgePage / the
// turnStream module rely on (addEventListener, close, withCredentials).
// Tests drive it via `emit()` to dispatch named events as the backend
// would. We intentionally do NOT extend the real EventSource class because
// jsdom doesn't ship one and the test must run in that environment.
class FakeEventSource {
  static instances: FakeEventSource[] = []
  url: string
  withCredentials: boolean
  closed = false
  // readyState mirrors the real EventSource constants: 0=CONNECTING, 1=OPEN, 2=CLOSED.
  readyState: number = 1
  listeners: Record<string, Array<(ev: MessageEvent) => void>> = {}
  constructor(url: string, init?: { withCredentials?: boolean }) {
    this.url = url
    this.withCredentials = init?.withCredentials ?? false
    FakeEventSource.instances.push(this)
  }
  addEventListener(type: string, fn: (ev: MessageEvent) => void) {
    ;(this.listeners[type] ??= []).push(fn)
  }
  close() {
    this.closed = true
    this.readyState = 2
  }
  emit(type: string, data: unknown) {
    const fns = this.listeners[type] ?? []
    const ev = new MessageEvent(type, {
      data: data === undefined ? undefined : typeof data === 'string' ? data : JSON.stringify(data),
    })
    for (const fn of fns) fn(ev)
  }
  emitTransientError() {
    const fns = this.listeners['error'] ?? []
    const ev = new Event('error')
    for (const fn of fns) fn(ev as MessageEvent)
  }
  emitClosedError() {
    this.readyState = 2
    const fns = this.listeners['error'] ?? []
    const ev = new Event('error')
    for (const fn of fns) fn(ev as MessageEvent)
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function acceptedResponse(turnId: string): Response {
  return new Response(JSON.stringify({ turn_id: turnId }), {
    status: 202,
    headers: { 'Content-Type': 'application/json' },
  })
}

function makeHandlers(): {
  handlers: Required<StreamTurnHandlers>
  calls: Record<string, unknown[]>
} {
  const calls: Record<string, unknown[]> = {
    sync: [],
    turnId: [],
    open: [],
    delta: [],
    tool: [],
    transient: [],
    complete: [],
    error: [],
  }
  const handlers: Required<StreamTurnHandlers> = {
    onSync: (p) => calls.sync.push(p),
    onTurnId: (id) => calls.turnId.push(id),
    onOpen: () => calls.open.push(true),
    onTextDelta: (c) => calls.delta.push(c),
    onTool: (c) => calls.tool.push(c),
    onTransientError: (m) => calls.transient.push(m),
    onComplete: (id) => calls.complete.push(id),
    onError: (m) => calls.error.push(m),
  }
  return { handlers, calls }
}

beforeEach(() => {
  FakeEventSource.instances = []
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('startTurn — 202 streaming branch', () => {
  it('opens an EventSource, streams text deltas, and emits complete', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(acceptedResponse('turn-1'))
    const { handlers, calls } = makeHandlers()

    const handle = await startTurn(
      42,
      { content: 'hi' },
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
      },
    )

    expect(handle.streaming).toBe(true)
    expect(calls.turnId).toEqual(['turn-1'])
    expect(FakeEventSource.instances).toHaveLength(1)
    const es = FakeEventSource.instances[0]
    expect(es.url).toBe('/api/forge/sessions/42/turn/turn-1/stream')
    expect(es.withCredentials).toBe(true)

    es.emit('open', undefined)
    es.emit('text_delta', 'hello ')
    es.emit('text_delta', 'world')
    es.emit('complete', 999)

    expect(calls.open).toHaveLength(1)
    expect(calls.delta).toEqual(['hello ', 'world'])
    expect(calls.complete).toEqual([999])
    expect(es.closed).toBe(true)
  })

  it('forwards tool_use and tool_result events as chips', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(acceptedResponse('t1'))
    const { handlers, calls } = makeHandlers()
    await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
      },
    )
    const es = FakeEventSource.instances[0]
    es.emit('tool_use', { name: 'grep', args: { pattern: 'foo' } })
    es.emit('tool_result', { name: 'grep', exit: 0 })
    expect(calls.tool).toEqual([
      { kind: 'tool_use', raw: { name: 'grep', args: { pattern: 'foo' } } },
      { kind: 'tool_result', raw: { name: 'grep', exit: 0 } },
    ])
  })

  it('treats an Event without data as a transient transport blip', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(acceptedResponse('t1'))
    const { handlers, calls } = makeHandlers()
    await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
      },
    )
    const es = FakeEventSource.instances[0]
    es.emitTransientError()
    expect(calls.transient).toHaveLength(1)
    expect(calls.error).toHaveLength(0)
    expect(es.closed).toBe(false)
  })

  it('treats a CLOSED EventSource error event as terminal (no auto-reconnect)', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(acceptedResponse('t1'))
    const { handlers, calls } = makeHandlers()
    await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
      },
    )
    const es = FakeEventSource.instances[0]
    es.emitClosedError()
    expect(calls.error).toHaveLength(1)
    expect(calls.transient).toHaveLength(0)
    expect(es.closed).toBe(true)
  })

  it('treats a named error event with data as terminal and closes the source', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(acceptedResponse('t1'))
    const { handlers, calls } = makeHandlers()
    await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
      },
    )
    const es = FakeEventSource.instances[0]
    es.emit('error', 'upstream down')
    expect(calls.error).toEqual(['upstream down'])
    expect(es.closed).toBe(true)
  })

  it('cancel() closes the EventSource and silences further handlers', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(acceptedResponse('t1'))
    const { handlers, calls } = makeHandlers()
    const handle = await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
      },
    )
    const es = FakeEventSource.instances[0]
    handle.cancel()
    expect(es.closed).toBe(true)
    // Late events arriving after cancel must not invoke handlers.
    es.emit('text_delta', 'late')
    es.emit('complete', 1)
    expect(calls.delta).toHaveLength(0)
    expect(calls.complete).toHaveLength(0)
  })
})

describe('startTurn — 200 sync branch', () => {
  it('routes a 200 response to onSync without opening a stream', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(
      jsonResponse({
        session: { id: 7, title: 'x', status: 'draft', stage: 'ready', created_at: '', updated_at: '', message_count: 0 },
        messages: [],
      }),
    )
    const { handlers, calls } = makeHandlers()
    const handle = await startTurn(
      7,
      { mark_ready: true },
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
      },
    )
    expect(handle.streaming).toBe(false)
    expect(FakeEventSource.instances).toHaveLength(0)
    expect(calls.sync).toHaveLength(1)
  })
})

describe('startTurn — error paths', () => {
  it('throws ApiError on a 4xx response and does not invoke handlers', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(
      jsonResponse({ error: 'bad request' }, 400),
    )
    const { handlers, calls } = makeHandlers()
    await expect(
      startTurn(
        1,
        {},
        handlers,
        {
          fetchImpl: fetchMock as unknown as typeof fetch,
          eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
        },
      ),
    ).rejects.toBeInstanceOf(ApiError)
    expect(calls.sync).toHaveLength(0)
    expect(FakeEventSource.instances).toHaveLength(0)
  })

  it('throws on 401 with status=401', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse({}, 401))
    const { handlers } = makeHandlers()
    await expect(
      startTurn(
        1,
        {},
        handlers,
        {
          fetchImpl: fetchMock as unknown as typeof fetch,
          eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
        },
      ),
    ).rejects.toMatchObject({ status: 401 })
  })

  it('throws when the 202 body is missing turn_id', async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(
      new Response('{}', {
        status: 202,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    const { handlers } = makeHandlers()
    await expect(
      startTurn(
        1,
        {},
        handlers,
        {
          fetchImpl: fetchMock as unknown as typeof fetch,
          eventSourceImpl: FakeEventSource as unknown as typeof EventSource,
        },
      ),
    ).rejects.toMatchObject({ status: 202 })
  })
})

// waitFor polls a predicate until it returns true or the timeout expires.
// Used by polling-fallback tests instead of vi.useFakeTimers because the
// polling loop interleaves `setTimeout` with `await fetch`, which is
// finicky to drive deterministically with fake timers.
async function waitFor(
  predicate: () => boolean,
  timeoutMs = 1000,
  pollMs = 5,
): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (!predicate()) {
    if (Date.now() > deadline) {
      throw new Error('waitFor: predicate did not become true within timeout')
    }
    await new Promise((r) => setTimeout(r, pollMs))
  }
}

describe('startTurn — polling fallback', () => {
  it('falls back to polling when EventSource is unavailable', async () => {
    const fetchMock = vi
      .fn()
      // POST /turn → 202
      .mockResolvedValueOnce(acceptedResponse('poll-1'))
      // First poll snapshot — running, partial text.
      .mockResolvedValueOnce(
        jsonResponse({
          id: 'poll-1',
          session_id: 1,
          status: 'running',
          text: 'first ',
          tool_events: [{ type: 'tool_use', data: { name: 'grep' } }],
        }),
      )
      // Second poll — complete.
      .mockResolvedValueOnce(
        jsonResponse({
          id: 'poll-1',
          session_id: 1,
          status: 'complete',
          text: 'first second',
          final_message_id: 99,
        }),
      )

    const { handlers, calls } = makeHandlers()
    const handle = await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        forcePolling: true,
        pollIntervalMs: 5,
      },
    )
    expect(handle.streaming).toBe(true)

    await waitFor(() => calls.complete.length > 0)
    expect(calls.delta).toEqual(['first ', 'second'])
    expect(calls.tool).toEqual([{ kind: 'tool_use', raw: { name: 'grep' } }])
    expect(calls.complete).toEqual([99])
  })

  it('stops polling on snapshot.status=error', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(acceptedResponse('poll-err'))
      .mockResolvedValueOnce(
        jsonResponse({
          id: 'poll-err',
          session_id: 1,
          status: 'error',
          error: 'runner crashed',
        }),
      )
    const { handlers, calls } = makeHandlers()
    await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        forcePolling: true,
        pollIntervalMs: 5,
      },
    )
    await waitFor(() => calls.error.length > 0)
    expect(calls.error).toEqual(['runner crashed'])
    expect(calls.complete).toHaveLength(0)
  })

  it('cancel() stops the polling loop', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(acceptedResponse('poll-c'))
      .mockResolvedValue(
        jsonResponse({
          id: 'poll-c',
          session_id: 1,
          status: 'running',
          text: 'streaming…',
        }),
      )
    const { handlers, calls } = makeHandlers()
    const handle = await startTurn(
      1,
      {},
      handlers,
      {
        fetchImpl: fetchMock as unknown as typeof fetch,
        forcePolling: true,
        pollIntervalMs: 5,
      },
    )
    await waitFor(() => calls.delta.length > 0)
    expect(calls.delta).toEqual(['streaming…'])
    handle.cancel()
    const before = fetchMock.mock.calls.length
    // Wait long enough for another poll, if the loop were still running.
    await new Promise((r) => setTimeout(r, 40))
    expect(fetchMock.mock.calls.length).toBe(before)
    expect(calls.complete).toHaveLength(0)
  })

  it('detects missing EventSource and falls back automatically', async () => {
    const originalES = globalThis.EventSource
    // @ts-expect-error — intentionally erasing for fallback detection.
    delete globalThis.EventSource
    try {
      const fetchMock = vi
        .fn()
        .mockResolvedValueOnce(acceptedResponse('auto-poll'))
        .mockResolvedValueOnce(
          jsonResponse({
            id: 'auto-poll',
            session_id: 1,
            status: 'complete',
            text: 'ok',
            final_message_id: 5,
          }),
        )
      const { handlers, calls } = makeHandlers()
      await startTurn(
        1,
        {},
        handlers,
        {
          fetchImpl: fetchMock as unknown as typeof fetch,
          pollIntervalMs: 5,
        },
      )
      await waitFor(() => calls.complete.length > 0)
      expect(calls.complete).toEqual([5])
    } finally {
      if (originalES) globalThis.EventSource = originalES
    }
  })
})
