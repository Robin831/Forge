import { useMemo, useState } from 'react'
import {
  Activity,
  AlertTriangle,
  Circle,
  Hammer,
  List,
  LogOut,
  Users,
} from 'lucide-react'
import { useAuth } from '../auth'
import { useApiPoll } from '../hooks/useApiPoll'
import type {
  QueueResponse,
  StatusResponse,
  WorkerInfo,
  WorkersResponse,
} from '../api'
import QueuePane from '../components/QueuePane'
import WorkersPane from '../components/WorkersPane'
import LiveActivity from '../components/LiveActivity'
import WorkerLogModal from '../components/WorkerLogModal'

const POLL_INTERVAL_MS = 5000

export default function DashboardPage() {
  const { user, logout } = useAuth()
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const queue = useApiPoll<QueueResponse>('/api/queue', POLL_INTERVAL_MS)
  const workers = useApiPoll<WorkersResponse>('/api/workers', POLL_INTERVAL_MS)
  const [logWorker, setLogWorker] = useState<WorkerInfo | null>(null)

  const activeWorkers = useMemo(
    () =>
      (workers.data?.workers ?? []).filter(
        (w) => w.status === 'pending' || w.status === 'running',
      ),
    [workers.data],
  )

  const queueCount = queue.data?.items.length ?? 0
  const daemonHealthy = status.data?.running

  return (
    <div className="mx-auto flex min-h-full max-w-7xl flex-col gap-6 p-4 sm:p-6">
      <header className="flex flex-wrap items-center gap-3">
        <Hammer size={24} className="text-amber-400" aria-hidden />
        <div>
          <h1 className="text-xl font-semibold text-slate-100 sm:text-2xl">Hearth</h1>
          <p className="text-xs text-slate-400">Forge orchestrator dashboard</p>
        </div>

        <span
          className={`ml-auto inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs ${
            daemonHealthy
              ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300'
              : status.loading
                ? 'border-slate-700 bg-slate-800/60 text-slate-400'
                : 'border-red-500/40 bg-red-500/10 text-red-300'
          }`}
          aria-live="polite"
        >
          <Circle size={8} fill="currentColor" />
          {status.loading
            ? 'connecting…'
            : daemonHealthy
              ? 'daemon online'
              : 'daemon offline'}
        </span>

        {user && (
          <span className="hidden items-center text-sm text-slate-400 sm:inline-flex">
            {user}
          </span>
        )}

        <button
          type="button"
          onClick={() => void logout()}
          className="inline-flex items-center gap-1.5 rounded-lg border border-slate-700 bg-slate-800 px-3 py-1.5 text-sm font-medium text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700"
        >
          <LogOut size={14} aria-hidden />
          <span>Sign out</span>
        </button>
      </header>

      <section
        aria-label="Summary"
        className="grid grid-cols-2 gap-3 sm:grid-cols-4 sm:gap-4"
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
