// turnStream wires the SPA into the backend's async turn pipeline.
//
// Flow:
//   1. POST /api/forge/sessions/{id}/turn with the user-supplied request body.
//      - 200: backend completed synchronously (e.g. mark_ready, settled-ready
//        no-op). Hand the parsed body to onSync and we're done.
//      - 202: backend scheduled an AI turn. Body is `{turn_id}`. Open the SSE
//        stream and forward each event to the matching handler.
//   3. Cancellation: callers receive a handle whose cancel() closes the
//      EventSource (or stops the polling fallback) and prevents any further
//      handler invocations. Safe to call multiple times. The component using
//      this should invoke cancel() on unmount or before starting a new turn.
//
// Polling fallback: in environments where `EventSource` is not defined (some
// embedded browsers, certain integration test harnesses), we poll the
// snapshot endpoint at a short interval, diff against the previously-seen
// state, and synthesise the same handler invocations.

import { ApiError, type ForgeTurnRequest, type ForgeTurnResponse } from '../api'

// ChipKind matches the SSE event names emitted by the runner for tool
// invocations. The SPA renders these as compact inline status chips inside
// the streaming assistant bubble.
export type ChipKind = 'tool_use' | 'tool_result'

// ToolChipData is what the consumer receives for each tool event. The raw
// payload is preserved so the UI can extract a name / summary heuristically
// — the backend's event schema is intentionally loose so callers should
// remain forgiving.
export interface ToolChipData {
  kind: ChipKind
  raw: unknown
}

// StreamTurnHandlers is the callback set passed to startTurn. All handlers
// are optional so callers can wire only what they need.
export interface StreamTurnHandlers {
  // onSync fires for the 200 (no-AI) response branch. Resolves with the same
  // {session, messages} shape the legacy forgeSessions.turn() returned.
  onSync?: (payload: ForgeTurnResponse) => void
  // onTurnId fires once the POST returns 202 and we have the server-side
  // identifier. Callers may store this for retries or diagnostic display.
  onTurnId?: (turnId: string) => void
  // onOpen fires the first time the underlying transport is ready to deliver
  // events (EventSource onopen, or first successful poll). Callers use this
  // to hide a "submitting…" spinner before any payload arrives.
  onOpen?: () => void
  // onTextDelta delivers a streamed chunk of assistant text. Callers should
  // append these into the active assistant bubble in the order received.
  onTextDelta?: (chunk: string) => void
  // onTool delivers a tool_use or tool_result event. The SPA renders these
  // as inline chips; the payload structure is intentionally opaque.
  onTool?: (chip: ToolChipData) => void
  // onTransientError reports a transient transport problem (e.g. EventSource
  // onerror). The browser auto-reconnects so the stream may resume — callers
  // typically render a quiet "reconnecting…" banner rather than tearing the
  // bubble down.
  onTransientError?: (message: string) => void
  // onComplete is the terminal success notification. finalMessageId is the
  // server-side id of the last persisted assistant message, or null when the
  // runner emitted nothing. Implementations typically refetch the session
  // after this to load the canonical message rows.
  onComplete?: (finalMessageId: number | null) => void
  // onError is the terminal failure notification. The transport has been
  // closed by the time this fires; no further handlers will be invoked.
  onError?: (message: string) => void
}

// StreamTurnHandle is returned from startTurn. cancel() releases the
// underlying transport and prevents any further handler invocations.
export interface StreamTurnHandle {
  cancel: () => void
  // streaming is true when the backend returned 202 and a stream/poll loop
  // is active. false for the synchronous 200 branch.
  streaming: boolean
}

// StartTurnOptions tunes test injection points and polling behaviour.
// Production callers should rely on the defaults.
export interface StartTurnOptions {
  // forcePolling skips the EventSource path entirely. Used by tests that
  // want to exercise the fallback regardless of jsdom's capabilities.
  forcePolling?: boolean
  // pollIntervalMs overrides the snapshot polling interval (default 500ms).
  pollIntervalMs?: number
  // fetchImpl injects a custom fetch (testing).
  fetchImpl?: typeof fetch
  // eventSourceImpl injects a custom EventSource constructor (testing).
  eventSourceImpl?: typeof EventSource
}

const DEFAULT_POLL_INTERVAL_MS = 500

