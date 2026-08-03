import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ArrowDown, Loader2, ScrollText } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import {
  parseTranscript,
  type HiddenEntry,
  type SummaryEntry,
  type ToolEntry,
  type TranscriptEntry,
} from '../lib/logParse'

const RESULT_PREVIEW_LINES = 3
// Hidden rows are raw wire records, some of them multi-kilobyte (a thinking
// block carrying only an encrypted signature runs ~1.5KB). Clamp them so
// switching on verbose stays readable.
const HIDDEN_PREVIEW_CHARS = 200

export interface LogViewerProps {
  // rawLines are the newline-delimited claude stream-json records (already
  // split into individual lines). They are parsed into the transcript model on
  // every change.
  rawLines: string[]
  // loading shows a spinner + "Loading log…" while a one-shot tail is in flight.
  loading?: boolean
  // liveWaiting shows "Waiting for log output…" when a live stream is connected
  // but has not yet produced any lines.
  liveWaiting?: boolean
  // statusText is rendered at the left of the toolbar (e.g. "live", "last 500
  // lines"). Optional.
  statusText?: ReactNode
  // keyPrefix disambiguates row keys when the same viewer instance is reused
  // for different sources (e.g. switching stage files).
  keyPrefix?: string
  // heightClass overrides the scroll container sizing. Defaults to the modal /
  // bead-logs full-height sizing (`min-h-[16rem] flex-1`); the worker panels
  // pass a bounded height such as `max-h-96` so the transcript scrolls inside a
  // fixed-height card instead of growing the page.
  heightClass?: string
  // jumpToBottom renders a floating "jump to bottom" control whenever the user
  // has scrolled up and auto-follow is suspended. Off by default so the modal
  // and bead-logs section keep their existing chrome; clicking it snaps back to
  // the tail and re-enables auto-follow.
  jumpToBottom?: boolean
}

// LogViewer renders a claude session log as a Claude Code CLI-style transcript:
// paired tool calls/results, markdown assistant text, thinking, and a summary
// footer, with a glyph gutter. It owns the verbose toggle (which reveals
// classified system noise) and the auto-scroll-to-bottom behaviour. Both the
// live WorkerLogModal and the per-bead Logs section feed it raw lines.
export default function LogViewer({
  rawLines,
  loading = false,
  liveWaiting = false,
  statusText,
  keyPrefix = 'log',
  heightClass,
  jumpToBottom = false,
}: LogViewerProps) {
  const [verbose, setVerbose] = useState(false)
  // atBottom drives the optional jump-to-bottom control. It mirrors
  // stickToBottomRef but lives in state so a scroll-up re-renders the button
  // into view; the ref stays the source of truth for the auto-scroll effect to
  // avoid stale-closure surprises under rapid streaming.
  const [atBottom, setAtBottom] = useState(true)

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
      // Re-render the jump button out of view once new entries land while we
      // are following. setState bails out when already true, so this is cheap.
      setAtBottom(true)
    }
  }, [visible])

  function onScroll(e: React.UIEvent<HTMLDivElement>) {
    const el = e.currentTarget
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    const stick = distanceFromBottom < 32
    stickToBottomRef.current = stick
    setAtBottom(stick)
  }

  function scrollToBottom() {
    const el = scrollRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
    stickToBottomRef.current = true
    setAtBottom(true)
  }

  return (
    <div className={`relative flex min-h-0 flex-col ${heightClass ? '' : 'flex-1'}`}>
      <div className="flex items-center gap-3 border-b border-slate-800 px-4 py-1.5 text-xs text-slate-500">
        {statusText != null && <span className="min-w-0 truncate">{statusText}</span>}
        <label className="ml-auto flex items-center gap-1.5 select-none text-slate-400">
          <input
            type="checkbox"
            checked={verbose}
            onChange={(e) => setVerbose(e.target.checked)}
            className="accent-amber-400"
          />
          <span>verbose{hiddenCount > 0 && !verbose ? ` (${hiddenCount})` : ''}</span>
        </label>
      </div>

      <div
        ref={scrollRef}
        onScroll={onScroll}
        className={`${heightClass ?? 'min-h-[16rem] flex-1'} overflow-y-auto bg-slate-950/60 font-mono text-xs`}
        role="log"
        aria-live="polite"
      >
        {visible.length === 0 ? (
          <div className="flex h-40 items-center justify-center gap-2 text-slate-500">
            {loading || liveWaiting ? (
              <>
                <Loader2 size={14} className="animate-spin" />
                <span>{liveWaiting ? 'Waiting for log output…' : 'Loading log…'}</span>
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
              <TranscriptRow key={`${keyPrefix}-${originalIndex}`} entry={entry} />
            ))}
          </div>
        )}
      </div>

      {jumpToBottom && !atBottom && (
        <button
          type="button"
          onClick={scrollToBottom}
          data-testid="log-jump-to-bottom"
          className="absolute bottom-3 right-3 inline-flex items-center gap-1 rounded-full border border-slate-700 bg-slate-800/90 px-3 py-1 text-xs text-slate-200 shadow-lg backdrop-blur transition-colors hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
        >
          <ArrowDown size={12} aria-hidden />
          Jump to bottom
        </button>
      )}
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
      return <HiddenRow entry={entry} />
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

  const { preview, totalLines } = useMemo(() => {
    if (!entry.result) return { preview: '', totalLines: 0 }
    let lineCount = 1
    let previewEnd = -1
    for (let i = 0; i < entry.result.length; i++) {
      if (entry.result[i] === '\n') {
        lineCount++
        if (lineCount === RESULT_PREVIEW_LINES + 1) {
          previewEnd = i
        }
      }
    }
    return {
      preview: previewEnd === -1 ? entry.result : entry.result.slice(0, previewEnd),
      totalLines: lineCount,
    }
  }, [entry.result])
  const truncated = totalLines > RESULT_PREVIEW_LINES
  const hiddenLines = totalLines - RESULT_PREVIEW_LINES
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

