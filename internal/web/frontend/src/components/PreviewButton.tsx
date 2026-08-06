import { ExternalLink, Play, RotateCcw, Square } from 'lucide-react'
import { usePreview } from '../hooks/usePreview'
import { useToast } from '../hooks/useToast'
import PreviewStatusChip from './PreviewStatusChip'

export interface PreviewButtonProps {
  beadId: string
  /** Owning anvil. Required — the daemon reads the manifest from its checkout. */
  anvil?: string
  /**
   * Whether the bead still has something to preview: a surviving forge branch
   * or an open PR. A preview is a detached checkout of that branch, so without
   * one there is nothing to build.
   */
  hasBranch?: boolean
  /** Compact rendering for dense surfaces (PR rows, worker cards). */
  compact?: boolean
  className?: string
}

// PreviewButton is the per-bead Kiln preview trigger: start, live status chip,
// an "Open" link once the entry service is up, and stop.
//
// It renders nothing at all unless a preview is actually possible — Kiln is
// enabled, the bead's anvil declares a `.forge/preview.yaml`, and the bead has
// a surviving branch/PR. That gate is client-side on purpose: the anvil list
// rides along with the previews snapshot every consumer already polls, so a
// page of PR rows costs no extra requests to decide what to show.
export default function PreviewButton({
  beadId,
  anvil,
  hasBranch = true,
  compact = false,
  className = '',
}: PreviewButtonProps) {
  const toast = useToast()
  const { available, status, preview, previewUrl, error, isBusy, start, stop } = usePreview(beadId, {
    anvil,
    onError: (message) => toast.push(message, 'error'),
    onQueued: (kind) =>
      toast.push(
        kind === 'start' ? `Starting preview for ${beadId}…` : `Stopping preview for ${beadId}…`,
        'info',
      ),
  })

  if (!available || !hasBranch) return null

  // The trigger only makes sense while the daemon holds no environment for this
  // bead: starting one that already exists returns the existing one, so a
  // failed *record* is cleared with Stop before it can be retried.
  const showTrigger = !preview && (status === 'idle' || status === 'failed')
  const label = status === 'failed' ? 'Retry preview' : 'Preview'
  const TriggerIcon = status === 'failed' ? RotateCcw : Play

  return (
    <span
      data-testid={`preview-controls-${beadId}`}
      className={`inline-flex flex-wrap items-center gap-1.5 ${className}`}
    >
      {showTrigger && (
        <button
          type="button"
          onClick={() => void start()}
          disabled={isBusy}
          data-testid={`preview-start-${beadId}`}
          title={
            status === 'failed' && error
              ? `Previous attempt failed: ${error}`
              : 'Start a preview environment for this branch'
          }
          aria-label={`${label} for ${beadId}`}
          className="inline-flex items-center gap-1 rounded-md border border-sky-600/30 bg-sky-600/15 px-2 py-1 text-xs font-medium text-sky-200 transition-colors hover:bg-sky-600/25 focus:outline-none focus-visible:ring-1 focus-visible:ring-sky-300 disabled:cursor-not-allowed disabled:opacity-50"
        >
          <TriggerIcon size={13} aria-hidden />
          <span className={compact ? 'hidden sm:inline' : undefined}>{label}</span>
        </button>
      )}

      {status !== 'idle' && (
        <PreviewStatusChip status={status} error={error} testId={`preview-status-${beadId}`} />
      )}

      {previewUrl && (status === 'healthy' || status === 'degraded') && (
        <a
          href={previewUrl}
          target="_blank"
          rel="noreferrer"
          data-testid={`preview-open-${beadId}`}
          title={`Open ${previewUrl}`}
          className="inline-flex items-center gap-1 rounded-md border border-emerald-600/30 bg-emerald-600/15 px-2 py-1 text-xs font-medium text-emerald-200 transition-colors hover:bg-emerald-600/25 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-300"
        >
          <ExternalLink size={13} aria-hidden />
          <span className={compact ? 'hidden sm:inline' : undefined}>Open</span>
        </a>
      )}

      {preview && status !== 'stopping' && (
        <button
          type="button"
          onClick={() => void stop()}
          disabled={isBusy}
          data-testid={`preview-stop-${beadId}`}
          title="Stop this preview and release its ports"
          aria-label={`Stop preview for ${beadId}`}
          className="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-xs font-medium text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700 focus:outline-none focus-visible:ring-1 focus-visible:ring-slate-300 disabled:cursor-not-allowed disabled:opacity-50"
        >
          <Square size={13} aria-hidden />
          <span className={compact ? 'hidden sm:inline' : undefined}>Stop</span>
        </button>
      )}
    </span>
  )
}
