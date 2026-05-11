import { useMemo, useState } from 'react'
import { Package } from 'lucide-react'
import { Link } from 'react-router-dom'
import { useApiPoll } from '../hooks/useApiPoll'
import type { Ingot, StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
import Pane, { EmptyState } from '../components/Pane'
import { relativeTime } from '../lib/format'

const POLL_INTERVAL_MS = 10000

const STATUS_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'All' },
  { value: 'init', label: 'Init' },
  { value: 'smith', label: 'Smith' },
  { value: 'temper', label: 'Temper' },
  { value: 'warden', label: 'Warden' },
  { value: 'approved', label: 'Approved' },
  { value: 'pr_open', label: 'PR Open' },
  { value: 'pr_merged', label: 'PR Merged' },
  { value: 'failed', label: 'Failed' },
  { value: 'stalled', label: 'Stalled' },
]

const STATUS_CLASSES: Record<string, string> = {
  init: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  smith: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  temper: 'bg-cyan-500/20 text-cyan-300 border-cyan-500/40',
  warden: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  approved: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  pr_open: 'bg-purple-500/20 text-purple-300 border-purple-500/40',
  pr_merged: 'bg-emerald-600/20 text-emerald-200 border-emerald-600/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
  stalled: 'bg-orange-500/20 text-orange-300 border-orange-500/40',
}

function statusClass(s: string): string {
  return STATUS_CLASSES[s] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

export default function IngotsPage() {
  const [statusFilter, setStatusFilter] = useState('')
  const [anvilFilter, setAnvilFilter] = useState('')

  const path = useMemo(() => {
    const qp = new URLSearchParams()
    if (statusFilter) qp.set('status', statusFilter)
    if (anvilFilter) qp.set('anvil', anvilFilter.trim())
    qp.set('limit', '200')
    const q = qp.toString()
    return q ? `/api/ingots?${q}` : '/api/ingots'
  }, [statusFilter, anvilFilter])

  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const ingots = useApiPoll<Ingot[]>(path, POLL_INTERVAL_MS)

  const items = ingots.data ?? []

  return (
    <div className="flex min-h-full flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <section className="flex flex-wrap items-end gap-3 rounded-xl border border-slate-800 bg-slate-900/60 p-4">
        <label className="flex flex-col gap-1 text-xs text-slate-400">
          <span>Status</span>
          <select
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
            className="min-w-[10rem] rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm text-slate-100 focus:outline-none focus:ring-1 focus:ring-amber-400/40"
          >
            {STATUS_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-col gap-1 text-xs text-slate-400">
          <span>Anvil</span>
          <input
            value={anvilFilter}
            onChange={(e) => setAnvilFilter(e.target.value)}
            placeholder="all"
            className="min-w-[10rem] rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm text-slate-100 focus:outline-none focus:ring-1 focus:ring-amber-400/40"
          />
        </label>
        <p className="ml-auto text-xs text-slate-500">
          Showing {items.length} ingot{items.length === 1 ? '' : 's'}
        </p>
      </section>

      <Pane
        title="Ingots"
        icon={<Package size={16} className="text-amber-400" aria-hidden />}
        count={items.length}
        loading={ingots.loading}
        error={ingots.error}
      >
        {items.length === 0 && !ingots.loading ? (
          <EmptyState message="No ingots match the current filters." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {items.map((ig) => (
              <li key={`${ig.anvil}:${ig.bead_id}`} className="px-4 py-3">
                <div className="flex flex-wrap items-start gap-2">
                  <span
                    className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${statusClass(ig.status)}`}
                  >
                    {ig.status}
                  </span>
                  {ig.pr_number ? (
                    <a
                      href={ig.pr_url || '#'}
                      target="_blank"
                      rel="noreferrer"
                      className="rounded-md border border-purple-500/40 bg-purple-500/10 px-2 py-0.5 text-[10px] text-purple-300 hover:bg-purple-500/20"
                    >
                      PR #{ig.pr_number}
                    </a>
                  ) : null}
                  {ig.test_results && ig.test_results.length > 0 && (
                    <span
                      className={`rounded-md border px-2 py-0.5 text-[10px] uppercase tracking-wide ${
                        ig.temper_passed
                          ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300'
                          : 'border-red-500/40 bg-red-500/10 text-red-300'
                      }`}
                    >
                      {ig.temper_passed ? 'temper pass' : `temper FAIL${ig.temper_failed_step ? ` · ${ig.temper_failed_step}` : ''}`}
                    </span>
                  )}
                  <span className="ml-auto text-[11px] text-slate-500" title={ig.updated_at}>
                    {relativeTime(ig.updated_at)}
                  </span>
                </div>
                <p className="mt-1.5 truncate text-sm font-medium text-slate-100">
                  {ig.title || ig.bead_id}
                </p>
                <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                  <Link
                    to={`/bead/${ig.bead_id}?anvil=${encodeURIComponent(ig.anvil)}`}
                    className="font-mono text-slate-400 hover:text-amber-300"
                  >
                    {ig.bead_id}
                  </Link>
                  <span aria-hidden>·</span>
                  <span>{ig.anvil}</span>
                  {ig.branch && (
                    <>
                      <span aria-hidden>·</span>
                      <span className="truncate font-mono text-slate-400">{ig.branch}</span>
                    </>
                  )}
                </p>
              </li>
            ))}
          </ul>
        )}
      </Pane>

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}
