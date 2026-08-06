// Typed client for the Kiln preview endpoints. Kept in its own module (rather
// than in api.ts) because the whole preview surface — trigger button, status
// chip, bead-detail panel, previews overview — reads from these four calls, and
// the wire shapes mirror internal/web/preview_handlers.go field for field.

import { ApiError, type QueuedBody } from '../api'

// PreviewServiceHealth mirrors the per-service `health` column
// (state.PreviewService*). Left open (`| string`) so an unexpected backend
// value renders instead of failing to type-check.
export type PreviewServiceHealth = 'starting' | 'healthy' | 'failed' | (string & {})

// PreviewRecordStatus mirrors the preview-level `status` column
// (state.Preview*). It is the *backend* vocabulary; the UI-facing vocabulary is
// PreviewStatus below, which adds the client-only transitions.
export type PreviewRecordStatus =
  | 'starting'
  | 'running'
  | 'degraded'
  | 'failed'
  | 'stopped'
  | (string & {})

// PreviewServiceStatus mirrors Go's web.PreviewServiceStatus.
export interface PreviewServiceStatus {
  name: string
  port: number
  health: PreviewServiceHealth
  entry: boolean
  uptime_seconds: number
  /** GET endpoint tailing this service's log. */
  log_url: string
  /** Explains a failed service (spawn error, health timeout, early exit). */
  error?: string
}

// PreviewSummary mirrors Go's web.PreviewSummary — one live preview.
export interface PreviewSummary {
  bead_id: string
  anvil?: string
  branch?: string
  status: PreviewRecordStatus
  services: PreviewServiceStatus[]
  /** The link an operator opens; '' when no service has a port yet. */
  entry_url: string
  created_at: string
  last_active_at: string
  /** When the idle reaper tears this down; null when the reaper is disabled. */
  idle_deadline: string | null
}

// PreviewsListResponse is the body of GET /api/previews. `enabled` false means
// the daemon runs without a Kiln manager — "previews are disabled", not "none
// are running". `anvils` names the anvils a preview can be started for, and is
// what gates the Preview affordance per row.
export interface PreviewsListResponse {
  enabled: boolean
  anvils: string[]
  previews: PreviewSummary[]
}

// PreviewStatus is the UI-facing state of one bead's preview. It folds the
// backend's record statuses into what the chip renders, and adds the two
// client-only transitions (`starting` before the daemon has written a row,
// `stopping` while a teardown is in flight).
export type PreviewStatus =
  | 'idle'
  | 'starting'
  | 'healthy'
  | 'degraded'
  | 'failed'
  | 'stopping'

// mapPreviewStatus folds a backend record status into the UI vocabulary. A
// stopped record reads as `idle`: the row lingers only until the manager drops
// it, and nothing about it is actionable. An unrecognised value is treated as
// `starting` — the honest reading of "the daemon knows about this preview but
// this client does not know that state yet" is in-progress, never healthy.
export function mapPreviewStatus(status: PreviewRecordStatus | undefined): PreviewStatus {
  switch (status) {
    case 'running':
      return 'healthy'
    case 'degraded':
      return 'degraded'
    case 'failed':
      return 'failed'
    case 'stopped':
    case undefined:
      return 'idle'
    case 'starting':
      return 'starting'
    default:
      return 'starting'
  }
}

// previewErrorText returns the first failed service's error for a preview, or
// null when nothing failed. Used to explain a failed chip without making the
// caller walk the service list.
export function previewErrorText(preview: PreviewSummary | null | undefined): string | null {
  if (!preview) return null
  for (const svc of preview.services ?? []) {
    if (svc.health === 'failed' && svc.error) return svc.error
  }
  return null
}

// fetchPreviews loads every live preview plus the enablement/gating metadata.
export async function fetchPreviews(signal?: AbortSignal): Promise<PreviewsListResponse> {
  const res = await fetch('/api/previews', { credentials: 'include', signal })
  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { error?: string }
    throw new ApiError(res.status, body.error ?? `HTTP ${res.status}`)
  }
  const body = (await res.json()) as Partial<PreviewsListResponse>
  return {
    enabled: body.enabled === true,
    anvils: body.anvils ?? [],
    previews: body.previews ?? [],
  }
}

// postPreviewAction POSTs a preview start/stop and returns the daemon's 202
// body *without* resolving it.
//
// It deliberately does not go through apiPost: that helper polls the queued
// request to a terminal state before returning, on a 15s budget sized for
// sub-second bd shell-outs. A preview start runs a setup script, spawns every
// service and waits out their health checks — minutes, not seconds — so
// blocking on it would leave the button spinning and then report the outcome as
// unknown. The caller (usePreview) keeps the returned poll_url and resolves it
// on its own clock alongside the previews list.
async function postPreviewAction(path: string, body: unknown): Promise<QueuedBody> {
  const res = await fetch(path, {
    method: 'POST',
    credentials: 'include',
    headers: { 'X-Forge-Action': '1', 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  const text = await res.text()
  let parsed: unknown = null
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = null
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, (parsed as { error?: string })?.error ?? `HTTP ${res.status}`)
  }
  return (parsed ?? {}) as QueuedBody
}

// startPreview asks the daemon to bring a bead's preview up. The anvil is
// required: the daemon reads the manifest from that anvil's main checkout.
export function startPreview(beadID: string, anvil: string): Promise<QueuedBody> {
  return postPreviewAction(`/api/bead/${encodeURIComponent(beadID)}/preview/start`, { anvil })
}

// stopPreview tears a bead's preview down. The anvil is optional — the bead id
// alone identifies the preview in the manager's registry — but is forwarded
// when known to match the action-request convention.
export function stopPreview(beadID: string, anvil?: string): Promise<QueuedBody> {
  return postPreviewAction(`/api/bead/${encodeURIComponent(beadID)}/preview/stop`, {
    anvil: anvil ?? '',
  })
}

// pollURLFor resolves the request-status endpoint for a queued response: the
// daemon's own poll_url when it sent one, else the canonical path for the
// request id. Returns null when the daemon answered without a request id
// (a synchronous 200), which means there is nothing to poll.
export function pollURLFor(queued: QueuedBody | null | undefined): string | null {
  if (!queued) return null
  if (queued.poll_url) return queued.poll_url
  if (queued.request_id) return `/api/requests/${encodeURIComponent(queued.request_id)}`
  return null
}
