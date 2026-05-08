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
import QueuePane from '../components/QueuePane'
import WorkersPane from '../components/WorkersPane'
import LiveActivity from '../components/LiveActivity'
import WorkerLogModal from '../components/WorkerLogModal'
import CruciblesPane from '../components/CruciblesPane'

const POLL_INTERVAL_MS = 5000

export default function DashboardPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const queue = useApiPoll<QueueResponse>('/api/queue', POLL_INTERVAL_MS)
  const workers = useApiPoll<WorkersResponse>('/api/workers', POLL_INTERVAL_MS)
  const crucibles = useApiPoll<CruciblesResponse>('/api/crucibles', POLL_INTERVAL_MS)
  const [logWorker, setLogWorker] = useState<WorkerInfo | null>(null)

  const activeWorkers = useMemo(
    () =>
      (workers.data?.workers ?? []).filter(
        (w) => w.status === 'pending' || w.status === 'running',
      ),
    [workers.data],
  )

  const queueCount = queue.data?.items.length ?? 0
  const crucibleCount = crucibles.data?.crucibles.length ?? 0
  const daemonHealthy = status.data?.running

  return (
    <div className="mx-auto flex min-h-full max-w-7xl flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={daemonHealthy} daemonLoading={status.loading} />

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

      <main className="grid flex-1 grid-cols-1 gap-4 lg:grid-cols-3">
        <QueuePane
          loading={queue.loading}
          error={queue.error}
          items={queue.data?.items ?? []}
        />
        <WorkersPane
          loading={workers.loading}
          error={workers.error}
          workers={workers.data?.workers ?? []}
          onSelectWorker={setLogWorker}
        />
        <LiveActivity />
      </main>

      <footer className="text-center text-xs text-slate-500">
        Live activity via SSE · daemon polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>

      <WorkerLogModal worker={logWorker} onClose={() => setLogWorker(null)} />
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
