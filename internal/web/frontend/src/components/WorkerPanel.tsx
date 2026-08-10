import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router'
import {
  ChevronDown,
  ChevronRight,
  Maximize2,
  Pause,
  Play,
  Skull,
  Terminal,
} from 'lucide-react'
import {
  actions,
  apiGet,
  isFinishedWorker,
  steerDisabledReason,
  steerIsResumeDelivery,
  type LogLine,
  type LogTailResponse,
  type WorkerInfo,
} from '../api'
import { useAction } from '../hooks/useAction'
import { useEventSource } from '../hooks/useEventSource'
import { useUIState } from '../hooks/useUIState'
import { relativeTime } from '../lib/format'
import { parseTranscript, type TranscriptEntry } from '../lib/logParse'
import ConfirmModal from './ConfirmModal'
import LogViewer from './LogViewer'
import PreviewButton from './PreviewButton'
import SteerComposer from './SteerComposer'

interface WorkerPanelProps {
  worker: WorkerInfo
  // maxItems caps the live SSE buffer fed into the LogViewer. Matches the
  // WorkerLogModal default (1000) so the panel and the expanded modal render
  // the same window of the transcript.
  maxItems?: number
  // onExpand opens the full-screen WorkerLogModal for this worker. Wired to the
  // dashboard's existing modal state so the expand button reuses that surface
  // rather than duplicating it.
  onExpand?: (worker: WorkerInfo) => void
  // onKilled fires after a successful kill so the dashboard can refresh its
  // poll immediately instead of waiting for the next 5s tick.
  onKilled?: () => void
}

const PREVIEW_ENTRIES = 3

// A worker occupies a Smith slot — and therefore streams live output worth
// showing — while pending, running, reviewing (Warden), or paused. Terminal
// workers linger for a few minutes as frozen panels (isFinishedWorker) — the
// SSE closed, the final transcript shown from the live snapshot or a one-shot
// tail fetch — until they age out of the ?recent= window and unmount.
const ACTIVE_STATUSES = new Set(['pending', 'running', 'reviewing', 'paused'])

// STATUS_CLASSES mirrors WorkersPane so the status chip reads identically
// across the two surfaces.
const STATUS_CLASSES: Record<string, string> = {
  pending: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  running: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  reviewing: 'bg-cyan-500/20 text-cyan-300 border-cyan-500/40',
  paused: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  done: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
  partial: 'bg-amber-500/20 text-amber-200 border-amber-500/40',
}

