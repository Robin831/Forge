import { Activity } from 'lucide-react'
import type { EventInfo } from '../api'
import { eventClasses, relativeTime } from '../lib/format'
import Pane, { EmptyState } from './Pane'

interface EventsPaneProps {
  events: EventInfo[]
  loading: boolean
  error: string | null
}

export default function EventsPane({ events, loading, error }: EventsPaneProps) {
  return (
    <Pane
      title="Recent events"
      icon={<Activity size={16} className="text-purple-400" aria-hidden />}
      count={events.length}
      loading={loading}
      error={error}
    >
      {events.length === 0 && !loading ? (
        <EmptyState message="No events yet." />
      ) : (
        <ul className="divide-y divide-slate-800">
          {events.map((e) => (
            <li key={e.id} className="px-4 py-2.5">
              <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
                <span className={`font-mono text-[11px] ${eventClasses(e.type)}`}>
                  {e.type}
                </span>
                <span className="ml-auto text-slate-500" title={e.timestamp}>
                  {relativeTime(e.timestamp)}
                </span>
              </div>
              <p className="mt-0.5 break-words text-sm text-slate-200">
                {e.message}
              </p>
              {(e.bead_id || e.anvil) && (
                <p className="mt-0.5 flex flex-wrap items-center gap-x-2 text-[11px] text-slate-500">
                  {e.bead_id && (
                    <span className="font-mono text-slate-400">{e.bead_id}</span>
                  )}
                  {e.bead_id && e.anvil && <span aria-hidden>·</span>}
                  {e.anvil && <span>{e.anvil}</span>}
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </Pane>
  )
}
