import { useCallback, useEffect, useState } from 'react'
import {
  AlertTriangle,
  CheckCircle2,
  FileText,
  Loader2,
  RefreshCw,
  ScanSearch,
  X,
} from 'lucide-react'
import {
  assay,
  type AssayFinding,
  type AssayRun,
  type FindingSeverity,
  type FindingStatus,
  type PRFindingsResponse,
  type PRItem,
} from '../api'
import { useAction } from '../hooks/useAction'
import { relativeTime } from '../lib/format'

interface PRFindingsPanelProps {
  pr: PRItem
  onClose?: () => void
}

// SEVERITY_CLASSES maps a finding severity to its badge styling. Important
// findings are actionable (amber), pre-existing issues are informational
// (slate), and nits are low-priority (sky). Unknown values fall back to the
// neutral slate badge.
const SEVERITY_CLASSES: Record<string, string> = {
  Important: 'border-amber-500/40 bg-amber-500/15 text-amber-200',
  PreExisting: 'border-slate-600/50 bg-slate-700/40 text-slate-300',
  Nit: 'border-sky-500/40 bg-sky-500/15 text-sky-200',
}

function severityClass(severity: FindingSeverity): string {
  return SEVERITY_CLASSES[severity] ?? 'border-slate-600/50 bg-slate-700/40 text-slate-300'
}

// STATUS_CLASSES maps a finding's lifecycle status to a small badge. Resolved
// findings are de-emphasised (emerald check), posted findings are live on the
// PR (indigo), and open findings are detected but not yet posted (slate).
const STATUS_CLASSES: Record<string, string> = {
  open: 'border-slate-600/50 bg-slate-700/40 text-slate-300',
  posted: 'border-indigo-500/40 bg-indigo-500/15 text-indigo-200',
  resolved: 'border-emerald-500/40 bg-emerald-500/15 text-emerald-200',
}

function statusClass(status: FindingStatus): string {
  return STATUS_CLASSES[status] ?? 'border-slate-600/50 bg-slate-700/40 text-slate-300'
}

// RUN_STATUS_CLASSES styles the run pill: running spins, complete is green,
// skipped is muted, error is red, and partial gets its own amber chip — a run
// that half-reviewed the head is neither a pass nor a failure, and colouring it
// as either would misstate the coverage the findings below actually have.
const RUN_STATUS_CLASSES: Record<string, string> = {
  running: 'border-sky-500/40 bg-sky-500/15 text-sky-200',
  complete: 'border-emerald-500/40 bg-emerald-500/15 text-emerald-200',
  skipped: 'border-slate-600/50 bg-slate-700/40 text-slate-300',
  partial: 'border-amber-500/40 bg-amber-500/15 text-amber-200',
  error: 'border-red-500/40 bg-red-500/15 text-red-200',
}

// partialCoverageText renders the caveat shown beside a partial run pill. It
// prefers the server's status_text so the panel, the worker row and the PR
// comment all name the same passes; the client-side fallback only fires for a
// run whose text the server did not render.
function partialCoverageText(run: AssayRun): string {
  if (run.status_text) return run.status_text
  const failed = (run.failed_passes ?? [])
    .map((f) => (f.reason ? `${f.name} — ${f.reason}` : f.name))
    .join(', ')
  const tally = `${run.completed_passes ?? 0} of ${run.total_passes ?? 0} passes completed`
  return failed ? `partial: ${tally} (failed: ${failed})` : `partial: ${tally}`
}

function runStatusClass(status: string): string {
  return RUN_STATUS_CLASSES[status] ?? 'border-slate-600/50 bg-slate-700/40 text-slate-300'
}

function RunSummary({ run }: { run: AssayRun }) {
  const isRunning = run.status === 'running'
  const isPartial = run.status === 'partial'
  return (
    <div className="flex flex-wrap items-center gap-2 text-xs text-slate-400">
      <span
        className={`inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-semibold uppercase tracking-wide ${runStatusClass(run.status)}`}
      >
        {isRunning ? (
          <Loader2 size={11} className="animate-spin" aria-hidden />
        ) : run.status === 'complete' ? (
          <CheckCircle2 size={11} aria-hidden />
        ) : run.status === 'error' || isPartial ? (
          <AlertTriangle size={11} aria-hidden />
        ) : null}
        {run.status}
      </span>
      {isPartial && (
        <span className="text-amber-200">{partialCoverageText(run)}</span>
      )}
      <span>
        {run.findings_count} finding{run.findings_count === 1 ? '' : 's'}
        {run.posted_count > 0 ? ` · ${run.posted_count} posted` : ''}
      </span>
      {run.shadow_mode && (
        <span className="rounded-md border border-slate-600/50 bg-slate-700/40 px-1.5 py-0.5 text-[10px] uppercase tracking-wide text-slate-400">
          shadow
        </span>
      )}
      {run.started_at && (
        <span title={run.started_at}>{relativeTime(run.started_at)}</span>
      )}
      {run.skipped_reason && (
        <span className="text-slate-500">skipped: {run.skipped_reason}</span>
      )}
      {run.error && <span className="text-red-300">{run.error}</span>}
    </div>
  )
}

function FindingRow({ finding }: { finding: AssayFinding }) {
  return (
    <li className="flex flex-col gap-1 px-4 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <span
          className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${severityClass(finding.severity)}`}
        >
          {finding.severity}
        </span>
        <span
          className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] uppercase tracking-wide ${statusClass(finding.status)}`}
        >
          {finding.status}
        </span>
        {finding.category && (
          <span className="rounded-md border border-slate-700 bg-slate-800/60 px-2 py-0.5 text-[10px] uppercase tracking-wide text-slate-400">
            {finding.category}
          </span>
        )}
      </div>
      <p className="text-sm text-slate-100">{finding.message || '(no message)'}</p>
      {finding.file && (
        <p className="flex items-center gap-1 font-mono text-xs text-slate-500">
          <FileText size={11} aria-hidden />
          {finding.file}
          {finding.anchor ? `:${finding.anchor}` : ''}
        </p>
      )}
      {finding.body && (
        <p className="whitespace-pre-wrap text-xs text-slate-400">{finding.body}</p>
      )}
    </li>
  )
}

