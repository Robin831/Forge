import { useState } from 'react'
import { MonitorOff, Skull, Users } from 'lucide-react'
import { actions, type WorkerInfo } from '../api'
import { useAction } from '../hooks/useAction'
import { relativeTime } from '../lib/format'
import { isBellowsMonitor } from './PipelineBar'
import ConfirmModal from './ConfirmModal'
import Pane, { EmptyState } from './Pane'

interface WorkersPaneProps {
  workers: WorkerInfo[]
  loading: boolean
  error: string | null
  // maxTotalSmiths is the global concurrent-Smith cap reported by /api/status.
  // The pane renders (maxTotalSmiths - active workers) dimmed Idle slots so
  // the user can see remaining capacity at a glance. Zero or negative values
  // disable the placeholders entirely (e.g. when the daemon has not yet
  // reported a value).
  maxTotalSmiths?: number
  onSelectWorker?: (worker: WorkerInfo) => void
  onActionSuccess?: () => void
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

export default function WorkersPane({
  workers,
  loading,
  error,
  maxTotalSmiths = 0,
  onSelectWorker,
  onActionSuccess,
}: WorkersPaneProps) {
  const { run, busy } = useAction()
  const [killTarget, setKillTarget] = useState<WorkerInfo | null>(null)

  // The Bellows PR-monitor row is intentionally filtered out — it produces no
  // smith log, so a row with no log modal would look broken. Its state is now
  // surfaced inside the Pipeline bar's PR stage. Bellows-spawned sub-workers
  // (quench/burnish/rebase) keep their own phase and remain clickable here.
  const visibleWorkers = workers.filter((w) => !isBellowsMonitor(w))

  // Sort by started_at descending so newest workers are at the top — matches
  // hearth TUI behaviour and Hytte's WorkersCard.
  const sorted = [...visibleWorkers].sort((a, b) => {
    const aT = Date.parse(a.started_at) || 0
    const bT = Date.parse(b.started_at) || 0
    return bT - aT
  })

  // Idle slot count = (configured cap) - (active Smith-like workers). We count
  // workers that occupy a Smith slot (pending/running/reviewing) and that are
  // not bellows monitors (already filtered above). "reviewing" covers Warden
  // phase workers (state.WorkerReviewing) which also hold a slot. When the
  // daemon reports a cap of 0 we omit the placeholders entirely.
  const activeSlotWorkers = sorted.filter(
    (w) => w.status === 'pending' || w.status === 'running' || w.status === 'reviewing',
  )
  const idleCount = Math.max(0, maxTotalSmiths - activeSlotWorkers.length)

  const handleKill = async () => {
    if (!killTarget) return
    const ok = await run(() => actions.killWorker(killTarget.id), {
      successMessage: `Kill signal sent to worker ${killTarget.id.slice(0, 8)}`,
      onSuccess: onActionSuccess,
    })
    if (ok) setKillTarget(null)
  }

  return (
    <>
      <Pane
        title="Workers"
        icon={<Users size={16} className="text-sky-400" aria-hidden />}
        count={visibleWorkers.length}
        loading={loading}
        error={error}
      >
        {sorted.length === 0 && idleCount === 0 && !loading ? (
          <EmptyState message="No active workers." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {sorted.map((w) => {
              // Bellows pseudo-workers monitor a PR; they have no claude log
              // so the modal would just render an error. Render their cards
              // as static info rather than clickable buttons. The phase
              // fallback is for older API clients that don't yet send `kind`.
              const isBellows = w.kind === 'bellows' || w.phase === 'bellows'
              const hasLog = !!w.log_path && !isBellows
              const clickable = hasLog && !!onSelectWorker
              const canKill = w.status === 'pending' || w.status === 'running'
              return (
                <li key={w.id} className="flex items-stretch">
                  <div className="min-w-0 flex-1">
                    <button
                      type="button"
                      disabled={!clickable}
                      onClick={() => {
                        if (clickable) onSelectWorker?.(w)
                      }}
                      className={`block w-full px-4 py-3 text-left ${
                        clickable
                          ? 'cursor-pointer transition-colors hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus:ring-1 focus:ring-amber-400/40'
                          : 'opacity-80'
                      }`}
                      aria-label={
                        clickable ? `Open log for ${w.title || w.bead_id}` : undefined
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
                        {clickable && (
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
                    </button>
                  </div>
                  {canKill && (
                    <div className="flex items-start p-2">
                      <button
                        type="button"
                        onClick={() => setKillTarget(w)}
                        disabled={busy}
                        className="rounded-md border border-red-500/40 bg-red-500/10 p-1.5 text-red-300 hover:bg-red-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-red-400 disabled:opacity-50"
                        aria-label={`Kill worker ${w.id}`}
                        title="Kill worker"
                      >
                        <Skull size={14} />
                      </button>
                    </div>
                  )}
                </li>
              )
            })}
            {idleCount > 0 &&
              Array.from({ length: idleCount }).map((_, i) => (
                <li
                  key={`idle-${i}`}
                  data-testid="workers-idle-slot"
                  className="flex items-center gap-3 border-t border-dashed border-slate-800/80 px-4 py-3 text-slate-600"
                  aria-label={`Idle slot ${i + 1}`}
                >
                  <MonitorOff size={16} aria-hidden />
                  <span className="text-xs uppercase tracking-wide">Idle</span>
                </li>
              ))}
          </ul>
        )}
      </Pane>

      <ConfirmModal
        open={killTarget !== null}
        title="Kill worker?"
        message={
          killTarget
            ? `This will SIGTERM the Smith process for ${killTarget.bead_id} (${killTarget.anvil}). Any in-progress changes will be lost.`
            : ''
        }
        confirmLabel="Kill worker"
        tone="danger"
        busy={busy}
        onConfirm={handleKill}
        onCancel={() => setKillTarget(null)}
      />
    </>
  )
}