// HiddenRow renders one classified-noise record, only reachable with verbose
// on. The raw content is clamped behind a show-more expander so a run's worth
// of signature-only thinking blocks doesn't bury the events worth reading.
function HiddenRow({ entry }: { entry: HiddenEntry }) {
  const [expanded, setExpanded] = useState(false)
  const truncated = entry.content.length > HIDDEN_PREVIEW_CHARS

  return (
    <div className="flex gap-2 text-slate-600">
      <span className="select-none" aria-hidden>
        ·
      </span>
      <div className="min-w-0">
        <span className="uppercase tracking-wide">{entry.label}</span>
        {entry.content && (
          <pre className="mt-0.5 whitespace-pre-wrap break-all text-slate-600">
            {expanded || !truncated
              ? entry.content
              : `${entry.content.slice(0, HIDDEN_PREVIEW_CHARS)}…`}
          </pre>
        )}
        {truncated && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="mt-0.5 text-[11px] text-slate-500 underline decoration-dotted hover:text-slate-300"
          >
            {expanded ? 'show less' : 'show more'}
          </button>
        )}
      </div>
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
        <span className="whitespace-pre-wrap break-words">{expanded ? text : firstLine}</span>
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
  const tokParts: string[] = []
  if (entry.inputTokens != null) tokParts.push(`${entry.inputTokens} in`)
  if (entry.outputTokens != null) tokParts.push(`${entry.outputTokens} out`)
  if (tokParts.length) parts.push(`${tokParts.join(' / ')} tok`)
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
          code: ({ children }) => (
            <code className="rounded bg-slate-800 px-1 py-0.5 text-amber-200">{children}</code>
          ),
          img: () => null,
          pre: ({ children }) => (
            <pre className="overflow-x-auto rounded bg-slate-900/80 p-2 text-slate-200 [&>code]:bg-transparent [&>code]:p-0 [&>code]:text-inherit">
              {children}
            </pre>
          ),
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
  const totalSec = Math.round(seconds)
  const minutes = Math.floor(totalSec / 60)
  const rem = totalSec % 60
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
