import { useMemo } from 'react'
import { MonitorOff, Users } from 'lucide-react'
import type { WorkerInfo } from '../api'
import { holdsDispatchSlot, isFinishedWorker, isSlotStatus } from '../lib/workerStatus'
import { isBellowsMonitor } from './PipelineBar'
import WorkerPanel from './WorkerPanel'

interface WorkerPanelGridProps {
  workers: WorkerInfo[]
  // maxTotalSmiths is the global concurrent-Smith cap reported by /api/status.
  // Idle placeholder slots fill the remaining capacity (cap − active workers)
  // so the operator sees how many more Smiths could run. Zero disables the
  // placeholders (e.g. before the daemon reports a value).
  maxTotalSmiths?: number
  onExpand?: (worker: WorkerInfo) => void
  onKilled?: () => void
}

// isSlotWorker reports whether a worker streams output worth a full panel: any
// status in SLOT_STATUSES (lib/workerStatus, shared with WorkersPane and
// WorkerPanel so the three cannot disagree about the list — they used to, and
// all three omitted 'stalled'). Bellows PR-monitor pseudo-workers are excluded
// here: they produce no claude log.
//
// This is deliberately WIDER than holdsDispatchSlot: a lifecycle fix worker
// (quench/burnish/rebase/assay) runs a real claude session with a real log, so
// its transcript belongs on the wall — but the daemon excludes its phase from
// dispatch capacity, so it must not consume an idle slot. The two questions
// are answered separately below.
export function isSlotWorker(w: WorkerInfo): boolean {
  return isSlotStatus(w.status) && !isBellowsMonitor(w)
}

// WorkerPanelGrid is the full-width dashboard section that renders one large
// live panel per active worker, plus dimmed idle placeholders up to the
// configured Smith cap. It replaces the compact worker list + one-modal-at-a-
// time flow with a wall of concurrent live transcripts. Panels are keyed by
// worker id so a finished worker unmounts cleanly (closing its SSE) on the next
// poll rather than lingering.
export default function WorkerPanelGrid({
  workers,
  maxTotalSmiths = 0,
  onExpand,
  onKilled,
}: WorkerPanelGridProps) {
  const active = useMemo(() => workers.filter(isSlotWorker), [workers])
  // Recently-finished workers (the daemon includes them for the linger window
  // requested via ?recent=) render as frozen panels after the live ones, so a
  // completed transcript stays readable for a few minutes instead of vanishing
  // on the next poll. They never count toward the idle-slot math.
  const finished = useMemo(
    () =>
      workers
        .filter((w) => isFinishedWorker(w) && !isBellowsMonitor(w))
        .sort((a, b) => (b.completed_at ?? '').localeCompare(a.completed_at ?? '')),
    [workers],
  )
  // Idle placeholders fill the capacity the daemon would actually dispatch
  // into, so the count is taken over holdsDispatchSlot — both axes of
  // state.ActiveDispatchWorkers — rather than over the panels on screen. A
  // running quench worker gets a panel and takes no slot.
  const slotHolders = useMemo(() => active.filter(holdsDispatchSlot), [active])
  const idleCount = Math.max(0, maxTotalSmiths - slotHolders.length)

  return (
    <section aria-label="Live workers" className="flex flex-col gap-3">
      <div className="flex items-center gap-2 text-sm font-semibold text-slate-200">
        <Users size={16} className="text-sky-400" aria-hidden />
        <span>Live workers</span>
        <span className="rounded-full bg-slate-800 px-2 py-0.5 text-xs font-normal text-slate-300">
          {active.length}
        </span>
      </div>

      {active.length === 0 && finished.length === 0 && idleCount === 0 ? (
        <div
          data-testid="worker-panel-grid-empty"
          className="rounded-xl border border-dashed border-slate-800 bg-slate-900/40 px-4 py-10 text-center text-sm text-slate-500"
        >
          No active workers. Panels appear here as beads are dispatched.
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2 2xl:grid-cols-3">
          {active.map((worker) => (
            <WorkerPanel
              key={worker.id}
              worker={worker}
              onExpand={onExpand}
              onKilled={onKilled}
            />
          ))}
          {finished.map((worker) => (
            <WorkerPanel
              key={worker.id}
              worker={worker}
              onExpand={onExpand}
              onKilled={onKilled}
            />
          ))}
          {Array.from({ length: idleCount }).map((_, i) => (
            <div
              key={`idle-${i}`}
              data-testid="worker-panel-idle-slot"
              className="flex min-h-[8rem] flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-slate-800 bg-slate-900/30 text-slate-600"
              aria-label={`Idle slot ${i + 1}`}
            >
              <MonitorOff size={20} aria-hidden />
              <span className="text-xs uppercase tracking-wide">idle</span>
            </div>
          ))}
        </div>
      )}
    </section>
  )
}
