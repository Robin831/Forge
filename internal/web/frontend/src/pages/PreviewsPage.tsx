import { ExternalLink, MonitorPlay, ScrollText, Square, Timer } from 'lucide-react'
import { useState } from 'react'
import { Link } from 'react-router'
import type { StatusResponse } from '../api'
import type { PreviewSummary } from '../api/previews'
import { mapPreviewStatus } from '../api/previews'
import AdHocPreviewForm from '../components/AdHocPreviewForm'
import AppHeader from '../components/AppHeader'
import Pane, { EmptyState } from '../components/Pane'
import PreviewLogModal, { type PreviewLogTarget } from '../components/PreviewLogModal'
import PreviewServiceHealthBadge from '../components/PreviewServiceHealth'
import PreviewStatusChip from '../components/PreviewStatusChip'
import { useApiPoll } from '../hooks/useApiPoll'
import { useNow } from '../hooks/useNow'
import { usePreview, usePreviewsList } from '../hooks/usePreview'
import { useToast } from '../hooks/useToast'
import { relativeTime } from '../lib/format'
import { formatDuration, previewIdleCountdown } from '../lib/previewFormat'

const POLL_INTERVAL_MS = 5000

// PreviewsPage is the fleet view of Kiln: every live preview across every
// anvil, with the idle countdown that says which one the reaper takes next.
//
// It holds no state of its own — the previews come from the same shared store
// the per-bead button and panel subscribe to, so opening this page alongside a
// dashboard costs no extra polling.
export default function PreviewsPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const snap = usePreviewsList()
  const now = useNow(1000)
  const [logTarget, setLogTarget] = useState<PreviewLogTarget | null>(null)

  return (
    <div className="flex min-h-full flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <main className="flex flex-col gap-6">
        {/*
          The ad-hoc starter sits above the fleet, and only once the snapshot has
          arrived saying Kiln is on: a form offering to start something the
          daemon has switched off would be a worse answer than the disabled
          message the pane below already gives.
        */}
        {snap.loaded && snap.enabled && <AdHocPreviewForm anvils={snap.anvils} />}

        <Pane
          title="Preview environments"
          icon={<MonitorPlay size={16} className="text-sky-400" aria-hidden />}
          count={snap.previews.length}
          loading={!snap.loaded}
          error={snap.error}
        >
          {!snap.loaded ? (
            <EmptyState message="Loading previews…" />
          ) : !snap.enabled ? (
            <EmptyState message="Previews are disabled. Set preview_enabled in forge.yaml and give an anvil a .forge/preview.yaml manifest to use them." />
          ) : snap.previews.length === 0 ? (
            <EmptyState message="No preview environments are running. Start one from the form above, or from a bead, a worker card or a PR row." />
          ) : (
            <ul className="divide-y divide-slate-800">
              {snap.previews.map((p) => (
                <PreviewRow
                  key={p.bead_id}
                  preview={p}
                  now={now}
                  fetchedAt={snap.fetchedAt}
                  onViewLog={(service, logUrl) =>
                    setLogTarget({ beadId: p.bead_id, service, logUrl })
                  }
                />
              ))}
            </ul>
          )}
        </Pane>
      </main>

      <PreviewLogModal target={logTarget} onClose={() => setLogTarget(null)} />

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}

interface PreviewRowProps {
  preview: PreviewSummary
  now: number
  /** When the snapshot was fetched; the idle countdown ages from it. */
  fetchedAt: number
  onViewLog: (service: string, logUrl: string) => void
}

