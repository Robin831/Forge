import { Users } from 'lucide-react'
import type { WorkerInfo } from '../api'
import { relativeTime } from '../lib/format'
import Pane, { EmptyState } from './Pane'

interface WorkersPaneProps {
  workers: WorkerInfo[]
  loading: boolean
  error: string | null
  onSelectWorker?: (worker: WorkerInfo) => void
}

const STATUS_CLASSES: Record<string, string> = {
  pending: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  running: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  done: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
}

function statusClass(status: string): string {
  return STATUS_CLASSES[status] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

export default function WorkersPane({ workers, loading, error, onSelectWorker }: WorkersPaneProps) {
  // Sort by started_at descending so newest workers are at the top — matches
  // hearth TUI behaviour and Hytte's WorkersCard.
  const sorted = [...workers].sort((a, b) => {
    const aT = Date.parse(a.started_at) || 0
    const bT = Date.parse(b.started_at) || 0
    return bT - aT
  })

  return (
    <Pane
      title="Workers"
      icon={<Users size={16} className="text-sky-400" aria-hidden />}
      count={workers.length}
      loading={loading}
      error={error}
    >
      {sorted.length === 0 && !loading ? (
        <EmptyState message="No active workers." />
      ) : (
        <ul className="divide-y divide-slate-800">
          {sorted.map((w) => {
            const hasLog = !!w.log_path
            const Wrapper = onSelectWorker && hasLog ? 'button' : 'div'
            const handleClick = () => {
              if (onSelectWorker && hasLog) onSelectWorker(w)
            }
            return (
              <li key={w.id}>
                <Wrapper
                  type={Wrapper === 'button' ? 'button' : undefined}
                  onClick={Wrapper === 'button' ? handleClick : undefined}
                  className={`block w-full px-4 py-3 text-left ${
                    Wrapper === 'button'
                      ? 'cursor-pointer transition-colors hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus:ring-1 focus:ring-amber-400/40'
                      : ''
                  }`}
                  aria-label={
                    Wrapper === 'button' ? `Open log for ${w.title || w.bead_id}` : undefined
                  }
                >
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
                    {hasLog && Wrapper === 'button' && (
                      <span className="ml-auto text-[10px] uppercase tracking-wide text-slate-500">
                        view log
                      </span>
                    )}
                  </div>
                  <p className="mt-1.5 truncate text-sm font-medium text-slate-100">
                    {w.title || w.bead_id}
                  </p>
                  <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                    <span className="font-mono text-slate-400">{w.bead_id}</span>
                    <span aria-hidden>·</span>
                    <span>{w.anvil}</span>
                    <span aria-hidden>·</span>
                    <span title={w.started_at}>{relativeTime(w.started_at)}</span>
                  </p>
                </Wrapper>
              </li>
            )
          })}
        </ul>
      )}
    </Pane>
  )
}