function statusClass(status: string): string {
  return STATUS_CLASSES[status] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

// finishedStatusClass colours the caption under a lingering frozen panel. It is
// deliberately three-way rather than done-or-red: a partial Assay run covered
// some of the head and failed none of it, so painting it the failure colour
// here would contradict every other surface that shows it in amber.
export function finishedStatusClass(status: string): string {
  if (status === 'done') return 'text-sky-300'
  if (status === 'partial') return 'text-amber-200'
  return 'text-red-300'
}

// formatElapsed renders the wall-clock time since a worker started as a compact
// "5s" / "3m 12s" / "1h 4m" string. `now` is passed in so an interval ticker
// can drive a live-updating display without the helper reaching for a clock.
export function formatElapsed(startedAt: string, now: number): string {
  const start = Date.parse(startedAt)
  if (Number.isNaN(start)) return '—'
  const elapsed = Math.max(0, Math.floor((now - start) / 1000))
  if (elapsed < 60) return `${elapsed}s`
  const mins = Math.floor(elapsed / 60)
  const secs = elapsed % 60
  if (mins < 60) return `${mins}m ${secs}s`
  const hours = Math.floor(mins / 60)
  return `${hours}h ${mins % 60}m`
}

// previewLabel collapses a parsed transcript entry into a single dimmed line for
// the collapsed-panel preview. It intentionally shows just enough to convey
// "what is the worker doing right now" without the full CLI chrome.
function previewLabel(entry: TranscriptEntry): string {
  switch (entry.kind) {
    case 'tool':
      return `⏺ ${entry.name}${entry.headline ? ` (${entry.headline})` : ''}`
    case 'assistant':
      return entry.text.split('\n')[0] ?? ''
    case 'thinking':
      return `✻ ${entry.text.split('\n')[0] ?? ''}`
    case 'meta':
      return `· ${entry.text}`
    case 'summary':
      return '✓ done'
    case 'raw':
      return entry.content.split('\n')[0] ?? ''
    default:
      return ''
  }
}

// WorkerPanel is one large live tile in the dashboard's worker grid. It embeds
// the shared CLI-style LogViewer fed by the worker's native SSE stream, adds a
// live elapsed-time ticker, and exposes kill (confirmed) + expand-to-modal
// actions. Collapse state persists per worker id so operators can tuck away
// panels they are not watching; a collapsed panel closes its EventSource and
// shows a dimmed three-line preview of the last output it saw.
export default function WorkerPanel({
  worker,
  maxItems = 1000,
  onExpand,
  onKilled,
}: WorkerPanelProps) {
  const { run, busy } = useAction()
  const [confirmKill, setConfirmKill] = useState(false)
  const [confirmPause, setConfirmPause] = useState(false)
  const [now, setNow] = useState(() => Date.now())
  // Collapse persists to localStorage keyed by worker id so it survives a
  // refresh; panels default to expanded so live output is visible out of the
  // box.
  const [collapsed, setCollapsed] = useUIState<boolean>(
    `worker-panel.collapsed.${worker.id}`,
    false,
    { storage: 'local' },
  )

  const isActive = ACTIVE_STATUSES.has(worker.status)
  const isFinished = isFinishedWorker(worker)
  const bodyId = `worker-panel-body-${worker.id}`

  // Live elapsed ticker: re-render once a second while the worker is active so
  // the duration stays fresh. A finished panel ticks slowly so its "Xm ago"
  // caption ages; other terminal states keep their frozen final value.
  useEffect(() => {
    if (!isActive && !isFinished) return
    const interval = window.setInterval(
      () => setNow(Date.now()),
      isActive ? 1000 : 15000,
    )
    return () => window.clearInterval(interval)
  }, [isActive, isFinished])

  // Only open the SSE for a visible (non-collapsed), active worker. Collapsing
  // sets url=null which closes the connection (the hook's url=null path), so
  // the dashboard never holds more concurrent streams than there are open
  // panels.
  const streamURL =
    isActive && !collapsed
      ? `/api/worker/${encodeURIComponent(worker.id)}/stream`
      : null
  const live = useEventSource<LogLine>(streamURL, { maxItems })
  const rawLines = useMemo(() => live.items.map((l) => l.line), [live.items])

  // Snapshot the most recent lines while the stream is open so a collapsed
  // panel can still render a preview after its EventSource has closed (the hook
  // clears its own buffer on url=null).
  const [snapshot, setSnapshot] = useState<string[]>([])
  useEffect(() => {
    if (streamURL && rawLines.length) setSnapshot(rawLines)
  }, [streamURL, rawLines])

  // A finished panel mounted after the run (page refresh, or the worker
  // completed before its panel ever streamed) has no live snapshot — fetch the
  // transcript tail once so the lingering panel still shows what happened.
  const [tailLines, setTailLines] = useState<string[] | null>(null)
  useEffect(() => {
    if (!isFinished || snapshot.length > 0 || tailLines !== null) return
    let cancelled = false
    apiGet<LogTailResponse>(
      `/api/worker/${encodeURIComponent(worker.id)}/log?tail=${maxItems}`,
    )
      .then((res) => {
        if (!cancelled) setTailLines(res.lines ?? [])
      })
      .catch(() => {
        if (!cancelled) setTailLines([])
      })
    return () => {
      cancelled = true
    }
  }, [isFinished, snapshot.length, tailLines, worker.id, maxItems])

  const frozenLines = snapshot.length > 0 ? snapshot : (tailLines ?? [])

  const previewEntries = useMemo(() => {
    const source = snapshot.length > 0 ? snapshot : (tailLines ?? [])
    const entries = parseTranscript(source).filter((e) => e.kind !== 'hidden')
    return entries.slice(-PREVIEW_ENTRIES)
  }, [snapshot, tailLines])

  const handleKill = async () => {
    const ok = await run(() => actions.killWorker(worker.id), {
      successMessage: `Kill signal sent to worker ${worker.id.slice(0, 8)}`,
      onSuccess: onKilled,
    })
    if (ok) setConfirmKill(false)
  }

  const handlePause = async () => {
    const ok = await run(() => actions.pause(worker.bead_id), {
      successMessage: `Pause requested for ${worker.bead_id}`,
      onSuccess: onKilled,
    })
    if (ok) setConfirmPause(false)
  }

  const handleResume = () =>
    run(() => actions.resume(worker.bead_id), {
      successMessage: `Resume requested for ${worker.bead_id}`,
      onSuccess: onKilled,
    })

  const canKill = worker.status === 'pending' || worker.status === 'running'
  // Pause/resume gate on the daemon's paused-status transition table: only a
  // running worker may be paused, only a paused worker resumed.
  const isPaused = worker.status === 'paused'
  const canPause = worker.status === 'running'
  const canResume = isPaused
  const liveStatus = live.status

  // Between-spawns semantics (Forge-doim): a pause interrupts the live Claude
  // spawn immediately during the Smith phase, but Temper runs build/test with no
  // Claude spawn — the worker is still "running" yet there is nothing to
  // interrupt, so the daemon honours the pause at the next Smith turn. Surface
  // that so the operator isn't surprised the current check keeps running.
  const pauseDeferred = canPause && worker.phase === 'temper'
  const pauseTitle = pauseDeferred
    ? 'Pause worker — pauses at the next Smith turn (a check is running now)'
    : 'Pause worker'
  const pauseConfirmMessage = pauseDeferred
    ? `${worker.bead_id} is between Smith turns (a Temper check is running). The pause takes effect at the next Smith turn; the current check finishes first. The transcript stays visible and you can resume later.`
    : `This interrupts the Claude process for ${worker.bead_id} (${worker.anvil}) and parks the pipeline. The transcript stays visible and you can resume it later.`

  return (
    <div
      data-testid={`worker-panel-${worker.id}`}
      data-paused={isPaused ? 'true' : undefined}
      className={`flex flex-col overflow-hidden rounded-xl border ${
        isPaused
          ? 'border-amber-500/50 bg-amber-950/10'
          : isFinished
            ? 'border-slate-800/80 bg-slate-900/40 opacity-80'
            : 'border-slate-800 bg-slate-900/60'
      }`}
    >
      <header
        className={`flex items-center gap-2 border-b px-3 py-2 ${
          isPaused ? 'border-amber-500/30' : 'border-slate-800'
        }`}
      >
        <button
          type="button"
          onClick={() => setCollapsed((v) => !v)}
          aria-expanded={!collapsed}
          aria-controls={bodyId}
          data-testid={`worker-panel-toggle-${worker.id}`}
          className="rounded-md p-1 text-slate-400 transition-colors hover:bg-slate-800 hover:text-slate-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
          aria-label={collapsed ? 'Expand panel' : 'Collapse panel'}
        >
          {collapsed ? <ChevronRight size={16} /> : <ChevronDown size={16} />}
        </button>

        <div className="flex min-w-0 flex-1 flex-col">
          <div className="flex flex-wrap items-center gap-2">
            <span
              className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${statusClass(worker.status)}`}
            >
              {worker.status}
            </span>
            {worker.phase && (
              <span className="rounded-md border border-slate-700 bg-slate-800/60 px-2 py-0.5 text-[10px] uppercase tracking-wide text-slate-300">
                {worker.phase}
              </span>
            )}
            <Link
              to={`/bead/${encodeURIComponent(worker.bead_id)}`}
              className="font-mono text-xs text-amber-300 hover:text-amber-200 hover:underline"
            >
              {worker.bead_id}
            </Link>
          </div>
          <p className="mt-0.5 flex flex-wrap items-center gap-x-2 truncate text-xs text-slate-500">
            <span className="truncate text-slate-300">{worker.title || worker.bead_id}</span>
            <span aria-hidden>·</span>
            <span>{worker.anvil}</span>
          </p>
        </div>

        <span
          className="shrink-0 text-xs tabular-nums text-slate-400"
          title={`Started ${worker.started_at}`}
          data-testid={`worker-panel-elapsed-${worker.id}`}
        >
          {isFinished && worker.completed_at
            ? formatElapsed(worker.started_at, Date.parse(worker.completed_at))
            : formatElapsed(worker.started_at, now)}
        </span>

        {/* Preview trigger. Most useful on a ready-to-merge card — the branch
            is final and the operator wants to look at it before merging — but
            it is offered for any worker with a branch, since the preview is
            built from the branch tip and never touches the worktree. It hides
            itself when the anvil declares no preview manifest. */}
        <PreviewButton
          beadId={worker.bead_id}
          anvil={worker.anvil}
          hasBranch={!!worker.branch}
          compact
          className="shrink-0"
        />

        {onExpand && (
          <button
            type="button"
            onClick={() => onExpand(worker)}
            data-testid={`worker-panel-expand-${worker.id}`}
            className="rounded-md border border-slate-700 bg-slate-800/60 p-1.5 text-slate-300 transition-colors hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
            aria-label={`Expand log for ${worker.bead_id}`}
            title="Expand to full log"
          >
            <Maximize2 size={14} />
          </button>
        )}
        {canPause && (
          <button
            type="button"
            onClick={() => setConfirmPause(true)}
            disabled={busy}
            data-testid={`worker-panel-pause-${worker.id}`}
            className="rounded-md border border-amber-500/40 bg-amber-500/10 p-1.5 text-amber-300 transition-colors hover:bg-amber-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:opacity-50"
            aria-label={`Pause worker ${worker.bead_id}`}
            title={pauseTitle}
          >
            <Pause size={14} />
          </button>
        )}
        {canResume && (
          <button
            type="button"
            onClick={handleResume}
            disabled={busy}
            data-testid={`worker-panel-resume-${worker.id}`}
            className="rounded-md border border-emerald-500/40 bg-emerald-500/10 p-1.5 text-emerald-300 transition-colors hover:bg-emerald-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-300 disabled:opacity-50"
            aria-label={`Resume worker ${worker.bead_id}`}
            title="Resume worker"
          >
            <Play size={14} />
          </button>
        )}
        {canKill && (
          <button
            type="button"
            onClick={() => setConfirmKill(true)}
            disabled={busy}
            data-testid={`worker-panel-kill-${worker.id}`}
            className="rounded-md border border-red-500/40 bg-red-500/10 p-1.5 text-red-300 transition-colors hover:bg-red-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-red-400 disabled:opacity-50"
            aria-label={`Kill worker ${worker.id}`}
            title="Kill worker"
          >
            <Skull size={14} />
          </button>
        )}
      </header>

      {collapsed ? (
        <div
          className="flex flex-col gap-0.5 px-4 py-2 opacity-60"
          data-testid={`worker-panel-preview-${worker.id}`}
        >
          {previewEntries.length === 0 ? (
            <span className="text-xs text-slate-600">No output captured yet.</span>
          ) : (
            previewEntries.map((entry, i) => (
              <span
                key={i}
                className="truncate font-mono text-xs text-slate-400"
              >
                {previewLabel(entry)}
              </span>
            ))
          )}
        </div>
      ) : (
        <div id={bodyId} className="flex flex-col">
          {isActive ? (
            <LogViewer
              rawLines={rawLines}
              liveWaiting={!isPaused && liveStatus === 'open' && rawLines.length === 0}
              statusText={
                isPaused ? (
                  <span
                    className="inline-flex items-center gap-1.5 text-amber-300"
                    data-testid={`worker-panel-frozen-${worker.id}`}
                  >
                    <Pause size={12} aria-hidden />
                    paused — stream frozen
                  </span>
                ) : (
                  <span className="inline-flex items-center gap-1.5">
                    <Terminal size={12} className="text-emerald-400" aria-hidden />
                    {liveStatus === 'open'
                      ? 'live'
                      : liveStatus === 'connecting'
                        ? 'connecting…'
                        : liveStatus === 'error'
                          ? 'reconnecting…'
                          : 'closed'}
                  </span>
                )
              }
              keyPrefix={worker.id}
              heightClass="max-h-96"
              jumpToBottom={!isPaused}
            />
          ) : isFinished ? (
            <LogViewer
              rawLines={frozenLines}
              liveWaiting={false}
              statusText={
                <span
                  className={`inline-flex items-center gap-1.5 ${finishedStatusClass(
                    worker.status,
                  )}`}
                  data-testid={`worker-panel-finished-${worker.id}`}
                >
                  <Terminal size={12} aria-hidden />
                  {worker.status} · {relativeTime(worker.completed_at)}
                </span>
              }
              keyPrefix={worker.id}
              heightClass="max-h-96"
              jumpToBottom={false}
            />
          ) : (
            <p className="px-4 py-6 text-center text-xs text-slate-500">
              Worker is {worker.status} — no live output.
            </p>
          )}
        </div>
      )}

      {/* Inline steer composer: reuses the shared SteerComposer (also mounted in
          the WorkerLogModal) so the dashboard grid can course-correct a live
          Smith without opening the full-screen view. steerDisabledReason mirrors
          the daemon's steer acceptance matrix (running/pending/reviewing/paused),
          so a non-Claude worker renders a disabled input with an explanatory
          tooltip rather than a dead button. A paused worker stays enabled but its
          message is delivered as a resume-with-message (steerIsResumeDelivery). */}
      {!collapsed && isActive && (
        <SteerComposer
          beadID={worker.bead_id}
          disabledReason={steerDisabledReason(worker)}
          paused={steerIsResumeDelivery(worker)}
          compact
        />
      )}

      <ConfirmModal
        open={confirmKill}
        title="Kill worker?"
        message={`This will SIGTERM the Smith process for ${worker.bead_id} (${worker.anvil}). Any in-progress changes will be lost.`}
        confirmLabel="Kill worker"
        tone="danger"
        busy={busy}
        onConfirm={handleKill}
        onCancel={() => setConfirmKill(false)}
      />

      <ConfirmModal
        open={confirmPause}
        title="Pause worker?"
        message={pauseConfirmMessage}
        confirmLabel="Pause worker"
        tone="primary"
        busy={busy}
        onConfirm={handlePause}
        onCancel={() => setConfirmPause(false)}
      />
    </div>
  )
}
