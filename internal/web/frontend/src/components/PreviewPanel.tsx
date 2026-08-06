import { useState } from 'react'
import { ExternalLink, MonitorPlay, Play, RotateCcw, ScrollText, Square } from 'lucide-react'
import { usePreview } from '../hooks/usePreview'
import { useNow } from '../hooks/useNow'
import { useToast } from '../hooks/useToast'
import { formatCountdown, formatDuration } from '../lib/previewFormat'
import { relativeTime } from '../lib/format'
import { EmptyState } from './Pane'
import PreviewLogModal, { type PreviewLogTarget } from './PreviewLogModal'
import PreviewServiceHealthBadge from './PreviewServiceHealth'
import PreviewStatusChip from './PreviewStatusChip'

export interface PreviewPanelProps {
  beadId: string
  /** Owning anvil. Required — the daemon reads the manifest from its checkout. */
  anvil?: string
  /**
   * Whether the bead still has something to preview: a surviving forge branch
   * or an open PR. A preview is a detached checkout of that branch, so without
   * one there is nothing to build.
   */
  hasBranch?: boolean
}

// PreviewPanel is the full preview surface for one bead: the per-service table
// (port, health, uptime, log), the entry link, the idle countdown and the
// start/stop controls.
//
// It is the bead-detail counterpart to PreviewButton — same state machine, same
// chip, more detail. The button is what dense surfaces (PR rows, worker cards)
// mount; this is what you open when a preview is misbehaving and you need to
// know *which* service failed and why.
export default function PreviewPanel({ beadId, anvil, hasBranch = true }: PreviewPanelProps) {
  const toast = useToast()
  const [logTarget, setLogTarget] = useState<PreviewLogTarget | null>(null)
  const now = useNow(1000)

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

  // Starting a bead that already has an environment returns the existing one,
  // so a failed *record* is cleared with Stop before a retry means anything.
  const showTrigger = !preview && (status === 'idle' || status === 'failed')
  const triggerLabel = status === 'failed' ? 'Retry preview' : 'Start preview'
  const TriggerIcon = status === 'failed' ? RotateCcw : Play
  const canOpen = !!previewUrl && (status === 'healthy' || status === 'degraded')
  const countdown = formatCountdown(preview?.idle_deadline, now)
  const services = preview?.services ?? []

  return (
    <section
      aria-label="Preview environment"
      data-testid={`preview-panel-${beadId}`}
      className="overflow-hidden rounded-xl border border-slate-800 bg-slate-900/60"
    >
      <header className="flex flex-wrap items-center gap-2 border-b border-slate-800 px-4 py-3">
        <MonitorPlay size={16} className="text-sky-400" aria-hidden />
        <h3 className="text-sm font-semibold text-slate-200">Preview environment</h3>
        <PreviewStatusChip status={status} error={error} testId={`preview-status-${beadId}`} />

        <div className="ml-auto flex flex-wrap items-center gap-2">
          {showTrigger && (
            <button
              type="button"
              onClick={() => void start()}
              disabled={isBusy}
              data-testid="preview-panel-start"
              aria-label={`${triggerLabel} for ${beadId}`}
              title={
                status === 'failed' && error
                  ? `Previous attempt failed: ${error}`
                  : 'Start a preview environment for this branch'
              }
              className="inline-flex items-center gap-1.5 rounded-md border border-sky-600/30 bg-sky-600/15 px-2.5 py-1.5 text-xs font-medium text-sky-200 transition-colors hover:bg-sky-600/25 focus:outline-none focus-visible:ring-1 focus-visible:ring-sky-300 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <TriggerIcon size={13} aria-hidden />
              {triggerLabel}
            </button>
          )}

          {canOpen && (
            <a
              href={previewUrl!}
              target="_blank"
              rel="noreferrer"
              data-testid="preview-panel-open"
              title={`Open ${previewUrl}`}
              className="inline-flex items-center gap-1.5 rounded-md border border-emerald-600/30 bg-emerald-600/15 px-2.5 py-1.5 text-xs font-medium text-emerald-200 transition-colors hover:bg-emerald-600/25 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-300"
            >
              <ExternalLink size={13} aria-hidden />
              Open preview
            </a>
          )}

          {preview && status !== 'stopping' && (
            <button
              type="button"
              onClick={() => void stop()}
              disabled={isBusy}
              data-testid="preview-panel-stop"
              aria-label={`Stop preview for ${beadId}`}
              title="Stop this preview and release its ports"
              className="inline-flex items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800/60 px-2.5 py-1.5 text-xs font-medium text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700 focus:outline-none focus-visible:ring-1 focus-visible:ring-slate-300 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <Square size={13} aria-hidden />
              Stop
            </button>
          )}
        </div>
      </header>

      {preview && (
        <p
          data-testid="preview-panel-meta"
          className="flex flex-wrap items-center gap-x-2 gap-y-0.5 border-b border-slate-800/60 px-4 py-2 text-xs text-slate-500"
        >
          {preview.branch && <span className="font-mono text-slate-400">{preview.branch}</span>}
          {preview.created_at && (
            <>
              <span aria-hidden>·</span>
              <span title={preview.created_at}>started {relativeTime(preview.created_at)}</span>
            </>
          )}
          {countdown && (
            <>
              <span aria-hidden>·</span>
              <span data-testid="preview-panel-countdown" title={preview.idle_deadline ?? undefined}>
                idles in {countdown}
              </span>
            </>
          )}
        </p>
      )}

      {/* Only when there is no record to explain it. Once a preview row exists
          its failures are attributed per service in the table below, and
          repeating the first one here would just read as a second, vaguer
          failure. */}
      {error && !preview && (
        <p
          data-testid="preview-panel-error"
          className="border-b border-red-700/40 bg-red-900/20 px-4 py-2 text-xs text-red-200"
        >
          {error}
        </p>
      )}

      {!preview ? (
        <EmptyState
          message={
            status === 'starting'
              ? 'Bringing the preview up — services appear as they are spawned.'
              : 'No preview environment is running for this bead.'
          }
        />
      ) : services.length === 0 ? (
        <EmptyState message="This preview declares no services." />
      ) : (
        <table className="w-full text-sm">
          <thead className="text-left text-[11px] uppercase tracking-wide text-slate-500">
            <tr>
              <th className="px-4 py-2 font-medium">Service</th>
              <th className="px-4 py-2 font-medium">Port</th>
              <th className="px-4 py-2 font-medium">Health</th>
              <th className="px-4 py-2 font-medium">Uptime</th>
              <th className="px-4 py-2 font-medium">
                <span className="sr-only">Log</span>
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-800">
            {services.map((svc) => (
              <tr key={svc.name} data-testid={`preview-service-${svc.name}`}>
                <td className="px-4 py-2">
                  <span className="font-mono text-slate-200">{svc.name}</span>
                  {svc.entry && (
                    <span className="ml-1.5 rounded border border-slate-700 bg-slate-800/60 px-1 py-0.5 text-[9px] uppercase tracking-wide text-slate-400">
                      entry
                    </span>
                  )}
                </td>
                <td className="px-4 py-2 font-mono text-slate-300">
                  {svc.port > 0 ? svc.port : '—'}
                </td>
                <td className="px-4 py-2">
                  <PreviewServiceHealthBadge
                    health={svc.health}
                    error={svc.error}
                    testId={`preview-service-health-${svc.name}`}
                  />
                </td>
                <td className="px-4 py-2 text-slate-400">
                  {svc.health === 'failed' ? '—' : formatDuration(svc.uptime_seconds)}
                </td>
                <td className="px-4 py-2 text-right">
                  {svc.log_url && (
                    <button
                      type="button"
                      onClick={() =>
                        setLogTarget({ beadId, service: svc.name, logUrl: svc.log_url })
                      }
                      data-testid={`preview-service-log-${svc.name}`}
                      className="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-xs text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700 focus:outline-none focus-visible:ring-1 focus-visible:ring-slate-300"
                    >
                      <ScrollText size={12} aria-hidden />
                      Log
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {services.some((svc) => svc.health === 'failed' && svc.error) && (
        <ul className="border-t border-slate-800/60 px-4 py-2 text-xs text-red-200">
          {services
            .filter((svc) => svc.health === 'failed' && svc.error)
            .map((svc) => (
              <li key={svc.name}>
                <span className="font-mono">{svc.name}</span>: {svc.error}
              </li>
            ))}
        </ul>
      )}

      <PreviewLogModal target={logTarget} onClose={() => setLogTarget(null)} />
    </section>
  )
}