// startTurn POSTs to the turn endpoint and routes the response to either a
// streaming or polling consumer. Returns a handle the caller can use to
// cancel before the turn completes.
export async function startTurn(
  sessionId: number,
  request: ForgeTurnRequest,
  handlers: StreamTurnHandlers,
  opts: StartTurnOptions = {},
): Promise<StreamTurnHandle> {
  const doFetch = opts.fetchImpl ?? fetch
  const path = `/api/forge/sessions/${sessionId}/turn`
  const res = await doFetch(path, {
    method: 'POST',
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      'X-Forge-Action': '1',
    },
    body: JSON.stringify(request ?? {}),
  })

  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  const text = await res.text()
  let parsed: unknown = null
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = null
    }
  }
  if (!res.ok) {
    const msg = (parsed as { error?: string })?.error ?? `HTTP ${res.status}`
    throw new ApiError(res.status, msg)
  }

  // 200: synchronous branch. The body is {session, messages}.
  if (res.status === 200) {
    if (handlers.onSync && parsed && typeof parsed === 'object') {
      handlers.onSync(parsed as ForgeTurnResponse)
    }
    return { cancel: () => {}, streaming: false }
  }

  // 202: async branch. Body is {turn_id}.
  const turnId = (parsed as { turn_id?: string } | null)?.turn_id
  if (!turnId) {
    throw new ApiError(res.status, 'missing turn_id in async response')
  }
  handlers.onTurnId?.(turnId)

  const eventSourceImpl = opts.eventSourceImpl ?? globalEventSource()
  if (opts.forcePolling || !eventSourceImpl) {
    return startPollingFallback(sessionId, turnId, handlers, doFetch, opts)
  }
  return startEventSource(sessionId, turnId, handlers, eventSourceImpl)
}

// globalEventSource returns the browser's EventSource constructor when one
// exists, or undefined in environments that don't expose it (jsdom, certain
// SSR contexts). The runtime check uses `typeof` so this stays SSR-safe.
function globalEventSource(): typeof EventSource | undefined {
  if (typeof EventSource === 'undefined') return undefined
  return EventSource
}

// startEventSource opens the named-event SSE stream and wires each event
// type to the matching handler. Handler invocations stop after cancel()
// even if the EventSource is still flushing buffered events.
function startEventSource(
  sessionId: number,
  turnId: string,
  handlers: StreamTurnHandlers,
  EventSourceCtor: typeof EventSource,
): StreamTurnHandle {
  const url = `/api/forge/sessions/${sessionId}/turn/${encodeURIComponent(turnId)}/stream`
  const es = new EventSourceCtor(url, { withCredentials: true })
  let cancelled = false

  const close = () => {
    if (cancelled) return
    cancelled = true
    es.close()
  }

  es.addEventListener('open', () => {
    if (cancelled) return
    handlers.onOpen?.()
  })

  es.addEventListener('text_delta', (ev: MessageEvent) => {
    if (cancelled) return
    const data = safeParse(ev.data)
    if (typeof data === 'string') {
      handlers.onTextDelta?.(data)
    }
  })

  es.addEventListener('tool_use', (ev: MessageEvent) => {
    if (cancelled) return
    handlers.onTool?.({ kind: 'tool_use', raw: safeParse(ev.data) })
  })

  es.addEventListener('tool_result', (ev: MessageEvent) => {
    if (cancelled) return
    handlers.onTool?.({ kind: 'tool_result', raw: safeParse(ev.data) })
  })

  es.addEventListener('complete', (ev: MessageEvent) => {
    if (cancelled) return
    const finalId = coerceFinalId(safeParse(ev.data))
    close()
    handlers.onComplete?.(finalId)
  })

  // turn_expired fires when the server no longer has this turn (GC expiry,
  // retention-cap eviction, or a daemon restart). Treat it like a terminal
  // complete with no final id: close the stream and let the caller refetch
  // the canonical messages, which clears the spinner without a dangling
  // "reconnecting…" state.
  es.addEventListener('turn_expired', () => {
    if (cancelled) return
    close()
    handlers.onComplete?.(null)
  })

  es.addEventListener('error', (ev: MessageEvent | Event) => {
    if (cancelled) return
    // Two failure modes share onerror:
    //  - A named "error" event carrying a server-side error payload.
    //    `MessageEvent.data` is populated.
    //  - An EventSource transport problem (HTTP error, network blip).
    //    `data` is undefined and the browser will auto-reconnect.
    const data = (ev as MessageEvent).data
    if (typeof data === 'string' && data.length > 0) {
      const parsed = safeParse(data)
      const message =
        typeof parsed === 'string'
          ? parsed
          : isStringy((parsed as { error?: string })?.error)
            ? ((parsed as { error?: string }).error as string)
            : 'turn failed'
      close()
      handlers.onError?.(message)
      return
    }
    // If the EventSource is CLOSED (readyState === 2) it will not
    // auto-reconnect; treat as terminal so the UI doesn't get stuck in
    // perpetual "reconnecting…" state.
    if (typeof es.readyState === 'number' && es.readyState === 2) {
      close()
      handlers.onError?.('connection closed')
      return
    }
    // Transient transport error — keep the connection going so the
    // browser's reconnect kicks in. Surface to the caller for UI feedback.
    handlers.onTransientError?.('connection lost — retrying')
  })

  return { cancel: close, streaming: true }
}

