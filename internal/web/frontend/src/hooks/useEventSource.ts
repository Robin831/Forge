import { useCallback, useEffect, useRef, useState } from 'react'

export type SSEStatus = 'connecting' | 'open' | 'error' | 'closed'

interface UseEventSourceOptions<T> {
  // parse turns the raw `data:` string into the consumer's record type. Errors
  // thrown here are caught and the message is dropped silently — preventing a
  // single malformed SSE frame from killing the stream.
  parse?: (raw: string) => T | null
  // maxItems caps the in-memory buffer so long-running connections don't pin
  // an ever-growing array in memory. Defaults to 500.
  maxItems?: number
  // enabled lets callers conditionally start/stop the connection without
  // unmounting the component (e.g. pause when a modal closes).
  enabled?: boolean
}

interface UseEventSourceResult<T> {
  items: T[]
  status: SSEStatus
  error: string | null
  // clear empties the in-memory buffer (e.g. when switching the worker shown
  // in a modal).
  clear: () => void
}

// RECONNECT_BASE_MS matches the backend's `retry: 3000` directive so a manual
// reattempt keeps the same first-retry cadence the browser would have used.
const RECONNECT_BASE_MS = 3000
const RECONNECT_MAX_MS = 30000

// useEventSource is a thin wrapper around the browser's native EventSource
// that buffers incoming events in React state and exposes a connection status.
//
// The browser's built-in reconnect (honouring `retry: 3000`) covers the normal
// case of a dropped connection, but it only applies when a stream that opened
// successfully later breaks. If the *initial* response is a non-2xx — a 429
// from the per-session SSE cap, or a 404 for a worker whose log path hasn't
// been recorded yet — the browser fails the connection permanently and never
// retries, leaving a panel stuck on "reconnecting" until it is unmounted and
// remounted (i.e. until you navigate away and back). So we watch for a CLOSED
// readyState in onerror and drive our own reconnect with backoff.
export function useEventSource<T>(
  url: string | null,
  opts: UseEventSourceOptions<T> = {},
): UseEventSourceResult<T> {
  const { parse, maxItems = 500, enabled = true } = opts

  const [items, setItems] = useState<T[]>([])
  const [status, setStatus] = useState<SSEStatus>('connecting')
  const [error, setError] = useState<string | null>(null)
  const itemsRef = useRef<T[]>([])

  useEffect(() => {
    // Clear the buffer on every URL/enabled change so stale frames from a
    // previous connection don't show when the caller switches the target URL.
    itemsRef.current = []
    setItems([])

    if (!enabled || !url) {
      setStatus('closed')
      return
    }

    setStatus('connecting')
    setError(null)

    let es: EventSource | null = null
    let retryTimer: ReturnType<typeof setTimeout> | null = null
    let attempt = 0
    let disposed = false

    const connect = () => {
      if (disposed) return
      const src = new EventSource(url, { withCredentials: true })
      es = src

      src.onopen = () => {
        attempt = 0
        setStatus('open')
        setError(null)
      }

      src.onmessage = (ev) => {
        try {
          let payload: T | null
          if (parse) {
            payload = parse(ev.data)
          } else {
            payload = JSON.parse(ev.data) as T
          }
          if (payload === null || payload === undefined) return
          const next = [...itemsRef.current, payload]
          if (next.length > maxItems) {
            next.splice(0, next.length - maxItems)
          }
          itemsRef.current = next
          setItems(next)
        } catch {
          // Drop malformed frames silently.
        }
      }

      src.onerror = () => {
        if (disposed) return
        setStatus('error')
        setError('connection lost')
        // readyState CONNECTING means the browser is already retrying on its
        // own; leave it alone. CLOSED means it has given up for good (a
        // non-2xx initial response), so reconnect ourselves with backoff.
        if (src.readyState !== EventSource.CLOSED) return
        src.close()
        const delay = Math.min(RECONNECT_BASE_MS * 2 ** attempt, RECONNECT_MAX_MS)
        attempt++
        retryTimer = setTimeout(() => {
          retryTimer = null
          setStatus('connecting')
          connect()
        }, delay)
      }
    }

    connect()

    return () => {
      disposed = true
      if (retryTimer) clearTimeout(retryTimer)
      es?.close()
      setStatus('closed')
    }
  }, [url, enabled, parse, maxItems])

  const clear = useCallback(() => {
    itemsRef.current = []
    setItems([])
  }, [])

  return { items, status, error, clear }
}
