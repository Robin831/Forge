import { useEffect, useRef, useState } from 'react'

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

// useEventSource is a thin wrapper around the browser's native EventSource
// that buffers incoming events in React state and exposes a connection
// status. It relies on the browser's built-in reconnect logic, which honours
// the `retry: 3000` directive emitted by the backend handlers.
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
    if (!enabled || !url) {
      setStatus('closed')
      return
    }

    setStatus('connecting')
    setError(null)
    const es = new EventSource(url, { withCredentials: true })

    es.onopen = () => {
      setStatus('open')
      setError(null)
    }

    es.onmessage = (ev) => {
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

    es.onerror = () => {
      // The browser will reconnect automatically; reflect the transient
      // disconnection in the UI but keep the buffer.
      setStatus('error')
      setError('connection lost')
    }

    return () => {
      es.close()
      setStatus('closed')
    }
  }, [url, enabled, parse, maxItems])

  const clear = () => {
    itemsRef.current = []
    setItems([])
  }

  return { items, status, error, clear }
}