// PreviewRow renders one live preview. It runs the shared per-bead hook so its
// Stop button behaves exactly like the one on the bead page — including the
// stopping chip while the teardown is in flight.
function PreviewRow({ preview, now, fetchedAt, onViewLog }: PreviewRowProps) {
  const toast = useToast()
  const { status, isBusy, stop } = usePreview(preview.bead_id, {
    anvil: preview.anvil,
    onError: (message) => toast.push(message, 'error'),
    onQueued: () => toast.push(`Stopping preview for ${preview.bead_id}…`, 'info'),
  })

  // The hook folds in whatever is in flight locally; fall back to the row's own
  // record so a preview still renders if the hook has not caught up.
  const shown = status === 'idle' ? mapPreviewStatus(preview.status) : status
  const countdown = previewIdleCountdown(preview, fetchedAt, now)
  const beadHref = `/bead/${encodeURIComponent(preview.bead_id)}${
    preview.anvil ? `?anvil=${encodeURIComponent(preview.anvil)}` : ''
  }`

  return (
    <li className="px-4 py-3" data-testid={`preview-row-${preview.bead_id}`}>
      <div className="flex flex-wrap items-center gap-2">
        <Link
          to={beadHref}
          className="font-mono text-sm text-slate-200 hover:text-amber-300 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
        >
          {preview.bead_id}
        </Link>
        <PreviewStatusChip status={shown} testId={`preview-row-status-${preview.bead_id}`} />

        <div className="ml-auto flex flex-wrap items-center gap-2">
          {countdown && (
            <span
              data-testid={`preview-row-countdown-${preview.bead_id}`}
              title={preview.idle_deadline ?? undefined}
              className="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800/60 px-1.5 py-0.5 text-[11px] text-slate-300"
            >
              <Timer size={11} aria-hidden />
              idles in {countdown}
            </span>
          )}
          {preview.entry_url && (
            <a
              href={preview.entry_url}
              target="_blank"
              rel="noreferrer"
              data-testid={`preview-row-open-${preview.bead_id}`}
              title={`Open ${preview.entry_url}`}
              className="inline-flex items-center gap-1 rounded-md border border-emerald-600/30 bg-emerald-600/15 px-2 py-1 text-xs font-medium text-emerald-200 transition-colors hover:bg-emerald-600/25 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-300"
            >
              <ExternalLink size={12} aria-hidden />
              Open
            </a>
          )}
          {shown !== 'stopping' && (
            <button
              type="button"
              onClick={() => void stop()}
              disabled={isBusy}
              data-testid={`preview-row-stop-${preview.bead_id}`}
              aria-label={`Stop preview for ${preview.bead_id}`}
              title="Stop this preview and release its ports"
              className="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-xs font-medium text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700 focus:outline-none focus-visible:ring-1 focus-visible:ring-slate-300 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <Square size={12} aria-hidden />
              Stop
            </button>
          )}
        </div>
      </div>

      <p className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
        {preview.anvil && <span>{preview.anvil}</span>}
        {preview.branch && (
          <>
            <span aria-hidden>·</span>
            <span className="font-mono">{preview.branch}</span>
          </>
        )}
        {preview.created_at && (
          <>
            <span aria-hidden>·</span>
            <span title={preview.created_at}>started {relativeTime(preview.created_at)}</span>
          </>
        )}
        {preview.resource_note && (
          <>
            <span aria-hidden>·</span>
            <span data-testid={`preview-row-resource-note-${preview.bead_id}`}>
              {preview.resource_note}
            </span>
          </>
        )}
      </p>

      <ul className="mt-2 flex flex-wrap items-center gap-1.5">
        {(preview.services ?? []).map((svc) => (
          <li
            key={svc.name}
            data-testid={`preview-row-service-${preview.bead_id}-${svc.name}`}
            className="inline-flex items-center gap-1.5 rounded-md border border-slate-800 bg-slate-950/40 px-2 py-1 text-xs"
          >
            <span className="font-mono text-slate-300">
              {svc.name}
              {svc.port > 0 ? `:${svc.port}` : ''}
            </span>
            <PreviewServiceHealthBadge health={svc.health} error={svc.error} />
            <span className="text-slate-500">
              {svc.health === 'failed' ? '—' : formatDuration(svc.uptime_seconds)}
            </span>
            {svc.log_url && (
              <button
                type="button"
                onClick={() => onViewLog(svc.name, svc.log_url)}
                data-testid={`preview-row-log-${preview.bead_id}-${svc.name}`}
                aria-label={`View ${svc.name} log for ${preview.bead_id}`}
                className="inline-flex items-center gap-1 text-slate-400 transition-colors hover:text-amber-300 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
              >
                <ScrollText size={12} aria-hidden />
                log
              </button>
            )}
          </li>
        ))}
      </ul>
    </li>
  )
}
