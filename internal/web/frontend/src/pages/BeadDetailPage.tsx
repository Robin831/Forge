import { useMemo, useState } from 'react'
import { ArrowLeft, FileText, Plus, StickyNote, X, XCircle } from 'lucide-react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { useApiPoll } from '../hooks/useApiPoll'
import {
  actions,
  type BeadDetailResponse,
  type BeadDetailWorker,
  type StatusResponse,
  type WorkerInfo,
} from '../api'
import AppHeader from '../components/AppHeader'
import ConfirmModal from '../components/ConfirmModal'
import Pane, { EmptyState } from '../components/Pane'
import WorkerLogModal from '../components/WorkerLogModal'
import { useAction } from '../hooks/useAction'
import { eventClasses, priorityClasses, priorityLabel, relativeTime } from '../lib/format'

const POLL_INTERVAL_MS = 5000

const STATUS_BADGE: Record<string, string> = {
  done: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
  timeout: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  running: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  pending: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
}

function badgeClass(s: string): string {
  return STATUS_BADGE[s] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

function workerToInfo(beadID: string, anvil: string, w: BeadDetailWorker): WorkerInfo {
  return {
    id: w.id,
    bead_id: beadID,
    anvil,
    branch: w.branch,
    status: w.status,
    phase: w.phase,
    started_at: w.started_at,
    completed_at: w.completed_at,
    log_path: w.log_path,
    pr_number: w.pr_number,
  }
}

export default function BeadDetailPage() {
  const { bead_id: rawBeadID } = useParams<{ bead_id: string }>()
  const [searchParams] = useSearchParams()
  const anvil = searchParams.get('anvil') ?? ''
  const [logWorker, setLogWorker] = useState<WorkerInfo | null>(null)

  const beadID = rawBeadID ?? ''
  const path = useMemo(() => {
    const qp = new URLSearchParams()
    if (anvil) qp.set('anvil', anvil)
    const q = qp.toString()
    return `/api/bead/${encodeURIComponent(beadID)}${q ? `?${q}` : ''}`
  }, [beadID, anvil])

  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const detail = useApiPoll<BeadDetailResponse>(path, POLL_INTERVAL_MS)
  const data = detail.data
  const resolvedAnvil = data?.anvil || anvil
  const { run, busy } = useAction()
  const [dialog, setDialog] = useState<
    | null
    | { kind: 'close' }
    | { kind: 'add-label' }
    | { kind: 'remove-label'; label: string }
    | { kind: 'note' }
  >(null)

  const closeDialog = () => setDialog(null)

  const handleConfirm = async (input: string) => {
    if (!dialog || !resolvedAnvil) {
      closeDialog()
      return
    }
    if (dialog.kind === 'close') {
      const ok = await run(() => actions.closeBead(beadID, resolvedAnvil), {
        successMessage: `Closed ${beadID}`,
      })
      if (ok) closeDialog()
    } else if (dialog.kind === 'add-label') {
      if (!input.trim()) return
      const ok = await run(
        () => actions.addLabel(beadID, resolvedAnvil, input.trim()),
        { successMessage: `Added label "${input.trim()}"` },
      )
      if (ok) closeDialog()
    } else if (dialog.kind === 'remove-label') {
      const ok = await run(
        () => actions.removeLabel(beadID, resolvedAnvil, dialog.label),
        { successMessage: `Removed label "${dialog.label}"` },
      )
      if (ok) closeDialog()
    } else if (dialog.kind === 'note') {
      if (!input.trim()) return
      const ok = await run(
        () => actions.addNote(beadID, resolvedAnvil, input.trim()),
        { successMessage: 'Note appended' },
      )
      if (ok) closeDialog()
    }
  }

  return (
    <div className="mx-auto flex min-h-full max-w-7xl flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <Link
        to="/"
        className="inline-flex w-fit items-center gap-1.5 text-xs text-slate-400 hover:text-amber-300"
      >
        <ArrowLeft size={12} aria-hidden />
        Back to dashboard
      </Link>

      <header className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
        <div className="flex flex-wrap items-baseline gap-2">
          <h2 className="text-lg font-semibold text-slate-100">
            {data?.queue?.title || data?.ingot?.title || beadID}
          </h2>
          {data?.queue && (
            <span
              className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${priorityClasses(data.queue.priority)}`}
            >
              {priorityLabel(data.queue.priority)}
            </span>
          )}
        </div>
        <p className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
          <span className="font-mono text-slate-400">{beadID}</span>
          {(data?.anvil || anvil) && (
            <>
              <span aria-hidden>·</span>
              <span>{data?.anvil || anvil}</span>
            </>
          )}
          {data?.queue?.section && (
            <>
              <span aria-hidden>·</span>
              <span className="capitalize">{data.queue.section}</span>
            </>
          )}
          {data?.queue?.status && (
            <>
              <span aria-hidden>·</span>
              <span>{data.queue.status}</span>
            </>
          )}
        </p>
        {data?.queue?.description && (
          <p className="mt-3 whitespace-pre-wrap text-sm text-slate-300">{data.queue.description}</p>
        )}
        <div className="mt-3 flex flex-wrap items-center gap-1">
          {(data?.queue?.labels ?? []).map((l) => (
            <span
              key={l}
              className="inline-flex items-center gap-1 rounded-full bg-slate-800 px-2 py-0.5 text-[10px] text-slate-300"
            >
              {l}
              {resolvedAnvil && (
                <button
                  type="button"
                  onClick={() => setDialog({ kind: 'remove-label', label: l })}
                  disabled={busy}
                  className="text-slate-500 hover:text-red-300 focus:outline-none focus-visible:ring-2 focus-visible:ring-red-400 disabled:opacity-50"
                  aria-label={`Remove label ${l}`}
                >
                  <X size={10} />
                </button>
              )}
            </span>
          ))}
          {resolvedAnvil && (
            <button
              type="button"
              onClick={() => setDialog({ kind: 'add-label' })}
              disabled={busy}
              className="inline-flex items-center gap-1 rounded-full border border-dashed border-slate-700 px-2 py-0.5 text-[10px] text-slate-400 hover:border-amber-400/40 hover:text-amber-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:opacity-50"
            >
              <Plus size={10} aria-hidden /> add label
            </button>
          )}
        </div>
        {resolvedAnvil && (
          <div className="mt-4 flex flex-wrap gap-2">
            <button
              type="button"
              onClick={() => setDialog({ kind: 'note' })}
              disabled={busy}
              className="inline-flex items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800 px-3 py-1.5 text-xs text-slate-200 hover:border-amber-400/40 hover:text-amber-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:opacity-50"
            >
              <StickyNote size={12} aria-hidden /> Append note
            </button>
            <button
              type="button"
              onClick={() => setDialog({ kind: 'close' })}
              disabled={busy}
              className="inline-flex items-center gap-1.5 rounded-md border border-red-500/40 bg-red-500/10 px-3 py-1.5 text-xs text-red-200 hover:bg-red-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-red-400 disabled:opacity-50"
            >
              <XCircle size={12} aria-hidden /> Close bead
            </button>
          </div>
        )}
        {detail.error && (
          <p className="mt-3 rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200">
            {detail.error}
          </p>
        )}
      </header>

      {data?.retry && (
        <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
          <h3 className="text-sm font-semibold text-slate-200">Retry & attention state</h3>
          <dl className="mt-2 grid grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-4">
            <DetailItem label="Retry count" value={String(data.retry.retry_count)} />
            <DetailItem
              label="Dispatch failures"
              value={String(data.retry.dispatch_failures)}
              warn={data.retry.dispatch_failures > 0}
            />
            <DetailItem
              label="Recovery failures"
              value={String(data.retry.recovery_failures)}
              warn={data.retry.recovery_failures > 0}
            />
            <DetailItem
              label="Needs human"
              value={data.retry.needs_human ? 'yes' : 'no'}
              warn={data.retry.needs_human}
            />
            <DetailItem
              label="Clarification needed"
              value={data.retry.clarification_needed ? 'yes' : 'no'}
              warn={data.retry.clarification_needed}
            />
            {data.retry.next_retry && (
              <DetailItem label="Next retry" value={relativeTime(data.retry.next_retry)} />
            )}
          </dl>
          {data.retry.last_error && (
            <pre className="mt-3 max-h-48 overflow-auto rounded-md border border-red-700/40 bg-red-900/10 px-3 py-2 text-xs text-red-200">
              {data.retry.last_error}
            </pre>
          )}
        </section>
      )}

      {data?.cost && (
        <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
          <h3 className="text-sm font-semibold text-slate-200">Bead cost</h3>
          <dl className="mt-2 grid grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-5">
            <DetailItem
              label="Estimated USD"
              value={`$${data.cost.estimated_cost_usd.toFixed(4)}`}
            />
            <DetailItem label="Input tokens" value={data.cost.input_tokens.toLocaleString()} />
            <DetailItem label="Output tokens" value={data.cost.output_tokens.toLocaleString()} />
            <DetailItem label="Cache read" value={data.cost.cache_read.toLocaleString()} />
            <DetailItem label="Cache write" value={data.cost.cache_write.toLocaleString()} />
          </dl>
        </section>
      )}

      {data?.ingot && (
        <Pane
          title="Ingot"
          icon={<FileText size={16} className="text-amber-400" aria-hidden />}
          count={data.ingot.test_results?.length ?? 0}
          loading={false}
          error={null}
        >
          <div className="px-4 py-3">
            <div className="flex flex-wrap items-baseline gap-3 text-xs text-slate-400">
              <span>
                Status:{' '}
                <span className="font-mono text-slate-200">{data.ingot.status}</span>
              </span>
              {data.ingot.branch && (
                <span>
                  Branch:{' '}
                  <span className="font-mono text-slate-200">{data.ingot.branch}</span>
                </span>
              )}
              {data.ingot.pr_number && (
                <a
                  href={data.ingot.pr_url || '#'}
                  target="_blank"
                  rel="noreferrer"
                  className="text-purple-300 hover:text-purple-200"
                >
                  PR #{data.ingot.pr_number}
                </a>
              )}
              <span className="ml-auto" title={data.ingot.updated_at}>
                updated {relativeTime(data.ingot.updated_at)}
              </span>
            </div>
            {data.ingot.test_results && data.ingot.test_results.length > 0 && (
              <table className="mt-3 w-full text-sm">
                <thead className="text-left text-xs uppercase tracking-wide text-slate-500">
                  <tr>
                    <th className="py-1.5">Step</th>
                    <th className="py-1.5">Verdict</th>
                    <th className="py-1.5">Duration</th>
                    <th className="py-1.5">Exit</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-800">
                  {data.ingot.test_results.map((tr) => (
                    <tr key={tr.id}>
                      <td className="py-1.5 font-mono text-slate-300">
                        {tr.step_name}
                        {tr.optional && (
                          <span className="ml-1 text-[10px] text-slate-500">(optional)</span>
                        )}
                      </td>
                      <td className="py-1.5">
                        <span
                          className={`rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${
                            tr.passed
                              ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300'
                              : 'border-red-500/40 bg-red-500/10 text-red-300'
                          }`}
                        >
                          {tr.passed ? 'pass' : 'FAIL'}
                        </span>
                      </td>
                      <td className="py-1.5 text-slate-400">
                        {(tr.duration_ms / 1000).toFixed(1)}s
                      </td>
                      <td className="py-1.5 text-slate-400">{tr.exit_code}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </Pane>
      )}

      {data && data.prs.length > 0 && (
        <Pane
          title="Pull requests"
          icon={<FileText size={16} className="text-purple-400" aria-hidden />}
          count={data.prs.length}
          loading={false}
          error={null}
        >
          <ul className="divide-y divide-slate-800">
            {data.prs.map((pr) => (
              <li key={pr.id} className="px-4 py-3">
                <div className="flex flex-wrap items-baseline gap-2">
                  <span className="text-sm font-semibold text-slate-100">
                    PR #{pr.number}
                  </span>
                  <span
                    className={`rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${badgeClass(pr.status)}`}
                  >
                    {pr.status}
                  </span>
                  <span className="ml-auto text-[11px] text-slate-500" title={pr.last_checked || pr.created_at}>
                    {relativeTime(pr.last_checked || pr.created_at)}
                  </span>
                </div>
                {pr.title && (
                  <p className="mt-1 truncate text-sm text-slate-200">{pr.title}</p>
                )}
                <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                  <span>{pr.anvil}</span>
                  {pr.branch && (
                    <>
                      <span aria-hidden>·</span>
                      <span className="font-mono">{pr.branch}</span>
                    </>
                  )}
                  {pr.base_branch && (
                    <>
                      <span aria-hidden>→</span>
                      <span className="font-mono">{pr.base_branch}</span>
                    </>
                  )}
                </p>
              </li>
            ))}
          </ul>
        </Pane>
      )}

      <Pane
        title="Workers for this bead"
        icon={<FileText size={16} className="text-sky-400" aria-hidden />}
        count={data?.workers.length ?? 0}
        loading={detail.loading && !data}
        error={null}
      >
        {(data?.workers.length ?? 0) === 0 ? (
          <EmptyState message="No worker history for this bead yet." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {(data?.workers ?? []).map((w) => {
              const hasLog = !!w.log_path
              return (
                <li key={w.id}>
                  <button
                    type="button"
                    onClick={() => {
                      if (hasLog && data) {
                        setLogWorker(workerToInfo(beadID, data.anvil ?? anvil, w))
                      }
                    }}
                    disabled={!hasLog}
                    className={`block w-full px-4 py-3 text-left ${
                      hasLog
                        ? 'cursor-pointer transition-colors hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus:ring-1 focus:ring-amber-400/40'
                        : 'opacity-80'
                    }`}
                  >
                    <div className="flex flex-wrap items-baseline gap-2">
                      <span
                        className={`rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${badgeClass(w.status)}`}
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
                      {hasLog && (
                        <span className="ml-auto text-[10px] uppercase tracking-wide text-slate-500">
                          view log
                        </span>
                      )}
                    </div>
                    <p className="mt-1 text-xs text-slate-500">
                      <span className="font-mono text-slate-400">{w.id}</span>
                      {w.branch && (
                        <>
                          <span aria-hidden> · </span>
                          <span className="font-mono">{w.branch}</span>
                        </>
                      )}
                      <span aria-hidden> · </span>
                      <span title={w.completed_at || w.started_at}>
                        {relativeTime(w.completed_at || w.started_at)}
                      </span>
                    </p>
                  </button>
                </li>
              )
            })}
          </ul>
        )}
      </Pane>

      <Pane
        title="Events for this bead"
        icon={<FileText size={16} className="text-emerald-400" aria-hidden />}
        count={data?.events.length ?? 0}
        loading={detail.loading && !data}
        error={null}
      >
        {(data?.events.length ?? 0) === 0 ? (
          <EmptyState message="No events recorded for this bead." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {(data?.events ?? []).map((e) => (
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
              </li>
            ))}
          </ul>
        )}
      </Pane>

      <WorkerLogModal worker={logWorker} onClose={() => setLogWorker(null)} />

      <ConfirmModal
        open={dialog?.kind === 'close'}
        title="Close bead?"
        message={`This calls bd close on ${beadID} (${resolvedAnvil}). Use this only when the work is genuinely finished.`}
        confirmLabel="Close bead"
        tone="danger"
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeDialog}
      />
      <ConfirmModal
        open={dialog?.kind === 'add-label'}
        title="Add label"
        message={`Add a label to ${beadID}.`}
        confirmLabel="Add label"
        tone="primary"
        inputLabel="Label"
        inputPlaceholder="forgeReady"
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeDialog}
      />
      <ConfirmModal
        open={dialog?.kind === 'remove-label'}
        title="Remove label?"
        message={
          dialog?.kind === 'remove-label'
            ? `Remove label "${dialog.label}" from ${beadID}.`
            : ''
        }
        confirmLabel="Remove"
        tone="danger"
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeDialog}
      />
      <ConfirmModal
        open={dialog?.kind === 'note'}
        title="Append note"
        message={`Add a triage note to ${beadID}.`}
        confirmLabel="Append"
        tone="primary"
        inputLabel="Note"
        inputPlaceholder="Manual triage step…"
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeDialog}
      />

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}

interface DetailItemProps {
  label: string
  value: string
  warn?: boolean
}

function DetailItem({ label, value, warn }: DetailItemProps) {
  return (
    <div>
      <dt className="text-slate-500">{label}</dt>
      <dd className={`font-mono ${warn ? 'text-amber-300' : 'text-slate-200'}`}>{value}</dd>
    </div>
  )
}
