import { useEffect, useMemo, useState } from 'react'
import type { BeadLogFile, BeadLogsResponse, LogLine } from '../api'
import { ApiError, beadLogs } from '../api'
import { useAuth } from '../auth'
import { useApiPoll } from '../hooks/useApiPoll'
import { useEventSource } from '../hooks/useEventSource'
import LogViewer from './LogViewer'
import { EmptyState } from './Pane'
import { relativeTime } from '../lib/format'

const LIST_POLL_MS = 8000
const TAIL_LINES = 500

const STAGE_CLASSES: Record<string, string> = {
  smith: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300',
  warden: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
  temper: 'border-amber-500/40 bg-amber-500/10 text-amber-300',
  schematic: 'border-purple-500/40 bg-purple-500/10 text-purple-300',
  quench: 'border-orange-500/40 bg-orange-500/10 text-orange-300',
  burnish: 'border-pink-500/40 bg-pink-500/10 text-pink-300',
  rebase: 'border-red-500/40 bg-red-500/10 text-red-300',
  assay: 'border-teal-500/40 bg-teal-500/10 text-teal-300',
  steer: 'border-indigo-500/40 bg-indigo-500/10 text-indigo-300',
}

function stageClass(stage: string): string {
  return STAGE_CLASSES[stage] ?? 'border-slate-600 bg-slate-700/40 text-slate-300'
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

interface BeadLogsSectionProps {
  beadID: string
}

// BeadLogsSection lists a bead's preserved + live stage log files and renders
// the selected one via the shared LogViewer. Completed files are tail-fetched
// once; the file an active worker is currently writing is streamed live via the
// existing /api/worker/{id}/stream SSE endpoint.
export default function BeadLogsSection({ beadID }: BeadLogsSectionProps) {
  const { logout } = useAuth()
  const listPath = `/api/bead/${encodeURIComponent(beadID)}/logs`
  const list = useApiPoll<BeadLogsResponse>(listPath, LIST_POLL_MS)
  const files = useMemo<BeadLogFile[]>(() => list.data?.files ?? [], [list.data])

  const [selected, setSelected] = useState<string | null>(null)

  // Auto-select the most recent file (last in the ascending list) once, when
  // files first arrive and nothing is selected yet.
  useEffect(() => {
    if (selected === null && files.length > 0) {
      setSelected(files[files.length - 1].filename)
    }
  }, [files, selected])

  const selectedFile = useMemo(
    () => files.find((f) => f.filename === selected) ?? null,
    [files, selected],
  )
  const isLiveSel = !!selectedFile?.live && !!selectedFile?.worker_id

  // Live stream for the file an active worker is currently writing.
  const liveURL = isLiveSel
    ? `/api/worker/${encodeURIComponent(selectedFile!.worker_id!)}/stream`
    : null
  const live = useEventSource<LogLine>(liveURL, { maxItems: 1000 })

  // One-shot tail for completed files. Keyed on the filename + live flag so it
  // does not re-fetch on every list poll while the same file stays selected.
  const [tailLines, setTailLines] = useState<string[] | null>(null)
  const [tailLoading, setTailLoading] = useState(false)
  const [tailError, setTailError] = useState<string | null>(null)
  useEffect(() => {
    if (!selected || isLiveSel) {
      setTailLines(null)
      setTailError(null)
      return
    }
    let cancelled = false
    setTailLoading(true)
    setTailError(null)
    beadLogs
      .tail(beadID, selected, TAIL_LINES)
      .then((resp) => {
        if (!cancelled) setTailLines(resp.lines ?? [])
      })
      .catch((err) => {
        if (cancelled) return
        if (err instanceof ApiError && err.status === 401) {
          void logout()
          return
        }
        setTailError(err instanceof Error ? err.message : 'failed to load log')
      })
      .finally(() => {
        if (!cancelled) setTailLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [beadID, selected, isLiveSel, logout])

  const rawLines = useMemo<string[]>(() => {
    if (!selectedFile) return []
    if (isLiveSel) return live.items.map((l) => l.line)
    return tailLines ?? []
  }, [selectedFile, isLiveSel, live.items, tailLines])

  if (files.length === 0) {
    return (
      <EmptyState
        message={list.loading ? 'Loading logs…' : 'No stage logs recorded for this bead yet.'}
      />
    )
  }

  const liveStatusText =
    live.status === 'open'
      ? 'live'
      : live.status === 'connecting'
        ? 'connecting…'
        : live.status === 'error'
          ? 'reconnecting…'
          : 'closed'
  const statusText = selectedFile
    ? isLiveSel
      ? `${selectedFile.filename} · ${liveStatusText}`
      : tailLoading
        ? `${selectedFile.filename} · loading…`
        : `${selectedFile.filename} · last ${TAIL_LINES} lines`
    : undefined

  return (
    <div className="flex flex-col">
      <div
        className="flex flex-wrap gap-1.5 border-b border-slate-800 px-4 py-3"
        role="tablist"
        aria-label="Stage logs"
      >
        {files.map((f) => {
          const active = f.filename === selected
          return (
            <button
              key={f.filename}
              type="button"
              role="tab"
              aria-selected={active}
              onClick={() => setSelected(f.filename)}
              className={`flex items-center gap-1.5 rounded-md border px-2 py-1 text-left text-[11px] transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 ${
                active
                  ? 'border-amber-400/50 bg-amber-500/10'
                  : 'border-slate-800 bg-slate-950/40 hover:border-slate-700 hover:bg-slate-800/40'
              }`}
              title={`${f.filename} · ${formatBytes(f.size_bytes)}`}
            >
              <span
                className={`rounded border px-1 py-0.5 font-semibold uppercase tracking-wide ${stageClass(f.stage)}`}
              >
                {f.stage}
              </span>
              <span className="text-slate-400" title={f.mtime}>
                {relativeTime(f.mtime)}
              </span>
              {f.live && (
                <span className="inline-flex items-center gap-1 text-emerald-300">
                  <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-emerald-400" aria-hidden />
                  live
                </span>
              )}
            </button>
          )
        })}
      </div>

      {tailError && (
        <div className="border-b border-red-700/40 bg-red-900/20 px-4 py-2 text-sm text-red-200">
          {tailError}
        </div>
      )}

      <div className="flex h-[32rem] flex-col">
        {selectedFile ? (
          <LogViewer
            rawLines={rawLines}
            loading={tailLoading}
            liveWaiting={isLiveSel && live.status === 'open'}
            statusText={statusText}
            keyPrefix={selected ?? 'log'}
          />
        ) : (
          <EmptyState message="Select a stage log to view its transcript." />
        )}
      </div>
    </div>
  )
}
