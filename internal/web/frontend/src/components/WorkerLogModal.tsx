import { useEffect, useMemo, useState } from 'react'
import { Pause, Play, Terminal, X } from 'lucide-react'
import type { LogLine, LogTailResponse, WorkerInfo } from '../api'
import { actions, ApiError, apiGet, steerDisabledReason } from '../api'
import { useAuth } from '../auth'
import { useAction } from '../hooks/useAction'
import { useEventSource } from '../hooks/useEventSource'
import LogViewer from './LogViewer'
import SteerComposer from './SteerComposer'

interface WorkerLogModalProps {
  worker: WorkerInfo | null
  onClose: () => void
}

const TAIL_LINES = 500

// WorkerLogModal renders a worker's log as a Claude Code CLI-style transcript.
// Active workers stream live via /api/worker/{id}/stream; completed workers
// fall back to a one-shot tail fetch from /api/worker/{id}/log?tail=N. The
// parsing/rendering is delegated to the shared LogViewer component.
export default function WorkerLogModal({ worker, onClose }: WorkerLogModalProps) {
  const { logout } = useAuth()
  const { run, busy } = useAction()
  const isLive = !!worker && (worker.status === 'pending' || worker.status === 'running')
  // Pause/resume follow the daemon's paused-status transition table: only a
  // running worker may be paused, and only a paused worker may be resumed.
  const canPause = worker?.status === 'running'
  const canResume = worker?.status === 'paused'

  const [tailLines, setTailLines] = useState<string[] | null>(null)
  const [tailError, setTailError] = useState<string | null>(null)
  const [tailLoading, setTailLoading] = useState(false)

  // Live tail: open the SSE only for active workers. The hook pauses by
  // setting url=null when the modal is closed.
  const liveURL = isLive && worker ? `/api/worker/${encodeURIComponent(worker.id)}/stream` : null
  const live = useEventSource<LogLine>(liveURL, { maxItems: 1000 })

  // For completed workers, fetch the tail once when the modal opens.
  // Also clears the live buffer when switching workers so stale lines from
  // the previous session don't persist until new SSE frames arrive.
  useEffect(() => {
    if (!worker || isLive) {
      setTailLines(null)
      setTailError(null)
      live.clear()
      return
    }
    let cancelled = false
    setTailLoading(true)
    setTailError(null)
    apiGet<LogTailResponse>(`/api/worker/${encodeURIComponent(worker.id)}/log?tail=${TAIL_LINES}`)
      .then((resp) => {
        if (cancelled) return
        setTailLines(resp.lines ?? [])
      })
      .catch((err) => {
        if (cancelled) return
        if (err instanceof ApiError && err.status === 401) {
          void logout()
          return
        }
        const message = err instanceof Error ? err.message : 'failed to load log'
        setTailError(message)
      })
      .finally(() => {
        if (!cancelled) setTailLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [worker, isLive, logout, live.clear])

  const rawLines: string[] = useMemo(() => {
    if (!worker) return []
    if (isLive) return live.items.map((l) => l.line)
    return tailLines ?? []
  }, [worker, isLive, live.items, tailLines])

  // ESC closes the modal.
  useEffect(() => {
    if (!worker) return
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [worker, onClose])

  if (!worker) return null

  const liveStatus = live.status
  const subtitle = isLive
    ? liveStatus === 'open'
      ? 'live'
      : liveStatus === 'connecting'
        ? 'connecting…'
        : liveStatus === 'error'
          ? 'reconnecting…'
          : 'closed'
    : tailLoading
      ? 'loading…'
      : `last ${TAIL_LINES} lines`

  return (
    <div
      className="fixed inset-0 z-50 flex items-stretch justify-center bg-black/60 p-2 sm:p-6"
      role="dialog"
      aria-modal="true"
      aria-label={`Logs for ${worker.title || worker.bead_id}`}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="flex w-full max-w-5xl flex-col rounded-xl border border-slate-800 bg-slate-900 shadow-xl">
        <header className="flex items-center gap-3 border-b border-slate-800 px-4 py-3">
          <Terminal size={18} className="text-amber-400" aria-hidden />
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-semibold text-slate-100">
              {worker.title || worker.bead_id}
            </p>
            <p className="flex flex-wrap items-center gap-x-2 text-xs text-slate-500">
              <span className="font-mono text-slate-400">{worker.bead_id}</span>
              <span aria-hidden>·</span>
              <span>{worker.anvil}</span>
              <span aria-hidden>·</span>
              <span className={isLive ? 'text-emerald-300' : 'text-slate-400'}>
                {isLive ? 'running' : worker.status}
              </span>
              <span aria-hidden>·</span>
              <span>{subtitle}</span>
              {worker.model && (
                <>
                  <span aria-hidden>·</span>
                  <span className="font-mono text-slate-400">{worker.model}</span>
                </>
              )}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-md p-1 text-slate-400 transition-colors hover:bg-slate-800 hover:text-slate-200"
            aria-label="Close"
          >
            <X size={18} />
          </button>
        </header>

        {tailError && (
          <div className="border-b border-red-700/40 bg-red-900/20 px-4 py-2 text-sm text-red-200">
            {tailError}
          </div>
        )}

        <LogViewer
          rawLines={rawLines}
          loading={tailLoading}
          liveWaiting={isLive && liveStatus === 'open'}
          keyPrefix={worker.id}
        />

        {(canPause || canResume) && (
          <div className="flex items-center gap-2 border-t border-slate-800 bg-slate-950/40 px-4 py-2">
            {canPause && (
              <button
                type="button"
                onClick={() =>
                  run(() => actions.pause(worker.bead_id), {
                    successMessage: `Pause requested for ${worker.bead_id}`,
                  })
                }
                disabled={busy}
                data-testid="worker-log-pause"
                className="inline-flex items-center gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-1.5 text-xs text-amber-200 hover:bg-amber-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:opacity-50"
                title="Pause worker"
              >
                <Pause size={12} aria-hidden />
                {busy ? 'Pausing…' : 'Pause'}
              </button>
            )}
            {canResume && (
              <button
                type="button"
                onClick={() =>
                  run(() => actions.resume(worker.bead_id), {
                    successMessage: `Resume requested for ${worker.bead_id}`,
                  })
                }
                disabled={busy}
                data-testid="worker-log-resume"
                className="inline-flex items-center gap-1.5 rounded-md border border-emerald-500/40 bg-emerald-500/10 px-3 py-1.5 text-xs text-emerald-200 hover:bg-emerald-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-300 disabled:opacity-50"
                title="Resume worker"
              >
                <Play size={12} aria-hidden />
                {busy ? 'Resuming…' : 'Resume'}
              </button>
            )}
            {canResume && (
              <span className="text-xs text-amber-300/80">
                Worker is paused — resume to continue the pipeline.
              </span>
            )}
          </div>
        )}

        {isLive && (
          <SteerComposer
            beadID={worker.bead_id}
            disabledReason={steerDisabledReason(worker)}
            compact
          />
        )}
      </div>
    </div>
  )
}
