import { useEffect, useMemo, useRef, useState } from 'react'
import { Loader2, ScrollText, Terminal, X } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import type { LogLine, LogTailResponse, WorkerInfo } from '../api'
import { ApiError, apiGet } from '../api'
import { useAuth } from '../auth'
import { useEventSource } from '../hooks/useEventSource'
import {
  parseTranscript,
  type SummaryEntry,
  type ToolEntry,
  type TranscriptEntry,
} from '../lib/logParse'

interface WorkerLogModalProps {
  worker: WorkerInfo | null
  onClose: () => void
}

const TAIL_LINES = 500
const RESULT_PREVIEW_LINES = 3

// WorkerLogModal renders a worker's log as a Claude Code CLI-style transcript.
// Active workers stream live via /api/worker/{id}/stream; completed workers
// fall back to a one-shot tail fetch from /api/worker/{id}/log?tail=N. Lines
// are parsed into a transcript model (paired tool calls/results, markdown
// assistant text, classified system noise) and rendered with a glyph gutter.
export default function WorkerLogModal({ worker, onClose }: WorkerLogModalProps) {
  const { logout } = useAuth()
  const isLive = !!worker && (worker.status === 'pending' || worker.status === 'running')

  const [tailLines, setTailLines] = useState<string[] | null>(null)
  const [tailError, setTailError] = useState<string | null>(null)
  const [tailLoading, setTailLoading] = useState(false)
  const [verbose, setVerbose] = useState(false)

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

  const entries = useMemo(() => parseTranscript(rawLines), [rawLines])

  const visible = useMemo(
    () =>
      entries
        .map((e, i) => ({ entry: e, originalIndex: i }))
        .filter(({ entry }) => verbose || entry.kind !== 'hidden'),
    [entries, verbose],
  )
  const hiddenCount = useMemo(() => entries.filter((e) => e.kind === 'hidden').length, [entries])

  // Auto-scroll to bottom unless the user has scrolled up.
  const scrollRef = useRef<HTMLDivElement | null>(null)
  const stickToBottomRef = useRef(true)
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    if (stickToBottomRef.current) {
      el.scrollTop = el.scrollHeight
    }
  }, [visible])

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
              {worker.model && (
                <>
                  <span aria-hidden>·</span>
                  <span className="font-mono text-slate-400">{worker.model}</span>
                </>
              )}
            </p>
          </div>
          <label className="flex items-center gap-1.5 text-xs text-slate-400 select-none">
            <input
              type="checkbox"
              checked={verbose}
              onChange={(e) => setVerbose(e.target.checked)}
              className="accent-amber-400"
            />
            <span>
              verbose{hiddenCount > 0 && !verbose ? ` (${hiddenCount})` : ''}
            </span>
          </label>
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
          {visible.length === 0 ? (
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
            <div className="flex flex-col gap-3 px-4 py-3">
              {visible.map(({ entry, originalIndex }) => (
                <TranscriptRow key={originalIndex} entry={entry} />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function TranscriptRow({ entry }: { entry: TranscriptEntry }) {
  switch (entry.kind) {
    case 'tool':
      return <ToolRow entry={entry} />
    case 'assistant':
      return (
        <div className="flex gap-2">
          <span className="select-none text-emerald-400" aria-hidden>
            ⏺
          </span>
          <MarkdownBlock text={entry.text} />
        </div>
      )
    case 'thinking':
      return <ThinkingRow text={entry.text} />
    case 'meta':
      return (
        <div className="flex gap-2 text-slate-500">
          <span className="select-none" aria-hidden>
            ·
          </span>
          <span className="italic">{entry.text}</span>
        </div>
      )
    case 'summary':
      return <SummaryRow entry={entry} />
    case 'hidden':
      return (
        <div className="flex gap-2 text-slate-600">
          <span className="select-none" aria-hidden>
            ·
          </span>
          <div className="min-w-0">
            <span className="uppercase tracking-wide">{entry.label}</span>
            {entry.content && (
              <pre className="mt-0.5 whitespace-pre-wrap break-all text-slate-600">
                {entry.content}
              </pre>
            )}
          </div>
        </div>
      )
    case 'raw':
      return (
        <div className="flex gap-2">
          <span className="select-none text-slate-600" aria-hidden>
            ⎿
          </span>
          <div className="min-w-0 flex-1">
            {entry.name && (
              <span
                className={`mr-2 text-[11px] uppercase tracking-wide ${
                  entry.status === 'error' ? 'text-red-400' : 'text-slate-500'
                }`}
              >
                {entry.name}
              </span>
            )}
            <pre
              className={`inline whitespace-pre-wrap break-all leading-relaxed ${
                entry.status === 'error' ? 'text-red-300' : 'text-slate-300'
              }`}
            >
              {entry.content}
            </pre>
          </div>
        </div>
      )
  }
}

function ToolRow({ entry }: { entry: ToolEntry }) {
  const [expanded, setExpanded] = useState(!!entry.isError)

  // Sync expanded state when a streaming tool_result arrives with an error
  // after the row has already mounted in its collapsed (non-error) state.
  useEffect(() => {
    if (entry.isError) setExpanded(true)
  }, [entry.isError])

  const resultLines = entry.result ? entry.result.split('\n') : []
  const truncated = resultLines.length > RESULT_PREVIEW_LINES
  const preview = resultLines.slice(0, RESULT_PREVIEW_LINES).join('\n')
  const hiddenLines = resultLines.length - RESULT_PREVIEW_LINES
  const hasExpandable = truncated || entry.input != null

  return (
    <div className="flex flex-col gap-1">
      <div className="flex gap-2">
        <span
          className={`select-none ${entry.isError ? 'text-red-400' : 'text-emerald-400'}`}
          aria-hidden
        >
          ⏺
        </span>
        <div className="min-w-0 flex-1">
          <span className="text-slate-200">
            <span className="font-semibold text-sky-300">{entry.name}</span>
            {entry.headline && <span className="text-slate-400">({entry.headline})</span>}
          </span>
        </div>
      </div>

      {entry.todos ? (
        <div className="ml-6 flex flex-col gap-0.5">
          {entry.todos.map((t, i) => (
            <div key={i} className="flex gap-1.5 text-slate-300">
              <span className="select-none text-slate-500" aria-hidden>
                {t.status === 'completed' ? '☒' : '☐'}
              </span>
              <span className={t.status === 'completed' ? 'text-slate-500 line-through' : ''}>
                {t.content}
              </span>
            </div>
          ))}
        </div>
      ) : (
        (entry.result || hasExpandable) && (
          <div className="ml-6 flex gap-2">
            <span
              className={`select-none ${entry.isError ? 'text-red-500' : 'text-slate-600'}`}
              aria-hidden
            >
              ⎿
            </span>
            <div className="min-w-0 flex-1">
              {entry.result != null && (
                <pre
                  className={`whitespace-pre-wrap break-all leading-relaxed ${
                    entry.isError ? 'text-red-300' : 'text-slate-400'
                  }`}
                >
                  {expanded ? entry.result : preview}
                </pre>
              )}
              {hasExpandable && (
                <button
                  type="button"
                  onClick={() => setExpanded((v) => !v)}
                  className="mt-0.5 text-[11px] text-slate-500 underline decoration-dotted hover:text-slate-300"
                >
                  {expanded
                    ? 'show less'
                    : truncated
                      ? `+${hiddenLines} line${hiddenLines === 1 ? '' : 's'}`
                      : 'show input'}
                </button>
              )}
              {expanded && entry.input != null && (
                <pre className="mt-1 whitespace-pre-wrap break-all rounded bg-slate-900/60 p-2 leading-relaxed text-slate-500">
                  {safeStringify(entry.input)}
                </pre>
              )}
            </div>
          </div>
        )
      )}
    </div>
  )
}

function ThinkingRow({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false)
  const lines = text.split('\n')
  const firstLine = lines[0] ?? ''
  const multiline = lines.length > 1 || firstLine.length > 120

  return (
    <div className="flex gap-2 text-slate-500">
      <span className="select-none" aria-hidden>
        ✻
      </span>
      <div className="min-w-0 flex-1 italic">
        <span className="whitespace-pre-wrap break-words">
          {expanded ? text : firstLine}
        </span>
        {multiline && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="ml-2 not-italic text-[11px] text-slate-500 underline decoration-dotted hover:text-slate-300"
          >
            {expanded ? 'show less' : 'show more'}
          </button>
        )}
      </div>
    </div>
  )
}

function SummaryRow({ entry }: { entry: SummaryEntry }) {
  const parts: string[] = []
  if (entry.durationMs != null) parts.push(formatDuration(entry.durationMs))
  if (entry.numTurns != null) parts.push(`${entry.numTurns} turn${entry.numTurns === 1 ? '' : 's'}`)
  if (entry.totalCostUsd != null) parts.push(`$${entry.totalCostUsd.toFixed(4)}`)
  const tokens = [entry.inputTokens, entry.outputTokens]
  if (tokens.some((t) => t != null)) {
    parts.push(`${entry.inputTokens ?? 0} in / ${entry.outputTokens ?? 0} out tok`)
  }
  return (
    <div className="mt-1 flex gap-2 border-t border-slate-800 pt-3 text-slate-400">
      <span className="select-none text-amber-400" aria-hidden>
        ✓
      </span>
      <span>{parts.length ? parts.join(' · ') : 'done'}</span>
    </div>
  )
}

// MarkdownBlock renders assistant prose as markdown. react-markdown does not
// render raw HTML by default (no rehype-raw plugin) and we never use
// dangerouslySetInnerHTML, so embedded HTML is escaped as text.
function MarkdownBlock({ text }: { text: string }) {
  return (
    <div className="min-w-0 flex-1 space-y-2 whitespace-normal leading-relaxed break-words text-slate-200">
      <ReactMarkdown
        components={{
          p: ({ children }) => <p className="whitespace-pre-wrap">{children}</p>,
          a: ({ children, href }) => (
            <a
              href={href}
              target="_blank"
              rel="noreferrer noopener"
              className="text-sky-400 underline"
            >
              {children}
            </a>
          ),
          ul: ({ children }) => <ul className="list-disc space-y-0.5 pl-5">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal space-y-0.5 pl-5">{children}</ol>,
          h1: ({ children }) => <h1 className="text-sm font-bold text-slate-100">{children}</h1>,
          h2: ({ children }) => <h2 className="text-sm font-bold text-slate-100">{children}</h2>,
          h3: ({ children }) => <h3 className="font-bold text-slate-100">{children}</h3>,
          code: ({ children, className }) => {
            const isBlock = /language-/.test(className ?? '')
            if (isBlock) {
              return (
                <code className="block overflow-x-auto rounded bg-slate-900/80 p-2 text-slate-200">
                  {children}
                </code>
              )
            }
            return (
              <code className="rounded bg-slate-800 px-1 py-0.5 text-amber-200">{children}</code>
            )
          },
          pre: ({ children }) => <pre className="overflow-x-auto">{children}</pre>,
          blockquote: ({ children }) => (
            <blockquote className="border-l-2 border-slate-700 pl-3 text-slate-400">
              {children}
            </blockquote>
          ),
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  )
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`
  const seconds = ms / 1000
  if (seconds < 60) return `${seconds.toFixed(1)}s`
  const minutes = Math.floor(seconds / 60)
  const rem = Math.round(seconds % 60)
  return `${minutes}m ${rem}s`
}

function safeStringify(input: unknown): string {
  if (typeof input === 'string') return input
  try {
    return JSON.stringify(input, null, 2)
  } catch {
    return String(input)
  }
}