// PRFindingsPanel renders the Assay findings for a single PR with a Re-run
// button. It loads an initial snapshot via assay.getFindings, then subscribes
// to the findings SSE channel so live updates (a rerun's progress, newly
// posted findings, resolved threads) arrive without a manual refresh. The
// snapshot is fully replaced on each SSE event — the backend re-emits the
// whole payload on change.
export default function PRFindingsPanel({ pr, onClose }: PRFindingsPanelProps) {
  const prID = pr.id
  const [snapshot, setSnapshot] = useState<PRFindingsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [streaming, setStreaming] = useState(false)
  const { run, busy } = useAction()

  const load = useCallback(
    (signal?: AbortSignal) => {
      if (prID === undefined) return
      setLoading(true)
      assay
        .getFindings(prID, signal)
        .then((data) => {
          setSnapshot(data)
          setError(null)
        })
        .catch((err: unknown) => {
          if (signal?.aborted) return
          setError(err instanceof Error ? err.message : 'failed to load findings')
        })
        .finally(() => {
          if (!signal?.aborted) setLoading(false)
        })
    },
    [prID],
  )

  // Initial fetch. Aborted on unmount / PR change so a slow response can't
  // write into a panel that has since been replaced.
  useEffect(() => {
    if (prID === undefined) {
      setLoading(false)
      return
    }
    const controller = new AbortController()
    load(controller.signal)
    return () => controller.abort()
  }, [prID, load])

  // Live updates via the findings SSE channel. Replaces the snapshot wholesale
  // on each event. No-ops when EventSource is unavailable (the initial fetch
  // above still populates the panel). The state setters are stable, so the
  // subscription is re-established only when the PR changes.
  useEffect(() => {
    if (prID === undefined) return
    const sub = assay.subscribeFindings(prID, {
      onSnapshot: (s) => {
        setSnapshot(s)
        setError(null)
        setLoading(false)
      },
      onOpen: () => setStreaming(true),
      onError: () => setStreaming(false),
    })
    return () => {
      sub.close()
      setStreaming(false)
    }
  }, [prID])

  const handleRerun = () => {
    if (prID === undefined || pr.anvil === '') return
    void run(() => assay.rerunAssay({ anvil: pr.anvil, pr: prID }), {
      successMessage: `Assay re-run requested for #${pr.number}`,
      // Refresh immediately so the run pill flips to "running" even if the SSE
      // channel is unavailable (e.g. behind a proxy that buffers events).
      onSuccess: () => load(),
    })
  }

  const findings = snapshot?.findings ?? []
  const rerunDisabled = prID === undefined || busy

  return (
    <section
      aria-label={`Assay findings for PR #${pr.number}`}
      className="flex flex-col gap-3 rounded-xl border border-slate-700/60 bg-slate-900/60 p-4 text-slate-100"
    >
      <header className="flex items-start gap-2">
        <ScanSearch size={16} className="mt-0.5 shrink-0 text-cyan-400" aria-hidden />
        <div className="flex-1">
          <h3 className="text-sm font-semibold text-slate-100">
            Assay findings
            {streaming && (
              <span className="ml-2 align-middle text-[10px] font-normal uppercase tracking-wide text-emerald-400">
                live
              </span>
            )}
          </h3>
          {snapshot?.run ? (
            <div className="mt-1">
              <RunSummary run={snapshot.run} />
            </div>
          ) : (
            !loading && (
              <p className="mt-1 text-xs text-slate-500">No Assay run recorded yet.</p>
            )
          )}
        </div>
        <button
          type="button"
          onClick={handleRerun}
          disabled={rerunDisabled}
          className="inline-flex items-center gap-1 rounded-md border border-cyan-600/30 bg-cyan-600/15 px-2.5 py-1 text-xs font-medium text-cyan-200 transition-colors hover:bg-cyan-600/25 focus:outline-none focus-visible:ring-1 focus-visible:ring-cyan-300 disabled:cursor-not-allowed disabled:opacity-50"
        >
          <RefreshCw size={12} aria-hidden className={busy ? 'animate-spin' : undefined} />
          {busy ? 'Re-running…' : 'Re-run'}
        </button>
        {onClose && (
          <button
            type="button"
            onClick={onClose}
            aria-label="Close findings panel"
            className="rounded-md border border-slate-700 bg-slate-800 p-1.5 text-slate-300 hover:bg-slate-700 focus:outline-none focus-visible:ring-1 focus-visible:ring-cyan-300"
          >
            <X size={14} aria-hidden />
          </button>
        )}
      </header>

      {error && (
        <div
          role="alert"
          className="rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200"
        >
          Failed to load findings: {error}
        </div>
      )}

      {loading && !snapshot ? (
        <p className="px-1 py-2 text-sm text-slate-500" role="status" aria-live="polite">
          Loading findings…
        </p>
      ) : findings.length === 0 ? (
        <p className="px-1 py-2 text-sm text-slate-500">
          No findings for this PR.
        </p>
      ) : (
        <ul
          className="divide-y divide-slate-800 overflow-hidden rounded-lg border border-slate-800"
          data-testid="assay-findings-list"
        >
          {findings.map((f) => (
            <FindingRow key={f.id} finding={f} />
          ))}
        </ul>
      )}
    </section>
  )
}