// startPollingFallback drives the GET snapshot endpoint on a short interval.
// It tracks the previously-seen text length and tool-event count so each
// poll surfaces just the new deltas. The loop stops on terminal status,
// cancel(), or an unrecoverable fetch error.
function startPollingFallback(
  sessionId: number,
  turnId: string,
  handlers: StreamTurnHandlers,
  doFetch: typeof fetch,
  opts: StartTurnOptions,
): StreamTurnHandle {
  const intervalMs = opts.pollIntervalMs ?? DEFAULT_POLL_INTERVAL_MS
  const path = `/api/forge/sessions/${sessionId}/turn/${encodeURIComponent(turnId)}`
  let cancelled = false
  let timer: ReturnType<typeof setTimeout> | null = null
  let opened = false
  let lastPollFailed = false
  let textCursor = 0
  let toolCursor = 0

  const stop = () => {
    if (cancelled) return
    cancelled = true
    if (timer !== null) {
      clearTimeout(timer)
      timer = null
    }
  }

  const tick = async () => {
    if (cancelled) return
    try {
      const res = await doFetch(path, { credentials: 'include' })
      if (cancelled) return
      if (res.status === 401) {
        stop()
        handlers.onError?.('unauthorized')
        return
      }
      if (res.status === 404) {
        // The turn is gone (GC expiry, retention-cap eviction, or a daemon
        // restart). Mirror the SSE turn_expired path: stop polling and let the
        // caller refetch canonical messages so the spinner clears instead of
        // polling a dead turn forever.
        stop()
        handlers.onComplete?.(null)
        return
      }
      if (!res.ok) {
        // Treat as transient — schedule another poll. The runner may still
        // be initialising the turn record.
        lastPollFailed = true
        handlers.onTransientError?.(`HTTP ${res.status}`)
        schedule()
        return
      }
      const snap = (await res.json()) as TurnSnapshotJSON
      if (cancelled) return
      if (!opened) {
        opened = true
        handlers.onOpen?.()
      } else if (lastPollFailed) {
        // A previously-failed poll recovered — signal transport recovery so
        // consumers (e.g. ForgePage) can clear their "reconnecting…" banner.
        handlers.onOpen?.()
      }
      lastPollFailed = false
      // Stream text deltas relative to the previously-seen tail of Text.
      const text = typeof snap.text === 'string' ? snap.text : ''
      if (text.length > textCursor) {
        const chunk = text.slice(textCursor)
        textCursor = text.length
        handlers.onTextDelta?.(chunk)
      }
      // Stream tool events relative to the previously-seen length of the
      // tool_events array.
      const tools = Array.isArray(snap.tool_events) ? snap.tool_events : []
      while (toolCursor < tools.length) {
        const ev = tools[toolCursor++]
        if (!ev || typeof ev !== 'object') continue
        const t = (ev as { type?: string }).type
        if (t === 'tool_use' || t === 'tool_result') {
          handlers.onTool?.({ kind: t, raw: (ev as { data?: unknown }).data })
        }
      }
      const status = typeof snap.status === 'string' ? snap.status : ''
      if (status === 'complete') {
        stop()
        const finalId =
          typeof snap.final_message_id === 'number'
            ? snap.final_message_id
            : null
        handlers.onComplete?.(finalId && finalId > 0 ? finalId : null)
        return
      }
      if (status === 'error') {
        stop()
        const msg =
          typeof snap.error === 'string' && snap.error.length > 0
            ? snap.error
            : 'turn failed'
        handlers.onError?.(msg)
        return
      }
      schedule()
    } catch (err) {
      if (cancelled) return
      // Fetch-layer failure (offline, DNS, abort). Surface as terminal so
      // the caller can show an error state — polling has no auto-recovery.
      stop()
      const msg = err instanceof Error ? err.message : 'poll failed'
      handlers.onError?.(msg)
    }
  }

  const schedule = () => {
    if (cancelled) return
    timer = setTimeout(() => {
      timer = null
      void tick()
    }, intervalMs)
  }

  // Kick off immediately so callers see the first payload without waiting
  // the full interval.
  void tick()

  return { cancel: stop, streaming: true }
}

// TurnSnapshotJSON matches the wire shape of TurnSnapshot (Go) used by the
// GET fallback. Kept local so the polling loop has a typed surface without
// re-exporting state-management types from api.ts.
interface TurnSnapshotJSON {
  id?: string
  session_id?: number
  status?: string
  text?: string
  tool_events?: unknown[]
  final_message_id?: number
  error?: string
}

// safeParse JSON-parses a string, returning the original value on failure
// so unstructured payloads (e.g. a bare quoted string for text_delta) round
// trip cleanly.
function safeParse(raw: unknown): unknown {
  if (typeof raw !== 'string') return raw
  try {
    return JSON.parse(raw)
  } catch {
    return raw
  }
}

// coerceFinalId normalises the complete event's data into an id|null. The
// backend sends an int64 (so JSON.parse yields a number), but we accept
// numeric strings as well for compatibility with future shape changes.
function coerceFinalId(value: unknown): number | null {
  if (typeof value === 'number' && Number.isFinite(value) && value > 0) {
    return value
  }
  if (typeof value === 'string') {
    const n = Number(value)
    if (Number.isFinite(n) && n > 0) return n
  }
  return null
}

function isStringy(v: unknown): v is string {
  return typeof v === 'string' && v.length > 0
}
