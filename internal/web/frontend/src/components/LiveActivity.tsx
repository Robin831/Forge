import { useMemo } from 'react'
import { Activity, AlertCircle, RefreshCw } from 'lucide-react'
import type { EventInfo } from '../api'
import { useEventSource } from '../hooks/useEventSource'
import { eventClasses, relativeTime } from '../lib/format'
import Pane, { EmptyState } from './Pane'

const MAX_EVENTS = 200

// LiveActivity renders the global event stream. It opens an EventSource on
// /api/activity/stream and renders the newest event at the top so the user
// never has to scroll past stale entries to see what just happened. The
// browser preserves scrollTop when content is prepended, so a user who has
// scrolled down to read older events stays where they are.
export default function LiveActivity() {
  const { items, status, error } = useEventSource<EventInfo>('/api/activity/stream', {
    maxItems: MAX_EVENTS,
  })

  const ordered = useMemo(() => [...items].reverse(), [items])

  // Surface error state but never block the rendering of buffered events —
  // the browser will reconnect automatically and new events resume flowing.
  const banner = error ? (
    <div className="flex items-center gap-2 border-b border-amber-700/40 bg-amber-900/20 px-4 py-1.5 text-xs text-amber-300">
      <AlertCircle size={12} />
      <span>{error}</span>
      {status === 'error' && <RefreshCw size={12} className="ml-auto animate-spin" />}
    </div>
  ) : null

  return (
    <Pane
      title="Live activity"
      icon={<Activity size={16} className="text-purple-400" aria-hidden />}
      count={items.length}
      loading={status === 'connecting'}
    >
      {banner}
      <div
        className="h-full overflow-y-auto"
        role="log"
        aria-live="polite"
      >
        {ordered.length === 0 ? (
          <EmptyState
            message={
              status === 'connecting' ? 'Connecting to event stream…' : 'No events yet.'
            }
          />
        ) : (
          <ul className="divide-y divide-slate-800">
            {ordered.map((e) => (
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
        )}
      </div>
    </Pane>
  )
}
