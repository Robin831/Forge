import { useMemo, useState } from 'react'
import { Activity, AlertTriangle, FlaskConical, List, Users } from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import type {
  CruciblesResponse,
  QueueResponse,
  StatusResponse,
  WorkerInfo,
  WorkersResponse,
} from '../api'
import AppHeader from '../components/AppHeader'
import DispatchToggle from '../components/DispatchToggle'
import QueuePane from '../components/QueuePane'
import WorkerPanelGrid from '../components/WorkerPanelGrid'
import NeedsAttentionPane from '../components/NeedsAttentionPane'
import WedgedAnvilsBanner from '../components/WedgedAnvilsBanner'
import LiveActivity from '../components/LiveActivity'
import WorkerLogModal from '../components/WorkerLogModal'
import CruciblesPane from '../components/CruciblesPane'
import PipelineBar from '../components/PipelineBar'

const POLL_INTERVAL_MS = 5000

export default function DashboardPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const queue = useApiPoll<QueueResponse>('/api/queue', POLL_INTERVAL_MS)
  const workers = useApiPoll<WorkersResponse>('/api/workers', POLL_INTERVAL_MS)
  const crucibles = useApiPoll<CruciblesResponse>('/api/crucibles', POLL_INTERVAL_MS)
  const [logWorker, setLogWorker] = useState<WorkerInfo | null>(null)

  // Re-resolve the open modal's worker from the latest poll so its status
  // (and the pause/resume controls gated on it) stay live — e.g. after a pause
  // the row flips running → paused within one poll and the modal follows. Falls
  // back to the last-selected snapshot if the worker has aged out of the list.
  const liveLogWorker = useMemo(() => {
    if (!logWorker) return null
    return (
      (workers.data?.workers ?? []).find((w) => w.id === logWorker.id) ?? logWorker
    )
  }, [logWorker, workers.data])

  const activeWorkers = useMemo(
    () =>
      (workers.data?.workers ?? []).filter(
        (w) => w.status === 'pending' || w.status === 'running',
      ),
    [workers.data],
  )

  const queueCount = queue.data?.items?.length ?? 0
  const crucibleCount = crucibles.data?.crucibles?.length ?? 0
  const daemonHealthy = status.data?.running
  const dispatchPaused = status.data?.dispatch_paused ?? false
  const pausedSince = status.data?.paused_since

  return (
    <div className="flex min-h-full w-full flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={daemonHealthy} daemonLoading={status.loading} />

      {/* A wedged anvil blocks every dispatch into it, so it is surfaced above
          the fold rather than inside the bead-centric needs-attention pane
          (which is keyed by bead id — a wedge belongs to no bead). */}
      <WedgedAnvilsBanner anvils={status.data?.wedged_anvils ?? []} />

      <div className="flex flex-wrap items-center justify-between gap-3">
        {dispatchPaused ? (
          <p className="text-sm text-amber-200/90">
            Auto-dispatch is <strong>paused</strong>{pausedSince ? <>{' '}since {Number.isNaN(Date.parse(pausedSince)) ? pausedSince : new Date(pausedSince).toLocaleString()}</> : null}. Running
            workers continue; no new beads are dispatched. Resume to start dispatching again.
          </p>
        ) : (
          <span aria-hidden />
        )}
        <DispatchToggle paused={dispatchPaused} pausedSince={pausedSince} />
      </div>

      <section
        aria-label="Summary"
        className="grid grid-cols-2 gap-3 sm:grid-cols-5 sm:gap-4"
      >
        <StatCard
          icon={<Users size={18} className="text-sky-400" aria-hidden />}
          label="Active workers"
          value={activeWorkers.length}
        />
        <StatCard
          icon={<List size={18} className="text-cyan-400" aria-hidden />}
          label="Queued beads"
          value={queueCount}
        />
        <StatCard
          icon={<FlaskConical size={18} className="text-fuchsia-400" aria-hidden />}
          label="Crucibles"
          value={crucibleCount}
        />
        <StatCard
          icon={<Activity size={18} className="text-purple-400" aria-hidden />}
          label="Stream"
          value="live"
        />
        <StatCard
          icon={
            <AlertTriangle
              size={18}
              className={status.error ? 'text-amber-400' : 'text-slate-500'}
              aria-hidden
            />
          }
          label="API status"
          value={status.error ? 'error' : status.loading ? '…' : 'ok'}
          highlight={!!status.error}
        />
      </section>

      {crucibleCount > 0 && (
        <CruciblesPane
          crucibles={crucibles.data?.crucibles ?? []}
          loading={crucibles.loading}
          error={crucibles.error}
          compact
        />
      )}

      <PipelineBar workers={workers.data?.workers ?? []} />

      {/* Full-width multi-panel live worker grid (Forge-f0iz). One large panel
          per active worker embeds the shared CLI-style LogViewer fed by the
          per-worker SSE stream, with idle placeholder slots up to the Smith
          cap. Replaces the old full-width NeedsAttentionPane row here; the
          expand button reuses the WorkerLogModal wired below. */}
      <WorkerPanelGrid
        workers={workers.data?.workers ?? []}
        maxTotalSmiths={status.data?.max_total_smiths ?? 0}
        onExpand={setLogWorker}
      />

      <main className="grid flex-1 grid-cols-1 gap-4 lg:grid-cols-3">
        <QueuePane
          loading={queue.loading}
          error={queue.error}
          items={queue.data?.items ?? []}
        />
        {/* Bead-centric needs-attention surface (Forge-iz6s), moved into the
            main grid where the workers list used to sit. Driven by the retries
            table, so graceful and stale escalations stay findable and
            resolvable even when no live worker row exists. */}
        <NeedsAttentionPane />
        <LiveActivity />
      </main>

      <footer className="text-center text-xs text-slate-500">
        Live activity via SSE · daemon polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>

      <WorkerLogModal worker={liveLogWorker} onClose={() => setLogWorker(null)} />
    </div>
  )
}

interface StatCardProps {
  icon: React.ReactNode
  label: string
  value: number | string
  highlight?: boolean
}

function StatCard({ icon, label, value, highlight }: StatCardProps) {
  return (
    <div
      className={`flex flex-col gap-2 rounded-xl border p-4 ${
        highlight
          ? 'border-amber-500/40 bg-amber-500/5'
          : 'border-slate-800 bg-slate-900/60'
      }`}
    >
      <div className="flex items-center gap-2 text-xs text-slate-400">
        {icon}
        <span>{label}</span>
      </div>
      <div className="text-2xl font-semibold text-slate-100">{value}</div>
    </div>
  )
}
