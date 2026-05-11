import { useEffect, useMemo, useRef, useState } from 'react'
import { Check, Loader2, ScrollText, Terminal, X } from 'lucide-react'
import type { LogLine, LogTailResponse, WorkerInfo } from '../api'
import { ApiError, apiGet } from '../api'
import { useAuth } from '../auth'
import { useEventSource } from '../hooks/useEventSource'
import { parseLogLine, type LogEntry } from '../lib/logParse'

interface WorkerLogModalProps {
  worker: WorkerInfo | null
  onClose: () => void
}

const TAIL_LINES = 500

// WorkerLogModal renders a worker's log file. Active workers stream live via
// /api/worker/{id}/stream; completed workers fall back to a one-shot tail
// fetch from /api/worker/{id}/log?tail=N. Either way the entries are parsed
// into structured rows so tool_use blocks render distinctly from plain text.
export default function WorkerLogModal({ worker, onClose }: WorkerLogModalProps) {
  const { logout } = useAuth()
  const isLive = !!worker && (worker.status === 'pending' || worker.status === 'running')

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

  const rawLines: { text: string; key: string }[] = useMemo(() => {
    if (!worker) return []
    if (isLive) {
      return live.items.map((l, i) => ({ text: l.line, key: `live-${i}` }))
    }
    if (!tailLines) return []
    return tailLines.map((line, i) => ({ text: line, key: `tail-${i}` }))
  }, [worker, isLive, live.items, tailLines])

  const entries = useMemo(() => {
    const out: { id: string; entry: LogEntry }[] = []
    for (const r of rawLines) {
      const parsed = parseLogLine(r.text)
      parsed.forEach((entry, idx) => {
        out.push({ id: `${r.key}-${idx}`, entry })
      })
    }
    return out
  }, [rawLines])

  // Auto-scroll to bottom unless the user has scrolled up.
  const scrollRef = useRef<HTMLDivElement | null>(null)
  const stickToBottomRef = useRef(true)
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    if (stickToBottomRef.current) {
      el.scrollTop = el.scrollHeight
    }
  }, [entries])

  function onScroll(e: React.UIEvent<HTMLDivElement>) {
    const el = e.currentTarget
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    stickToBottomRef.current = distanceFromBottom < 32
  }

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

        <div
          ref={scrollRef}
          onScroll={onScroll}
          className="min-h-[20rem] flex-1 overflow-y-auto bg-slate-950/60 font-mono text-xs"
          role="log"
          aria-live="polite"
        >
          {entries.length === 0 ? (
            <div className="flex h-40 items-center justify-center gap-2 text-slate-500">
              {tailLoading ||
              liveStatus === 'connecting' ||
              (isLive && liveStatus === 'open') ? (
                <>
                  <Loader2 size={14} className="animate-spin" />
                  <span>
                    {isLive && liveStatus === 'open'
                      ? 'Waiting for log output…'
                      : 'Loading log…'}
                  </span>
                </>
              ) : (
                <span className="text-sm">
                  <ScrollText size={14} className="mr-1.5 inline align-text-bottom" />
                  No log content yet.
                </span>
              )}
            </div>
          ) : (
            <ul className="divide-y divide-slate-900/60">
              {entries.map((row) => (
                <li key={row.id}>
                  <LogEntryRow entry={row.entry} />
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </div>
  )
}

function LogEntryRow({ entry }: { entry: LogEntry }) {
  if (entry.kind === 'tool_use') {
    return (
      <div className="flex flex-col gap-1 px-4 py-2">
        <div className="flex items-center gap-2">
          <span className="rounded bg-purple-500/20 px-1.5 py-0.5 font-mono text-[11px] font-semibold uppercase tracking-wide text-purple-300">
            {entry.name}
          </span>
        </div>
        {entry.content && (
          <pre className="whitespace-pre-wrap break-all leading-relaxed text-slate-300">
            {entry.content}
          </pre>
        )}
      </div>
    )
  }
  if (entry.kind === 'tool_result') {
    return (
      <div className="flex flex-col gap-1 px-4 py-2">
        <div className="flex items-center gap-2 text-[11px] text-slate-400">
          <span className="font-mono uppercase tracking-wide">tool result</span>
          {entry.status === 'success' && <Check size={12} className="text-emerald-400" />}
          {entry.status === 'error' && <X size={12} className="text-red-400" />}
        </div>
        {entry.content && (
          <pre
            className={`whitespace-pre-wrap break-all leading-relaxed ${
              entry.status === 'error' ? 'text-red-300' : 'text-slate-400'
            }`}
          >
            {entry.content}
          </pre>
        )}
      </div>
    )
  }
  if (entry.kind === 'thinking') {
    return (
      <div className="flex flex-col gap-1 px-4 py-2">
        <span className="text-[11px] italic text-slate-500">thinking</span>
        <p className="whitespace-pre-wrap break-words italic leading-relaxed text-slate-500">
          {entry.content}
        </p>
      </div>
    )
  }
  if (entry.kind === 'system') {
    return (
      <div className="px-4 py-1 text-[11px] text-slate-500">
        <span className="font-mono uppercase tracking-wide text-slate-500">{entry.name}</span>
      </div>
    )
  }
  // text or raw
  return (
    <pre className="whitespace-pre-wrap break-words px-4 py-2 leading-relaxed text-slate-200">
      {entry.content}
    </pre>
  )
}
