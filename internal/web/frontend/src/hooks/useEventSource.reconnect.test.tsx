import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { useEventSource } from './useEventSource'

// FakeEventSource stands in for the browser's EventSource so a test can drive
// the two failure modes apart: a transient drop (readyState CONNECTING — the
// browser retries on its own) versus a permanently failed connection
// (readyState CLOSED — a non-2xx initial response, which the browser never
// retries).
class FakeEventSource {
  static instances: FakeEventSource[] = []
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2

  url: string
  readyState = FakeEventSource.CONNECTING
  closed = false
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string }) => void) | null = null
  onerror: (() => void) | null = null

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  close() {
    this.closed = true
    this.readyState = FakeEventSource.CLOSED
  }

  // Server accepted the stream.
  emitOpen() {
    this.readyState = FakeEventSource.OPEN
    this.onopen?.()
  }

  // Connection dropped mid-stream; the browser will retry itself.
  emitTransientError() {
    this.readyState = FakeEventSource.CONNECTING
    this.onerror?.()
  }

  // Initial response was a non-2xx (429 SSE cap, 404 missing log path).
  // The browser gives up permanently.
  emitFatalError() {
    this.readyState = FakeEventSource.CLOSED
    this.onerror?.()
  }
}

const last = () => FakeEventSource.instances[FakeEventSource.instances.length - 1]

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('useEventSource reconnect', () => {
  it('reconnects after a permanently-closed connection', () => {
    const { result } = renderHook(() => useEventSource<{ line: string }>('/api/worker/w1/stream'))
    expect(FakeEventSource.instances).toHaveLength(1)

    act(() => last().emitFatalError())
    expect(result.current.status).toBe('error')

    act(() => void vi.advanceTimersByTime(3000))
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(last().url).toBe('/api/worker/w1/stream')

    act(() => last().emitOpen())
    expect(result.current.status).toBe('open')
  })

  it('leaves a transient drop to the browser', () => {
    renderHook(() => useEventSource('/api/activity/stream'))
    act(() => last().emitOpen())

    act(() => last().emitTransientError())
    act(() => void vi.advanceTimersByTime(60000))

    // Still the original connection — no manual reconnect piled on top of the
    // browser's own retry.
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('backs off exponentially while the failure persists', () => {
    renderHook(() => useEventSource('/api/worker/w2/stream'))

    act(() => last().emitFatalError())
    act(() => void vi.advanceTimersByTime(3000))
    expect(FakeEventSource.instances).toHaveLength(2)

    // Second failure waits 6s, not 3s.
    act(() => last().emitFatalError())
    act(() => void vi.advanceTimersByTime(3000))
    expect(FakeEventSource.instances).toHaveLength(2)
    act(() => void vi.advanceTimersByTime(3000))
    expect(FakeEventSource.instances).toHaveLength(3)
  })

  it('resets the backoff once a connection succeeds', () => {
    renderHook(() => useEventSource('/api/worker/w3/stream'))

    act(() => last().emitFatalError())
    act(() => void vi.advanceTimersByTime(3000))
    act(() => last().emitOpen())

    // Backoff reset, so the next failure retries at the base delay again.
    act(() => last().emitFatalError())
    act(() => void vi.advanceTimersByTime(3000))
    expect(FakeEventSource.instances).toHaveLength(3)
  })

  it('does not reconnect after unmount', () => {
    const { unmount } = renderHook(() => useEventSource('/api/worker/w4/stream'))

    act(() => last().emitFatalError())
    unmount()
    act(() => void vi.advanceTimersByTime(60000))

    expect(FakeEventSource.instances).toHaveLength(1)
  })
})
