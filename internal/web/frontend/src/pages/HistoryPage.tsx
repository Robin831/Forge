import { useState } from 'react'
import { History as HistoryIcon } from 'lucide-react'
import { Link } from 'react-router-dom'
import { useApiPoll } from '../hooks/useApiPoll'
import type { EventsResponse, HistoryWorkersResponse, StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
import Pane, { EmptyState } from '../components/Pane'
import { eventClasses, relativeTime } from '../lib/format'

const POLL_INTERVAL_MS = 10000

const STATUS_CLASSES: Record<string, string> = {
  done: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
  timeout: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  stalled: 'bg-orange-500/20 text-orange-300 border-orange-500/40',
}

function statusClass(s: string): string {
  return STATUS_CLASSES[s] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

function formatDuration(seconds?: number): string {
  if (!seconds || seconds <= 0) return ''
  if (seconds < 60) return `${Math.round(seconds)}s`
  const mins = Math.floor(seconds / 60)
  const secs = Math.round(seconds % 60)
  if (mins < 60) return `${mins}m ${secs}s`
  const hrs = Math.floor(mins / 60)
  return `${hrs}h ${mins % 60}m`
}

export default function HistoryPage() {
  const [tab, setTab] = useState<'workers' | 'events'>('workers')
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const workers = useApiPoll<HistoryWorkersResponse>('/api/history/workers?limit=200', POLL_INTERVAL_MS)
  const events = useApiPoll<EventsResponse>('/api/events?limit=200', POLL_INTERVAL_MS)

  return (
    <div className="flex min-h-full flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <section
        aria-label="History tabs"
        className="inline-flex w-fit gap-1 rounded-lg border border-slate-800 bg-slate-900/60 p-1 text-sm"
      >
        <TabButton active={tab === 'workers'} onClick={() => setTab('workers')}>
          Workers
        </TabButton>
        <TabButton active={tab === 'events'} onClick={() => setTab('events')}>
          Events
        </TabButton>
      </section>

      {tab === 'workers' ? (
        <Pane
          title="Worker history"
          icon={<HistoryIcon size={16} className="text-sky-400" aria-hidden />}
          count={workers.data?.workers.length ?? 0}
          loading={workers.loading}
          error={workers.error}
        >
          {(workers.data?.workers.length ?? 0) === 0 && !workers.loading ? (
            <EmptyState message="No completed workers yet." />
          ) : (
            <ul className="divide-y divide-slate-800">
              {(workers.data?.workers ?? []).map((w) => (
                <li key={w.id} className="px-4 py-3">
                  <div className="flex flex-wrap items-start gap-2">
                    <span
                      className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${statusClass(w.status)}`}
                    >
                      {w.status}
                    </span>
                    {w.phase && (
                      <span className="rounded-md border border-slate-700 bg-slate-800/60 px-2 py-0.5 text-[10px] uppercase tracking-wide text-slate-300">
                        {w.phase}
                      </span>
                    )}
                    {w.pr_number ? (
                      <span className="rounded-md border border-purple-500/40 bg-purple-500/10 px-2 py-0.5 text-[10px] text-purple-300">
                        PR #{w.pr_number}
                      </span>
                    ) : null}
                    {w.duration_sec ? (
                      <span className="text-[11px] text-slate-500">{formatDuration(w.duration_sec)}</span>
                    ) : null}
                    <span className="ml-auto text-[11px] text-slate-500" title={w.completed_at || w.started_at}>
                      {relativeTime(w.completed_at || w.started_at)}
                    </span>
                  </div>
                  <p className="mt-1.5 truncate text-sm font-medium text-slate-100">
                    {w.title || w.bead_id}
                  </p>
                  <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                    <Link
                      to={`/bead/${w.bead_id}?anvil=${encodeURIComponent(w.anvil)}&tab=logs`}
                      className="font-mono text-slate-400 hover:text-amber-300"
                    >
                      {w.bead_id}
                    </Link>
                    <span aria-hidden>·</span>
                    <span>{w.anvil}</span>
                    {w.branch && (
                      <>
                        <span aria-hidden>·</span>
                        <span className="truncate font-mono text-slate-400">{w.branch}</span>
                      </>
                    )}
                  </p>
                </li>
              ))}
            </ul>
          )}
        </Pane>
      ) : (
        <Pane
          title="Recent events"
          icon={<HistoryIcon size={16} className="text-purple-400" aria-hidden />}
          count={events.data?.events.length ?? 0}
          loading={events.loading}
          error={events.error}
        >
          {(events.data?.events.length ?? 0) === 0 && !events.loading ? (
            <EmptyState message="No events yet." />
          ) : (
            <ul className="divide-y divide-slate-800">
              {(events.data?.events ?? []).map((e) => (
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
                      {e.bead_id && (
                        <Link
                          to={`/bead/${e.bead_id}${e.anvil ? `?anvil=${encodeURIComponent(e.anvil)}` : ''}`}
                          className="font-mono text-slate-400 hover:text-amber-300"
                        >
                          {e.bead_id}
                        </Link>
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
      )}

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}

interface TabButtonProps {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}

function TabButton({ active, onClick, children }: TabButtonProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-md px-3 py-1.5 transition-colors ${
        active
          ? 'bg-amber-400/15 text-amber-200'
          : 'text-slate-300 hover:bg-slate-800/60 hover:text-slate-100'
      }`}
    >
      {children}
    </button>
  )
}
