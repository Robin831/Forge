import { FlaskConical } from 'lucide-react'
import { Link } from 'react-router'
import type { CrucibleStatus } from '../api'
import { relativeTime } from '../lib/format'
import Pane, { EmptyState } from './Pane'

interface CruciblesPaneProps {
  crucibles: CrucibleStatus[]
  loading: boolean
  error: string | null
  compact?: boolean
}

const PHASE_CLASSES: Record<string, string> = {
  started: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  dispatching: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  waiting: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  final_pr: 'bg-purple-500/20 text-purple-300 border-purple-500/40',
  complete: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  paused: 'bg-red-500/20 text-red-300 border-red-500/40',
}

function phaseClass(phase: string): string {
  return PHASE_CLASSES[phase] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

export default function CruciblesPane({
  crucibles,
  loading,
  error,
  compact,
}: CruciblesPaneProps) {
  return (
    <Pane
      title="Crucibles"
      icon={<FlaskConical size={16} className="text-fuchsia-400" aria-hidden />}
      count={crucibles.length}
      loading={loading}
      error={error}
    >
      {crucibles.length === 0 && !loading ? (
        <EmptyState message="No active crucibles." />
      ) : (
        <ul className="divide-y divide-slate-800">
          {crucibles.map((c) => {
            const progress = c.total_children > 0
              ? Math.round((c.completed_children / c.total_children) * 100)
              : 0
            return (
              <li key={`${c.anvil}:${c.parent_id}`} className="px-4 py-3">
                <div className="flex flex-wrap items-start gap-2">
                  <span
                    className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${phaseClass(c.phase)}`}
                  >
                    {c.phase}
                  </span>
                  <span className="text-xs text-slate-500">
                    {c.completed_children}/{c.total_children} children
                  </span>
                  <span className="ml-auto text-[11px] text-slate-500" title={c.started_at}>
                    {relativeTime(c.started_at)}
                  </span>
                </div>
                <p className="mt-1.5 truncate text-sm font-medium text-slate-100">
                  {c.parent_title || c.parent_id}
                </p>
                <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                  <Link
                    to={`/bead/${c.parent_id}?anvil=${encodeURIComponent(c.anvil)}`}
                    className="font-mono text-slate-400 hover:text-amber-300"
                  >
                    {c.parent_id}
                  </Link>
                  <span aria-hidden>·</span>
                  <span>{c.anvil}</span>
                  {c.branch && (
                    <>
                      <span aria-hidden>·</span>
                      <span className="truncate font-mono text-slate-400">{c.branch}</span>
                    </>
                  )}
                  {c.current_child && (
                    <>
                      <span aria-hidden>·</span>
                      <span>
                        current: <span className="font-mono text-slate-300">{c.current_child}</span>
                      </span>
                    </>
                  )}
                </p>
                {!compact && c.total_children > 0 && (
                  <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-slate-800">
                    <div
                      className="h-full bg-fuchsia-400/60"
                      style={{ width: `${progress}%` }}
                    />
                  </div>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </Pane>
  )
}
