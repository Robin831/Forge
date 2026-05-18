import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import {
  Activity,
  AlertCircle,
  Pause,
  Play,
  RefreshCw,
  X,
} from 'lucide-react'
import type { EventInfo } from '../api'
import { useEventSource } from '../hooks/useEventSource'
import { useUIState } from '../hooks/useUIState'
import { eventClasses, relativeTime } from '../lib/format'
import Pane, { EmptyState } from './Pane'

const MAX_EVENTS = 200
const PAGE_SIZE = 10

// LiveActivity renders the global event stream. It opens an EventSource on
// /api/activity/stream and renders the newest event at the top so the user
// never has to scroll past stale entries to see what just happened. Browsers
// use scroll anchoring to preserve the user's visual position when content is
// prepended, so a reader scrolled down to older events stays where they are.
export default function LiveActivity() {
  const { items, status, error } = useEventSource<EventInfo>('/api/activity/stream', {
    maxItems: MAX_EVENTS,
  })

  // paused & filter are user preferences — localStorage so they survive a
  // browser restart. Scroll position is transient navigation state —
  // sessionStorage so it survives back-nav but resets when the tab closes.
  // Keys are namespaced by route via useUIState's default `route` scope.
  const [paused, setPaused] = useUIState<boolean>('liveActivity.paused', false, {
    storage: 'local',
  })
  const [filter, setFilter] = useUIState<string>('liveActivity.filter', '', {
    storage: 'local',
  })
  const [scrollTop, setScrollTop] = useUIState<number>('liveActivity.scroll', 0, {
    storage: 'session',
  })
  const filterInputRef = useRef<HTMLInputElement>(null)
  const bodyRef = useRef<HTMLDivElement | null>(null)

  // pauseSnapshot freezes the visible event window when the user toggles
  // paused on. The SSE keeps running so the buffer continues to grow, but
  // the snapshot is what the list renders — mirroring the "freeze frame"
  // behaviour of a tailing log. Initialise with null even when paused is
  // restored from storage so the very first paint shows whatever the
  // buffer holds right now; the post-mount effect then captures that
  // buffer as the frozen snapshot.
  const [pauseSnapshot, setPauseSnapshot] = useState<EventInfo[] | null>(null)

  useEffect(() => {
    if (paused) {
      if (pauseSnapshot === null) {
        setPauseSnapshot([...items])
      }
    } else if (pauseSnapshot !== null) {
      setPauseSnapshot(null)
    }
  }, [paused, items, pauseSnapshot])

  const sourceItems = pauseSnapshot ?? items

  // Filter matches against type, message, bead_id, and anvil — the same
  // surface as QueuePane's filter. Case-insensitive substring match keeps
  // the UX predictable across mixed-case event types and free-form messages.
  const filteredItems = useMemo(() => {
    const q = filter.trim().toLowerCase()
    if (!q) return sourceItems
    return sourceItems.filter((e) => {
      if (e.type.toLowerCase().includes(q)) return true
      if (e.message && e.message.toLowerCase().includes(q)) return true
      if (e.bead_id && e.bead_id.toLowerCase().includes(q)) return true
      if (e.anvil && e.anvil.toLowerCase().includes(q)) return true
      return false
    })
  }, [sourceItems, filter])

  const ordered = useMemo(() => [...filteredItems].reverse(), [filteredItems])
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE)
  const visible = ordered.slice(0, visibleCount)
  const hasMore = visibleCount < ordered.length

  // Restore scroll position before the browser paints so users see no jump
  // from 0 → saved on back-navigation. The dep on `status` re-fires once the
  // SSE connection moves out of "connecting" so the list has actual height
  // to scroll into.
  useLayoutEffect(() => {
    const el = bodyRef.current
    if (!el) return
    if (scrollTop > 0 && el.scrollTop !== scrollTop) {
      el.scrollTop = scrollTop
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status])

  // Throttle scroll capture so we don't write to storage on every pixel.
  // The hook itself debounces writes by 150ms, but updating React state on
  // every scroll event still causes wasteful renders.
  useEffect(() => {
    const el = bodyRef.current
    if (!el) return
    let rafId = 0
    const onScroll = () => {
      if (rafId) return
      rafId = window.requestAnimationFrame(() => {
        rafId = 0
        setScrollTop(el.scrollTop)
      })
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => {
      el.removeEventListener('scroll', onScroll)
      if (rafId) window.cancelAnimationFrame(rafId)
    }
  }, [setScrollTop])

  // Surface error state but never block the rendering of buffered events —
  // the browser will reconnect automatically and new events resume flowing.
  const banner = error ? (
    <div className="flex items-center gap-2 border-b border-amber-700/40 bg-amber-900/20 px-4 py-1.5 text-xs text-amber-300">
      <AlertCircle size={12} />
      <span>{error}</span>
      {status === 'error' && <RefreshCw size={12} className="ml-auto animate-spin" />}
    </div>
  ) : null

  const filterActive = filter.trim().length > 0

  return (
    <Pane
      title="Live activity"
      icon={<Activity size={16} className="text-purple-400" aria-hidden />}
      count={items.length}
      loading={status === 'connecting'}
      bodyRef={bodyRef}
      headerExtra={
        <div className="flex items-center gap-2">
          <div className="relative flex-1">
            <input
              ref={filterInputRef}
              type="text"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Filter events (type, message, bead, anvil)"
              aria-label="Filter events"
              className={`w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1 text-sm text-slate-100 placeholder:text-slate-500 focus:border-amber-400/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300 ${filterActive ? 'pr-7' : ''}`}
            />
            {filterActive && (
              <button
                type="button"
                onClick={() => {
                  setFilter('')
                  filterInputRef.current?.focus()
                }}
                aria-label="Clear filter"
                className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-0.5 text-slate-400 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
              >
                <X size={14} aria-hidden />
              </button>
            )}
          </div>
          <button
            type="button"
            onClick={() => setPaused(!paused)}
            aria-pressed={paused}
            aria-label={paused ? 'Resume live updates' : 'Pause live updates'}
            title={paused ? 'Resume live updates' : 'Pause live updates'}
            className={`shrink-0 rounded-md border p-1.5 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 ${
              paused
                ? 'border-amber-400/60 bg-amber-500/10 text-amber-300 hover:bg-amber-500/20'
                : 'border-slate-700 bg-slate-800 text-slate-300 hover:border-amber-400/40 hover:text-amber-200'
            }`}
          >
            {paused ? <Play size={14} aria-hidden /> : <Pause size={14} aria-hidden />}
          </button>
        </div>
      }
    >
      {banner}
      <div role="log" aria-live={paused ? 'off' : 'polite'}>
        {ordered.length === 0 ? (
          <EmptyState
            message={
              status === 'connecting'
                ? 'Connecting to event stream…'
                : filterActive
                  ? 'No events match the filter.'
                  : paused
                    ? 'Paused. No events captured before pause.'
                    : 'No events yet.'
            }
          />
        ) : (
          <>
            <ul className="divide-y divide-slate-800">
              {visible.map((e) => (
                <li key={e.id} className="px-4 py-2.5">
                  <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
                    <span className={`font-mono text-[11px] ${eventClasses(e.type)}`}>{e.type}</span>
                    <span className="ml-auto text-slate-500" title={e.timestamp}>
                      {relativeTime(e.timestamp)}
                    </span>
                  </div>
                  {e.message && (
                    <p className="mt-0.5 break-words text-sm text-slate-200">{e.message}</p>
                  )}
                  {(e.bead_id || e.anvil) && (
                    <p className="mt-0.5 flex flex-wrap items-center gap-x-2 text-[11px] text-slate-500">
                      {e.bead_id && <span className="font-mono text-slate-400">{e.bead_id}</span>}
                      {e.bead_id && e.anvil && <span aria-hidden>·</span>}
                      {e.anvil && <span>{e.anvil}</span>}
                    </p>
                  )}
                </li>
              ))}
            </ul>
            {hasMore && (
              <div className="border-t border-slate-800 px-4 py-2">
                <button
                  type="button"
                  onClick={() => setVisibleCount((c) => c + PAGE_SIZE)}
                  className="w-full rounded-md border border-slate-700 bg-slate-800/60 px-3 py-1.5 text-xs text-slate-300 hover:bg-slate-800"
                >
                  Fetch more
                </button>
              </div>
            )}
          </>
        )}
      </div>
    </Pane>
  )
}
