import { useCallback, useEffect, useState } from 'react'
import { RefreshCw, ScrollText, X } from 'lucide-react'
import { ApiError, apiGet, type LogTailResponse } from '../api'
import { useAuth } from '../auth'

const TAIL_LINES = 500
const REFRESH_MS = 5000

// PreviewLogTarget identifies the one service log this modal is showing.
export interface PreviewLogTarget {
  beadId: string
  service: string
  /** The daemon-supplied tail endpoint (PreviewServiceStatus.log_url). */
  logUrl: string
}

interface PreviewLogModalProps {
  target: PreviewLogTarget | null
  onClose: () => void
}

// PreviewLogModal tails one preview service's log.
//
// It deliberately does not go through LogViewer: that component parses claude
// stream-json transcripts, and a preview log is the raw stdout/stderr of
// whatever the manifest spawns — `npm run dev`, a Go binary, a compose stack.
// Rendering it as plain monospace lines is both correct and what an operator
// debugging a failed health check actually wants to read.
export default function PreviewLogModal({ target, onClose }: PreviewLogModalProps) {
  const { logout } = useAuth()
  const [lines, setLines] = useState<string[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const logUrl = target?.logUrl ?? null

  const load = useCallback(
    async (signal?: AbortSignal) => {
      if (!logUrl) return
      setLoading(true)
      try {
        const sep = logUrl.includes('?') ? '&' : '?'
        const resp = await apiGet<LogTailResponse>(`${logUrl}${sep}tail=${TAIL_LINES}`, signal)
        if (signal?.aborted) return
        setLines(resp.lines ?? [])
        setError(null)
      } catch (err) {
        if (signal?.aborted) return
        if (err instanceof ApiError && err.status === 401) {
          void logout()
          return
        }
        setError(err instanceof Error ? err.message : 'failed to load preview log')
      } finally {
        if (!signal?.aborted) setLoading(false)
      }
    },
    [logUrl, logout],
  )

  // Reset between services so the previous service's output never shows under
  // the new one's header, then tail on a slow interval — the file is appended
  // to by a live process, and there is no SSE stream for preview services.
  useEffect(() => {
    if (!logUrl) return
    const controller = new AbortController()
    setLines([])
    setError(null)
    void load(controller.signal)
    const timer = setInterval(() => {
      void load(controller.signal)
    }, REFRESH_MS)
    return () => {
      controller.abort()
      clearInterval(timer)
    }
  }, [logUrl, load])

  useEffect(() => {
    if (!target) return
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [target, onClose])

  if (!target) return null

  return (
    <div
      className="fixed inset-0 z-50 flex items-stretch justify-center bg-black/60 p-2 sm:p-6"
      role="dialog"
      aria-modal="true"
      aria-label={`Preview log for ${target.service}`}
      data-testid="preview-log-modal"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="flex w-full max-w-4xl flex-col rounded-xl border border-slate-800 bg-slate-900 shadow-xl">
        <header className="flex items-center gap-3 border-b border-slate-800 px-4 py-3">
          <ScrollText size={18} className="text-amber-400" aria-hidden />
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-semibold text-slate-100">
              {target.service} · preview log
            </p>
            <p className="flex flex-wrap items-center gap-x-2 text-xs text-slate-500">
              <span className="font-mono text-slate-400">{target.beadId}</span>
              <span aria-hidden>·</span>
              <span>{loading ? 'loading…' : `last ${TAIL_LINES} lines`}</span>
            </p>
          </div>
          <button
            type="button"
            onClick={() => void load()}
            disabled={loading}
            data-testid="preview-log-refresh"
            aria-label="Refresh log"
            className="rounded-md p-1 text-slate-400 transition-colors hover:bg-slate-800 hover:text-slate-200 disabled:opacity-50"
          >
            <RefreshCw size={16} aria-hidden />
          </button>
          <button
            type="button"
            onClick={onClose}
            className="rounded-md p-1 text-slate-400 transition-colors hover:bg-slate-800 hover:text-slate-200"
            aria-label="Close"
          >
            <X size={18} />
          </button>
        </header>

        {error && (
          <div className="border-b border-red-700/40 bg-red-900/20 px-4 py-2 text-sm text-red-200">
            {error}
          </div>
        )}

        <div className="min-h-[16rem] flex-1 overflow-auto bg-slate-950/60 px-4 py-3">
          {lines.length === 0 ? (
            <p className="py-8 text-center text-sm text-slate-500">
              {loading ? 'Loading log…' : 'No output yet.'}
            </p>
          ) : (
            <pre
              data-testid="preview-log-body"
              className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed text-slate-300"
            >
              {lines.join('\n')}
            </pre>
          )}
        </div>
      </div>
    </div>
  )
}
